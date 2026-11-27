// Package panel 是 Web 管理面板：REST API + 内嵌前端。
package panel

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"time"

	"boxctl/internal/auth"
	"boxctl/internal/buildinfo"
	"boxctl/internal/certs"
	"boxctl/internal/groups"
	"boxctl/internal/guard"
	"boxctl/internal/ipc"
	"boxctl/internal/nodes"
	"boxctl/internal/secret"
	"boxctl/internal/store"
	"boxctl/internal/subscriptions"
	"boxctl/internal/webui"
)

type Options struct {
	Listen      string // HTTPS 监听地址
	HTTPListen  string // 仅做 HTTP→HTTPS 跳转；空则不监听
	DataDir     string // 数据库、设备密钥、证书、初始密码文件
	GuardSocket string
	FetchProxy  string // 订阅 fetch_via=proxy 时使用的本地代理（Mihomo mixed-port）
	Logger      *slog.Logger

	// 以下供管理服务（boxctl server）使用，路由盒面板不设置。
	TLSCert, TLSKey string // 指定证书；为空时生成自签名证书
	PlainHTTP       bool   // 只监听 HTTP，由前面的反向代理负责 HTTPS
	Server          bool   // 管理服务模式：前端显示配置下发相关的页面
	// Box 为路由盒模式：前端显示与桌面客户端同款的界面。
	Box bool
	// Wrap 包装整个处理器（路由盒：数据变化后自动重新生成配置）。
	Wrap func(http.Handler) http.Handler
	// Extend 注册附加路由：admin 需要登录，public 不经过登录（自行认证）。
	Extend func(Services) (admin func(*http.ServeMux, func(http.HandlerFunc) http.Handler), public func(*http.ServeMux))
	// ConnectSrc 返回页面还允许连接的来源（CSP connect-src），如管理服务的 OSS 直传地址。
	ConnectSrc func() []string
}

// 数据目录下的固定文件名
const (
	dbFile          = "boxctl.db"
	deviceKeyFile   = "device.key"
	initialPwdFile  = "initial-password"
	tlsDir          = "tls"
	shutdownTimeout = 5 * time.Second
)

// OpenAuth 打开数据库并返回认证服务；供 panel 与 reset-password 子命令共用。
func OpenAuth(ctx context.Context, dataDir string) (*sql.DB, *auth.Service, error) {
	db, err := store.Open(ctx, filepath.Join(dataDir, dbFile))
	if err != nil {
		return nil, nil, err
	}
	return db, auth.New(db, filepath.Join(dataDir, initialPwdFile)), nil
}

func Run(ctx context.Context, opts Options) error {
	db, authSvc, err := OpenAuth(ctx, opts.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	key, err := secret.LoadOrCreateKey(filepath.Join(opts.DataDir, deviceKeyFile))
	if err != nil {
		return err
	}
	secrets, err := secret.New(key)
	if err != nil {
		return err
	}

	created, err := authSvc.Bootstrap(ctx)
	if err != nil {
		return err
	}
	if created {
		opts.Logger.Warn("generated initial admin password; change it on first login",
			"file", authSvc.InitialPasswordPath)
	}

	host, httpsPort, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return err
	}
	certFile, keyFile := opts.TLSCert, opts.TLSKey
	if !opts.PlainHTTP && certFile == "" {
		if certFile, keyFile, err = certs.EnsureSelfSigned(filepath.Join(opts.DataDir, tlsDir), []string{host}); err != nil {
			return err
		}
	}

	groupSvc := groups.New(db)
	nodeSvc := nodes.New(db, secrets)
	nodeSvc.Groups = groupSvc // 前置代理可以是代理组，需整图校验
	deps := Deps{
		DB: db, Auth: authSvc, Subs: subscriptions.New(db, secrets, opts.FetchProxy),
		Nodes: nodeSvc, Groups: groupSvc,
		GuardSocket: opts.GuardSocket, Static: webui.FS(), Logger: opts.Logger, Server: opts.Server, Box: opts.Box,
		ConnectSrc: opts.ConnectSrc,
	}
	if opts.Extend != nil {
		deps.ExtraRoutes, deps.PublicRoutes = opts.Extend(Services{DB: db, Subs: deps.Subs, Nodes: nodeSvc, Groups: groupSvc, Secrets: secrets})
	}
	handler := NewHandler(deps)
	if opts.Wrap != nil {
		handler = opts.Wrap(handler)
	}
	servers := []*http.Server{{
		Addr:              opts.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}}
	errc := make(chan error, 2)
	go func() {
		if opts.PlainHTTP {
			errc <- servers[0].ListenAndServe()
			return
		}
		errc <- servers[0].ListenAndServeTLS(certFile, keyFile)
	}()

	if opts.HTTPListen != "" {
		redirect := &http.Server{
			Addr:              opts.HTTPListen,
			Handler:           redirectToHTTPS(httpsPort),
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, redirect)
		go func() { errc <- redirect.ListenAndServe() }()
	}
	opts.Logger.Info("panel started", "https", opts.Listen, "http_redirect", opts.HTTPListen,
		"version", buildinfo.String())

	var runErr error
	select {
	case runErr = <-errc:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return runErr
}

func redirectToHTTPS(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		target := url.URL{Scheme: "https", Host: net.JoinHostPort(host, httpsPort), Path: r.URL.Path, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
	})
}

