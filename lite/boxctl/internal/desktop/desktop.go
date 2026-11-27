// Package desktop 是 Banbu Connect Lite 的本机后台进程：绑定管理服务，管理托管的 AI 账号
// （Claude / Codex / Gemini）、工作区多开、额度、Claude Code 遥测、文件下载和应用更新。
// 不做代理：没有内核、TUN、Kill Switch 和受信网络，所以不需要管理员权限。
//
// 进程以当前用户身份运行，由托盘外壳（client/app，Tauri）拉起；外壳退出时关闭标准输入，进程随之退出。
package desktop

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"boxctl/internal/buildinfo"
	"boxctl/internal/dist"
	"boxctl/internal/otel"
	"boxctl/internal/webui"
)

// DefaultAddr 是后台进程的固定地址，Tauri 外壳也用它。和完整版（17930）分开，两者可以同时运行。
const DefaultAddr = "127.0.0.1:17940"

// Options 是 `boxctl desktop run` 的参数。
type Options struct {
	Addr    string
	DataDir string
	// ExitOnStdinClose 为真时，标准输入关闭（外壳退出）后进程退出。外壳拉起时带上。
	ExitOnStdinClose bool
}

// Main 处理 `boxctl desktop run`。
func Main(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("用法：boxctl desktop run [-addr 地址] [-data 数据目录] [-exit-on-stdin-close]")
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	opts := Options{}
	fs.StringVar(&opts.Addr, "addr", DefaultAddr, "监听地址")
	fs.StringVar(&opts.DataDir, "data", dataDir(), "数据目录")
	fs.BoolVar(&opts.ExitOnStdinClose, "exit-on-stdin-close", false, "标准输入关闭时退出（由外壳拉起时使用）")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.ExitOnStdinClose {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		go func() {
			_, _ = io.Copy(io.Discard, bufio.NewReader(os.Stdin))
			cancel()
		}()
	}
	return Run(ctx, opts)
}