type Deps struct {
	DB          *sql.DB
	Auth        *auth.Service
	Subs        *subscriptions.Service
	Nodes       *nodes.Service
	Groups      *groups.Service
	GuardSocket string
	Static      fs.FS
	Logger      *slog.Logger
	// DesktopToken 非空时为桌面模式：API 只认这个 Bearer 令牌，没有登录页和 Cookie 会话。
	DesktopToken string
	// ExtraRoutes 注册附加的路由（桌面模式、管理服务），authed 负责认证。
	ExtraRoutes func(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler)
	// PublicRoutes 注册不经过登录的路由，由路由自己认证（管理服务的客户端接口）。
	PublicRoutes func(mux *http.ServeMux)
	// Server 为管理服务模式。
	Server bool
	// Box 为路由盒模式。
	Box bool
	// ConnectSrc 见 Options.ConnectSrc。
	ConnectSrc func() []string
}

// Services 是桌面模式复用的数据服务。
type Services struct {
	DB      *sql.DB
	Subs    *subscriptions.Service
	Nodes   *nodes.Service
	Groups  *groups.Service
	Secrets *secret.Box
}

// OpenServices 打开数据库和各数据服务，供桌面模式使用。
func OpenServices(ctx context.Context, dataDir, fetchProxy string) (Services, error) {
	db, err := store.Open(ctx, filepath.Join(dataDir, dbFile))
	if err != nil {
		return Services{}, err
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(dataDir, deviceKeyFile))
	if err != nil {
		db.Close()
		return Services{}, err
	}
	secrets, err := secret.New(key)
	if err != nil {
		db.Close()
		return Services{}, err
	}
	groupSvc := groups.New(db)
	nodeSvc := nodes.New(db, secrets)
	nodeSvc.Groups = groupSvc
	return Services{DB: db, Subs: subscriptions.New(db, secrets, fetchProxy), Nodes: nodeSvc, Groups: groupSvc, Secrets: secrets}, nil
}

type handler struct {
	Deps
}

// NewHandler 组装路由。拆出来便于测试。
func NewHandler(d Deps) http.Handler {
	h := &handler{Deps: d}
	mux := http.NewServeMux()

	if d.DesktopToken != "" {
		mux.HandleFunc("GET /api/v1/auth/session", h.desktopSession)
	} else {
		mux.HandleFunc("POST /api/v1/auth/login", h.login)
		mux.HandleFunc("GET /api/v1/auth/session", h.session)
		mux.Handle("POST /api/v1/auth/logout", h.authed(h.logout, allowMustChange))
		mux.Handle("POST /api/v1/auth/password", h.authed(h.changePassword, allowMustChange))
		mux.Handle("GET /api/v1/health", h.authed(h.health, 0))
	}
	h.routeSubscriptions(mux)
	h.routeNodes(mux)
	h.routeGroups(mux)
	if d.PublicRoutes != nil {
		d.PublicRoutes(mux)
	}
	if d.ExtraRoutes != nil {
		d.ExtraRoutes(mux, func(fn http.HandlerFunc) http.Handler {
			return h.authed(func(w http.ResponseWriter, r *http.Request, _ *requestSession) { fn(w, r) }, 0)
		})
	}

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "unknown api path")
	})
	if d.Server {
		// 管理服务：根路径是落地页（版本信息），后台页面在 /admin 下（前端路由，回落到 index.html）。
		mux.HandleFunc("GET /{$}", landing)
	}
	mux.Handle("/", spaHandler(d.Static))
	return securityHeaders(d.ConnectSrc, sameOrigin(mux))
}

type healthResponse struct {
	Version       string        `json:"version"`
	SchemaVersion int           `json:"schema_version"`
	Guard         *guard.Status `json:"guard"`
	GuardError    string        `json:"guard_error,omitempty"`
}

// health 汇报 panel 自身与 guard 状态。guard 不可达不算 panel 故障（M1-03）。
func (h *handler) health(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	v, err := store.SchemaVersion(r.Context(), h.DB)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "db_error", err.Error())
		return
	}
	resp := healthResponse{Version: buildinfo.Version, SchemaVersion: v}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var st guard.Status
	switch err := ipc.Call(ctx, h.GuardSocket, guard.MethodStatus, nil, &st); {
	case err == nil:
		resp.Guard = &st
	case errors.Is(err, ipc.ErrUnreachable):
		resp.GuardError = "unreachable"
	default:
		resp.GuardError = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// spaHandler 提供静态文件；未知路径回落到 index.html 以支持前端路由（M1-05）。
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

// apiError 是统一错误结构 {code, message, detail}。
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