// Run 运行后台进程，直到 ctx 结束。
func Run(ctx context.Context, opts Options) error {
	if host, _, err := net.SplitHostPort(opts.Addr); err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("监听地址必须是本机回环地址：%s", opts.Addr)
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return err
	}
	logger, closeLog := openLog(opts.DataDir)
	defer closeLog()
	// 托盘的「完全退出」经接口让进程自己退出。
	ctx, quit := context.WithCancel(ctx)
	defer quit()

	// 先占端口：同一用户已经有一个在运行时直接报错退出，外壳会连那一个。
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败（可能已经有一个在运行）：%w", opts.Addr, err)
	}

	token, err := ensureSecretFile(filepath.Join(opts.DataDir, tokenFileName))
	if err != nil {
		_ = ln.Close()
		return err
	}

	distClient, err := dist.NewClient(opts.DataDir, buildinfo.Version, logger)
	if err != nil {
		_ = ln.Close()
		return err
	}

	// Claude Code 遥测：本机接收器默认开启，托盘外壳据状态里的 otel.env 自动配置 Claude Code；数据上报到管理服务。
	telemetry, err := otel.Open(opts.DataDir, logger)
	if err != nil {
		_ = ln.Close()
		return err
	}
	telemetry.SetUploader(distClient.ReportOTel)

	events := newEventLog()
	reportUpdate := func(level, msg string, fields map[string]string) {
		logger.Warn(msg, "fields", fields)
		events.record(level, msg)
	}
	appUpdate := &appUpdater{client: distClient, report: reportUpdate,
		notify: func(title, body string) { events.announce("app_update", title, body) }}

	// 账号操作不再检查代理保护状态：Lite 版不做代理，请求直接发出（走系统网络）。
	quota := newAIQuota(directRoute)
	ws := newWorkspaces(quota)
	// 管理服务托管的 Claude / Codex 凭据：写进工作区、读回上传、鉴权测试、上报额度（ADR-061）。
	creds := newAICreds(distClient, ws, quota, opts.DataDir, logger, func(title, body string) { events.announce("ai_refresh", title, body) })
	downloads := &fileJobs{} // 下载页（S1.11）：排队给外壳执行

	mux := http.NewServeMux()
	authed := func(fn http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authorized(r, token) {
				writeJSON(w, http.StatusUnauthorized, apiErr{"unauthenticated", "令牌无效，请从桌面客户端打开"})
				return
			}
			fn(w, r)
		})
	}
	mux.HandleFunc("GET /api/v1/auth/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": authorized(r, token), "desktop": true, "lite": true})
	})
	mux.Handle("GET /api/v1/desktop/status", authed(func(w http.ResponseWriter, _ *http.Request) {
		sum := Summary{Version: buildinfo.Version, Server: distClient.Status(), Notices: events.notices()}
		sum.ClaudeLogin = distClient.PendingClaude()
		sum.OpenLink = distClient.PendingOpen()
		sum.Gemini = distClient.GeminiBrief()
		env, force := telemetry.Brief()
		sum.OTel = &OTelBrief{Enabled: len(env) > 0, Env: env, Force: force, SyncRequest: telemetry.SyncRequest()}
		sum.AppUpdate = appUpdate.Pending()
		sum.WorkspaceAction = ws.Pending()
		sum.FileAction = downloads.Pending()
		writeJSON(w, http.StatusOK, sum)
	}))
	mux.Handle("GET /api/v1/desktop/events", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, events.list())
	}))
	mux.Handle("POST /api/v1/desktop/shutdown", authed(func(w http.ResponseWriter, _ *http.Request) {
		logger.Info("收到完全退出请求，后台进程停止")
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
		go func() {
			time.Sleep(300 * time.Millisecond) // 先让响应发出去
			quit()
		}()
	}))
	serverRoutes(mux, authed, distClient)
	updateRoutes(mux, authed, distClient, reportUpdate, appUpdate.SetAppVersion)
	appUpdate.routes(mux, authed)
	claudeRoutes(mux, authed, distClient, ws.Check)
	workspaceRoutes(mux, authed, ws, distClient)
	fileRoutes(mux, authed, distClient, downloads)
	egressRoutes(mux, authed, opts.DataDir)
	geminiRoutes(mux, authed, distClient)
	telemetry.Routes(mux, authed, writeJSON)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, apiErr{"not_found", "unknown api path"})
	})
	mux.Handle("/", spaHandler(webui.FS()))

	go distClient.Run(ctx, func() {})
	go distClient.RunGemini(ctx)
	go distClient.RunAI(ctx)
	go creds.Run(ctx)
	go runAIOnline(ctx, distClient, ws, logger) // AI 账号在线时长（按切换时间算）
	go quota.Run(ctx)
	go appUpdate.Run(ctx)
	go distClient.RunGeminiQuota(ctx)
	go telemetry.Run(ctx)

	logger.Info("lite service started", "addr", opts.Addr, "version", buildinfo.Version)
	srv := &http.Server{Handler: requireLocalHost(securityHeaders(sameOrigin(mux))), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// directRoute 是额度查询、刷新令牌、鉴权测试的出口：Lite 版不经代理，总是直连。
func directRoute(string) (string, error) { return "", nil }

func authorized(r *http.Request, token string) bool {
	want := []byte("Bearer " + token)
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1
}

// requireLocalHost 校验 Host，防止恶意网页用 DNS 重绑定访问本机端口。
func requireLocalHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
			writeJSON(w, http.StatusForbidden, apiErr{"forbidden_host", "只允许通过本机地址访问"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin 拒绝跨站的写请求。
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					writeJSON(w, http.StatusForbidden, apiErr{"bad_origin", "跨站请求被拒绝"})
					return
				}
			} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeJSON(w, http.StatusForbidden, apiErr{"bad_origin", "跨站请求被拒绝"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// antd 使用 CSS-in-JS 注入 style，需放行 inline style
		h.Set("Content-Security-Policy",
			"default-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// spaHandler 提供静态文件；未知路径回落到 index.html 以支持前端路由。
func spaHandler(static fs.FS) http.Handler {
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if p != "/" {
			if _, err := fs.Stat(static, p[1:]); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}
		index, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "前端未构建：请在 web/ 下执行 npm run build", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

type apiErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = jsonEncode(w, v)
}

// Summary 是托盘和首页用的汇总状态。
type Summary struct {
	// Version 是后台进程的版本。应用更新后外壳据此发现进程还是旧的，重新拉起。
	Version string            `json:"version"`
	Server  dist.ClientStatus `json:"server"`
	// Notices 是待外壳弹出的系统通知（续期提醒、更新），外壳按 ID 去重。
	Notices []Notice `json:"notices,omitempty"`
	// ClaudeLogin 是待托盘外壳启动的 Claude Code 登录（外壳每 3 秒读一次状态）。
	ClaudeLogin *dist.PendingClaude `json:"claude_login,omitempty"`
	// OpenLink 是页面请求在浏览器打开的登录链接，外壳认领后打开。
	OpenLink *dist.PendingOpen `json:"open_link,omitempty"`
	// OTel 是托盘外壳自动配置 Claude Code 遥测用的：env 为空表示应移除之前写入的项。
	OTel *OTelBrief `json:"otel,omitempty"`
	// AppUpdate 是首页点「更新」「立即重启」后待外壳处理的请求。
	AppUpdate *AppUpdateRequest `json:"app_update,omitempty"`
	// Gemini 是托盘外壳写入 Antigravity 凭据、启动登录用的（S1.7）。
	Gemini *dist.GeminiBrief `json:"gemini,omitempty"`
	// WorkspaceAction 是待托盘外壳执行的多开操作（S1.9）。
	WorkspaceAction string `json:"workspace_action,omitempty"`
	// FileAction 是待托盘外壳执行的下载、打开文件、在文件夹中显示（S1.11）。
	FileAction string `json:"file_action,omitempty"`
}

// OTelBrief 是状态里给外壳的遥测摘要。Force 为真时外壳覆盖 settings.json 里已有的 OTel 配置（用户明确开启过）。
type OTelBrief struct {
	Enabled bool              `json:"enabled"`
	Env     map[string]string `json:"env"`
	Force   bool              `json:"force"`
	// SyncRequest 变化时外壳立即重新核对、写入（页面上点了「立即写入配置」）。
	SyncRequest int64 `json:"sync_request"`
}

const tokenFileName = "token"

func ensureSecretFile(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := writeSecret(path, t); err != nil {
		return "", fmt.Errorf("写入 %s 失败：%w", filepath.Base(path), err)
	}
	return t, nil
}

func writeSecret(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

const maxLogSize = 1 << 20

func openLog(dir string) (*slog.Logger, func()) {
	path := filepath.Join(dir, "boxctl.log")
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.New(slog.DiscardHandler), func() {}
	}
	return slog.New(slog.NewTextHandler(f, nil)), func() { _ = f.Close() }
}
