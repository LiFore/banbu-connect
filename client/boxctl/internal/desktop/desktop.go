// Package desktop 是桌面客户端的后台服务：复用 panel 的订阅、节点、代理组，
// 加上配置生成、内核运行和 Kill Switch。托盘和窗口由 client/app（Tauri）提供。
package desktop

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"boxctl/internal/boxfind"
	"boxctl/internal/buildinfo"
	"boxctl/internal/core"
	"boxctl/internal/dist"
	"boxctl/internal/gen"
	"boxctl/internal/killswitch"
	"boxctl/internal/otel"
	"boxctl/internal/panel"
	"boxctl/internal/trust"
	"boxctl/internal/webui"
)

// DefaultAddr 是后台服务的固定地址，Tauri 外壳也用它。
const DefaultAddr = "127.0.0.1:17930"

// 出口闸门（闸门直连）的端口，只监听本机：闸门本身，以及内核给它的 DIRECT 入口。
const (
	gatePort       = 19180
	gateDirectPort = 19181
	lanDNSPort     = 19182 // 局域网 DNS 转发口，见 trust.ServeLANDNS
	localDNSPort   = 19183 // 内网域名转发口：任何网络上都转给当前网络的私有地址 DNS（ADR-080）
)

var (
	lanDNSAddr   = fmt.Sprintf("127.0.0.1:%d", lanDNSPort)
	localDNSAddr = fmt.Sprintf("127.0.0.1:%d", localDNSPort)
)

// Options 是 `boxctl desktop run` 的参数。
type Options struct {
	Addr    string
	DataDir string
	Fake    bool // 保护服务用假后端，不改动系统；用于开发和测试
	// FakeSSID 非空时把当前网络当作这个 Wi-Fi，用于预览受信网络。
	FakeSSID string
}

// Main 处理 `boxctl desktop <run|install|uninstall|repair>`。
func Main(args []string) error {
	if len(args) == 0 {
		return errors.New("用法：boxctl desktop <run|install|uninstall|repair>")
	}
	switch args[0] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		opts := Options{}
		fs.StringVar(&opts.Addr, "addr", DefaultAddr, "监听地址")
		fs.StringVar(&opts.DataDir, "data", dataDir(), "数据目录")
		fs.BoolVar(&opts.Fake, "fake", false, "保护服务用假后端，不改动系统")
		fs.StringVar(&opts.FakeSSID, "fake-ssid", "", "把当前网络当作这个 Wi-Fi（预览受信网络）")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if handled, err := runAsService(opts); handled {
			return err
		}
		ctx, stop := notifyContext()
		defer stop()
		return Run(ctx, opts)
	case "install":
		fs := flag.NewFlagSet("install", flag.ContinueOnError)
		user := fs.String("user", "", "macOS：使用客户端的用户名（提权后无法自动得知）")
		tokenPath := fs.String("token-path", "", "令牌写到这个路径，让客户端不提权也能读取")
		result := fs.String("result", "", "把结果（ok 或错误信息）写到这个文件：Windows 提权后外壳拿不到输出")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		err := installService(*user, *tokenPath)
		if *result != "" {
			msg := "ok"
			if err != nil {
				msg = err.Error()
			}
			_ = os.WriteFile(*result, []byte(msg), 0o600)
		}
		return err
	case "uninstall":
		return uninstallService()
	case "repair":
		fs := flag.NewFlagSet("repair", flag.ContinueOnError)
		result := fs.String("result", "", "把结果（ok 或错误信息）写到这个文件：Windows 提权后外壳拿不到输出")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		err := repairSystem()
		if *result != "" {
			msg := "ok"
			if err != nil {
				msg = err.Error()
			}
			_ = os.WriteFile(*result, []byte(msg), 0o600)
		}
		return err
	}
	return fmt.Errorf("未知命令：%s", args[0])
}

// Run 运行后台服务，直到 ctx 结束。退出时停止内核，但不撤销 Kill Switch 规则。
func Run(ctx context.Context, opts Options) error {
	if host, _, err := net.SplitHostPort(opts.Addr); err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("监听地址必须是本机回环地址：%s", opts.Addr)
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return err
	}
	logger, closeLog := openLog(opts.DataDir)
	defer closeLog()
	// 托盘的「完全退出」经接口让服务自己退出；ctx 结束后内核停止、TUN 撤销。
	ctx, quit := context.WithCancel(ctx)
	defer quit()

	token, err := ensureSecretFile(filepath.Join(opts.DataDir, tokenFileName))
	if err != nil {
		return err
	}
	coreSecret, err := ensureSecretFile(filepath.Join(opts.DataDir, "core-secret"))
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	svc, err := panel.OpenServices(ctx, opts.DataDir, "http://127.0.0.1:7890")
	if err != nil {
		return err
	}
	defer svc.DB.Close()
	if err := seedGroups(ctx, svc.DB, svc.Groups); err != nil {
		return err
	}

	var backend killswitch.Backend = killswitch.NewSystemBackend()
	if opts.Fake {
		backend = previewBackend()
	}
	engine, err := killswitch.NewEngine(filepath.Join(opts.DataDir, "killswitch.json"), backend, killswitch.RealProber{}, logger)
	if err != nil {
		return err
	}
	// network 在下面创建；生成配置时（Run 之后）才读它的闸门开关。
	var network *trust.Evaluator
	gateInput := func() *gen.GateInput {
		if network == nil || !network.Config().GateDirect {
			return nil
		}
		return &gen.GateInput{Port: gatePort, Secret: coreSecret, DirectPort: gateDirectPort}
	}
	boxRoute := &boxRouter{kick: make(chan struct{}, 1), log: logger}
	supervisor := core.NewSupervisor(core.Data{DB: svc.DB, Nodes: svc.Nodes, Groups: svc.Groups}, settingsStore(svc.DB),
		desktopHooks(engine, svc.DB, gateInput, boxRoute.Kick, boxRoute.Box), self, filepath.Join(opts.DataDir, "core"), coreSecret, logger)
	engine.SetController(supervisor.ControllerURL(), coreSecret)

	// 管理服务下发的配置：绑定后以服务端为准，本地在「高级」里追加。
	distClient, err := dist.NewClient(opts.DataDir, buildinfo.Version, logger)
	if err != nil {
		return err
	}
	supervisor.Dist = distClient
	distClient.GeminiRoute = geminiRoute(supervisor, engine)

	// 受信网络：在受信 Wi-Fi 上、出口符合要求时直连放行（代理由路由盒负责），出口不对时阻断。
	var detector trust.Detector = trust.NewSystemDetector()
	if opts.FakeSSID != "" {
		detector = fakeDetector{ssid: opts.FakeSSID}
	}
	network, err = trust.New(filepath.Join(opts.DataDir, "network.json"), detector, trust.HTTPProber{}, logger)
	if err != nil {
		return err
	}
	network.SetGate(supervisor.GateDirectURL, func() map[string]string { return gateProbeURLs(engine.Config()) })
	// 路由盒自动发现（ADR-073）：口令随管理服务的配置下发，经物理网卡的 DNS 查找、验证路由盒。
	network.SetBoxDiscovery(supervisor.BoxSecret, trust.InterfaceDNS)
	network.SetBoxHello(func() boxfind.Hello {
		return boxfind.Hello{Name: deviceName(), OS: osVersion(), Version: buildinfo.Version}
	})
	boxRoute.sup = supervisor
	boxRoute.direct = func() bool {
		return network.Config().GateDirect && network.Verdict(engine.Status().Health.TUNOK).Path == killswitch.PathDirect
	}
	// 路由盒的代理入口（ADR-084）：密码由发现口令派生，和路由盒那边一致。
	boxRoute.box = func() *gen.ProxyEndpoint {
		addr, ok := network.BoxAddr()
		pass := boxfind.ProxyPassword(supervisor.BoxSecret())
		if !ok || pass == "" {
			return nil
		}
		return &gen.ProxyEndpoint{Server: addr.String(), Port: boxfind.ProxyPort, User: boxfind.ProxyUser, Password: pass}
	}
	network.SetNotify(engine.Record)
	hold := trust.DefaultHoldListen()
	if opts.Fake {
		hold = "127.0.0.1:10054" // 预览时不占用正式服务的端口
	}
	// 严格线路：经每个严格组探测出口，管理服务下发了允许的出口时，任何一组不符合就阻断。
	strict := trust.NewStrictChecker(supervisor.StrictTargets, supervisor.StrictEgress,
		func() []string { return network.Config().ProbeURLs })
	strict.SetNotify(engine.Record)
	alert := func(title, body string) { engine.Notify(killswitch.NoticeProbeFailed, title, body) }
	network.SetAlert(alert)
	strict.SetAlert(alert)
	// 换了网络，经代理的出口一般不变，但通路可能断了：立即重测，不等下一轮。
	// Kill Switch 也立即核对：TUN 接管时判断不随网络变，但 DNS 防泄漏要跟着受信网络切换。
	network.OnNetworkChange(func() { strict.Kick(); engine.Kick() })
	supervisor.OnLines = strict.Invalidate
	network.OnLeaveTrusted(func(from, to string) { go autoStartProxy(ctx, supervisor, engine, logger, from, to) })
	if !opts.Fake && opts.FakeSSID == "" {
		// 换网络后立即作废出口结果、写上规则，不等 3 秒一次的检查。
		if err := network.Watch(ctx); err != nil {
			logger.Warn("路由变化监听启动失败", "err", err)
			engine.Record("warn", "路由变化监听启动失败（"+err.Error()+"），换网络后要等最多 3 秒才会重新确认")
		}
	}
	gate := trust.NewGate(network, coreSecret, supervisor.GatePorts, logger)
	boxRoute.revokeGate = gate.Revoke
	if err := gate.Serve(ctx, fmt.Sprintf("127.0.0.1:%d", gatePort)); err != nil {
		// 闸门起不来时，内核连不上它，受保护服务的连接失败（不会直连）。
		logger.Warn("出口闸门启动失败", "err", err)
		engine.Record("warn", "出口闸门启动失败（"+err.Error()+"），开启闸门直连时受保护的服务无法访问")
	}

	settings, err := loadSettings(ctx, svc.DB)
	if err != nil {
		return err
	}
	diag := newDiagnostics(opts.DataDir, distClient, diagSources{
		killswitch: engine.Status, network: network.Status, strict: strict.Status, core: supervisor.Status,
	}, settings.Diagnostics, logger)
	engine.SetPolicy(trust.StrictPolicy{Inner: network, Strict: strict}, hold)
	var guard killswitch.DNSGuard = killswitch.NewSystemGuard()
	if opts.Fake {
		guard = &killswitch.FakeGuard{}
	}
	engine.SetDNSGuard(guard, func() bool {
		st, err := loadSettings(context.Background(), svc.DB)
		return err == nil && st.DNSGuard
	})
	// 受信网络就是路由盒网络：路由盒的 DNS 本身防泄漏，而且它会拦公共 DoH，拦了它的 DNS 整机就无法解析。
	engine.SetDNSDelegate(network.OnTrustedNetwork)
	// 局域网 DNS 转发口：受信网络上把内核的国内查询交给路由盒（路由盒网络拦公共 DoH）。
	if !opts.Fake {
		if err := trust.ServeLANDNS(ctx, lanDNSAddr, network.TrustedLANDNS(trust.InterfaceDNS), [2]int{}, logger); err != nil {
			// 起不来时内核的这一路上游失败，国内域名仍用 DoH；路由盒网络上国内域名会解析失败。
			logger.Warn("局域网 DNS 转发口启动失败", "addr", lanDNSAddr, "err", err)
			engine.Record("warn", "局域网 DNS 转发口 "+lanDNSAddr+" 启动失败（"+err.Error()+"），路由盒网络上国内域名可能无法解析")
		}
		if err := trust.ServeLANDNS(ctx, localDNSAddr, network.LocalLANDNS(trust.InterfaceDNS), killswitch.LocalDNSSrcPorts, logger); err != nil {
			logger.Warn("内网域名转发口启动失败", "addr", localDNSAddr, "err", err)
			engine.Record("warn", "内网域名转发口 "+localDNSAddr+" 启动失败（"+err.Error()+"），开代理后 .lan、.local 等内网域名无法解析")
		}
	}
	if err := trust.ServeHold(ctx, hold); err != nil {
		// 端口不可用时阻断仍然有效：解析会失败，只是要等超时。
		logger.Warn("阻断入口启动失败", "addr", hold, "err", err)
		engine.Record("warn", "阻断入口 "+hold+" 启动失败（"+err.Error()+"），阻断时受保护的域名要等超时才失败")
	}

	// Claude Code 遥测：本机接收器默认开启，托盘外壳据状态里的 otel.env 自动配置 Claude Code；数据上报到管理服务。
	telemetry, err := otel.Open(opts.DataDir, logger)
	if err != nil {
		return err
	}
	// 绑定管理服务后上报（不含提示词和助手回复原文）。
	telemetry.SetUploader(distClient.ReportOTel)

	reportUpdate := func(level, msg string, fields map[string]string) {
		logger.Warn(msg, "fields", fields)
		engine.Record(level, msg)
		diag.add("update", level, msg, fields)
	}
	appUpdate := &appUpdater{client: distClient, report: reportUpdate,
		notify: func(title, body string) { engine.Announce("app_update", title, body) }}

	// 代理连接统计（S1.8，ADR-048）：绑定管理服务后统计经代理组的连接并上报，「服务端」页可关闭。
	connStats := core.NewConnStats(filepath.Join(opts.DataDir, "conn-stats.json"), "desktop", distClient, settings.ConnStats, logger)
	connStats.SetDecorate(func(b *dist.ConnBatch) { b.LAN = lanAddrs() })

	// 多开（S1.9）：工作区由托盘外壳管理，这里保存状态、检查保护状态、排队操作、查额度。
	// 信任 Bypass：打开后账号操作不检查保护状态（口令防误操作），页面顶部红色提示。
	bypass := newTrustBypass(filepath.Join(opts.DataDir, trustBypassFile), engine.Record)
	aiGate := quotaGate(supervisor, engine, strict, bypass)
	quota := newAIQuota(aiGate)
	distClient.GeminiGate = func() error { _, err := aiGate(kindGemini); return err }
	ws := newWorkspaces(engine, strict, quota)
	ws.bypass = bypass
	// 管理服务托管的 Claude / Codex 凭据：写进工作区、读回上传、鉴权测试、上报额度（ADR-061）。
	creds := newAICreds(distClient, ws, quota, opts.DataDir, logger, func(title, body string) { engine.Announce("ai_refresh", title, body) })
	downloads := &fileJobs{} // 下载页（S1.11）：排队给外壳执行

	handler := panel.NewHandler(panel.Deps{
		Subs: svc.Subs, Nodes: svc.Nodes, Groups: svc.Groups, DB: svc.DB, Static: webui.FS(), Logger: logger,
		DesktopToken: token,
		ExtraRoutes: func(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
			engine.Routes(mux, authed)
			routes(mux, authed, supervisor, engine, svc, network, strict, telemetry, appUpdate, ws, downloads, bypass)
			bypassRoutes(mux, authed, bypass)
			mux.Handle("POST /api/v1/desktop/shutdown", authed(func(w http.ResponseWriter, _ *http.Request) {
				logger.Info("收到完全退出请求，后台服务停止")
				writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
				// 先让响应发出去再退出。
				go func() {
					time.Sleep(300 * time.Millisecond)
					stopSelf(quit)
				}()
			}))
			// 客户端「退出」：只停内核，服务保留，设置不变；下次打开客户端（resume）或服务重启时恢复。
			mux.Handle("POST /api/v1/desktop/quit", authed(func(w http.ResponseWriter, r *http.Request) {
				logger.Info("客户端退出，内核暂停，后台服务保留")
				supervisor.Pause(r.Context())
				writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			}))
			mux.Handle("POST /api/v1/desktop/resume", authed(func(w http.ResponseWriter, r *http.Request) {
				if supervisor.Resume(r.Context()) {
					logger.Info("客户端打开，恢复内核")
				}
				writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			}))
			// 客户端「卸载」：在独立进程里停止并删除后台服务、撤销保护规则，数据保留。
			mux.Handle("POST /api/v1/desktop/uninstall", authed(func(w http.ResponseWriter, _ *http.Request) {
				if opts.Fake {
					writeJSON(w, http.StatusBadRequest, map[string]string{"code": "fake", "message": "预览模式不能卸载"})
					return
				}
				if err := spawnUninstall(); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "uninstall_failed", "message": err.Error()})
					return
				}
				logger.Info("收到卸载请求，后台服务即将删除")
				writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
			}))
			networkRoutes(mux, authed, engine, network)
			egressRoutes(mux, authed, supervisor, engine)
			core.ServerRoutes(mux, authed, "desktop", distClient, supervisor, core.Device{Name: deviceName, OSVersion: osVersion})
			core.RulesRefreshRoutes(mux, authed, "desktop", distClient, supervisor)
			updateRoutes(mux, authed, distClient, reportUpdate, appUpdate.SetAppVersion)
			appUpdate.routes(mux, authed)
			diagRoutes(mux, authed, diag, svc)
			claudeRoutes(mux, authed, distClient, ws.Check)
			workspaceRoutes(mux, authed, ws, distClient)
			fileRoutes(mux, authed, distClient, downloads)
			core.ConnStatsRoutes(mux, authed, "desktop", connStats, supervisor)
			geminiRoutes(mux, authed, distClient)
			telemetry.Routes(mux, authed, writeJSON)
		},
	})

	done := make(chan struct{})
	go func() { defer close(done); supervisor.Run(ctx) }()
	go engine.Run(ctx)
	go network.Run(ctx, func() bool { return engine.Status().Health.TUNOK }, func() { engine.Kick(); boxRoute.Kick() })
	go boxRoute.Run(ctx)
	go strict.Run(ctx, engine.Kick)
	go distClient.Run(ctx, supervisor.Trigger)
	go distClient.RunGemini(ctx)
	go distClient.RunAI(ctx)
	go creds.Run(ctx)
	go runAIOnline(ctx, distClient, ws, logger)                        // AI 账号在线时长（按切换时间算）
	go runAccess(ctx, distClient, supervisor, engine, network, logger) // 接入状态（经路由盒、本机代理等）
	go core.RunConnSampler(ctx, supervisor, connStats)
	go connStats.Run(ctx)
	go quota.Run(ctx)
	go appUpdate.Run(ctx)
	go distClient.RunGeminiQuota(ctx)
	go diag.Run(ctx)
	go telemetry.Run(ctx)

	logger.Info("desktop service started", "addr", opts.Addr, "fake", opts.Fake)
	if opts.Fake {
		fmt.Printf("http://%s/#token=%s\n", opts.Addr, token)
	}
	srv := &http.Server{Addr: opts.Addr, Handler: requireLocalHost(autoApply(handler, supervisor)), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err = srv.ListenAndServe()
	<-done // 等内核退出，它要撤销 TUN 和路由
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// autoStartProxy 在离开受信 Wi-Fi 后启动内核：在受信网络上代理交给路由盒，用户可能关着本机内核，
// 换到普通网络后不启动的话，受保护的服务会被 Kill Switch 阻断、其他应用直连。内核已经在运行（或正在启动）时不动。
func autoStartProxy(ctx context.Context, s *core.Supervisor, ks *killswitch.Engine, log *slog.Logger, from, to string) {
	if s.Status().Desired {
		return
	}
	where := fmt.Sprintf("已离开受信网络%s，当前为%s", from, to)
	log.Info("离开受信网络，自动启动代理", "from", from, "to", to)
	if err := s.SetRunning(ctx, true); err != nil {
		ks.Record("error", where+"，自动启动代理失败："+err.Error())
		ks.Announce(killswitch.NoticeAutoStart, "自动启动代理失败", err.Error())
		return
	}
	ks.Record("info", where+"，已自动启动代理")
	ks.Announce(killswitch.NoticeAutoStart, "已自动启动代理", where)
}

// gateProbeURLs 给每个受保护服务一个它自己的出口探测地址（第一个域名后缀的 Cloudflare trace）：
// 闸门按服务分别确认出口，因为路由盒可能按域名把它们分到不同的线路。
func gateProbeURLs(c killswitch.Config) map[string]string {
	out := map[string]string{}
	for _, s := range c.Services {
		if s.Enabled && len(s.Suffixes) > 0 && out[s.Group] == "" {
			out[s.Group] = "https://" + s.Suffixes[0] + "/cdn-cgi/trace"
		}
	}
	return out
}

// autoApply 在数据变化后触发重新生成配置。
func autoApply(next http.Handler, s *core.Supervisor) http.Handler {
	// 受信网络设置里的闸门直连开关会改变生成的配置。
	return core.AutoApply(next, s, "/api/v1/subscriptions", "/api/v1/nodes", "/api/v1/groups", "/api/v1/killswitch/", "/api/v1/desktop/settings",
		"/api/v1/desktop/network/config")
}

// requireLocalHost 校验 Host，防止恶意网页用 DNS 重绑定访问本机端口：
// 服务以管理员权限运行。
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

type apiErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) { core.WriteJSON(w, status, v) }

// Summary 是托盘和首页用的汇总状态。
type Summary struct {
	// Version 是后台服务的版本。应用更新后外壳据此发现服务还是旧的，提示重新安装。
	Version    string             `json:"version"`
	Core       core.CoreStatus    `json:"core"`
	KillSwitch killswitch.Status  `json:"killswitch"`
	Network    trust.Status       `json:"network"`
	Strict     trust.StrictStatus `json:"strict"`
	Server     dist.ClientStatus  `json:"server"`
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
	// TrustBypass 打开时页面顶部显示红色提示。
	TrustBypass *TrustBypassBrief `json:"trust_bypass,omitempty"`
}

// OTelBrief 是状态里给外壳的遥测摘要。Force 为真时外壳覆盖 settings.json 里已有的 OTel 配置（用户明确开启过）。
type OTelBrief struct {
	Enabled bool              `json:"enabled"`
	Env     map[string]string `json:"env"`
	Force   bool              `json:"force"`
	// SyncRequest 变化时外壳立即重新核对、写入（页面上点了「立即写入配置」）。
	SyncRequest int64 `json:"sync_request"`
}

func routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, s *core.Supervisor, ks *killswitch.Engine, svc panel.Services, network *trust.Evaluator, strict *trust.StrictChecker, telemetry *otel.Service, appUpdate *appUpdater, ws *workspaces, files *fileJobs, bypass *trustBypass) {
	mux.Handle("GET /api/v1/desktop/status", authed(func(w http.ResponseWriter, r *http.Request) {
		st := s.Status()
		// 直接查库：内核没启动时也要显示已添加的节点。
		list, err := svc.Nodes.List(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		for _, n := range list {
			if n.Enabled {
				st.NodeCount++
			}
		}
		sum := Summary{Version: buildinfo.Version, Core: st, KillSwitch: ks.Status(), Network: network.Status(), Strict: strict.Status()}
		if telemetry != nil {
			env, force := telemetry.Brief()
			sum.OTel = &OTelBrief{Enabled: len(env) > 0, Env: env, Force: force, SyncRequest: telemetry.SyncRequest()}
		}
		if appUpdate != nil {
			sum.AppUpdate = appUpdate.Pending()
		}
		if ws != nil {
			sum.WorkspaceAction = ws.Pending()
		}
		if files != nil {
			sum.FileAction = files.Pending()
		}
		if bypass.Enabled() {
			b := bypass.Brief()
			sum.TrustBypass = &b
		}
		if s.Dist != nil {
			sum.Server = s.Dist.Status()
			sum.ClaudeLogin = s.Dist.PendingClaude()
			sum.OpenLink = s.Dist.PendingOpen()
			sum.Gemini = s.Dist.GeminiBrief()
		}
		writeJSON(w, http.StatusOK, sum)
	}))
	core.Routes(mux, authed, "desktop", s, strict, ks.Kick)
	core.SettingsRoutes(mux, authed, "desktop", s, true)
	(&trayStats{}).routes(mux, authed, s)
}

// desktopHooks：受保护服务和 DNS 监听来自 Kill Switch；热重载后重新确认规则检查。
// applied 在新配置生效后调用（路由盒切换组按当前网络重新选择：内核重启时会恢复上次保存的选择）。
func desktopHooks(ks *killswitch.Engine, db *sql.DB, gate func() *gen.GateInput, applied func(), box func() *gen.ProxyEndpoint) core.Hooks {
	return core.Hooks{
		Policy: func(settings core.Settings) core.Policy {
			cfg := ks.Config()
			// Windows 的 strict-route 只放行内核和 TUN 网卡上的 53 端口，系统 DNS 服务按 NRPT
			// 访问回环地址也会被拦，受保护的域名将无法解析。TODO(verify:V22) 实机确认。
			nrptConflict := runtime.GOOS == "windows" && cfg.Enabled
			p := core.Policy{Services: cfg.Services, DNSListen: cfg.DNSListen, LANDNS: lanDNSAddr,
				LocalDNS:    []string{"udp://" + localDNSAddr}, // 内网域名问当前网络的私有地址 DNS，受信与否都问（ADR-080）
				StrictRoute: !nrptConflict, Gate: gate(), Box: box()}
			if nrptConflict && settings.TUN && !settings.DNSGuard {
				p.Warnings = append(p.Warnings, "保护服务启用时无法开启严格路由，又关闭了 DNS 防泄漏：普通域名的 DNS 查询仍可能经物理网卡发出")
			}
			return p
		},
		OnApplied: func(ctx context.Context, reloaded bool) {
			applied()
			if reloaded {
				ks.RefreshRuleChecks(ctx)
			}
		},
	}
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

// previewBackend 把本机非回环网卡当作物理网卡，让 TUN 探测的结果和真实情况一致。
func previewBackend() *killswitch.FakeBackend {
	f := &killswitch.FakeBackend{}
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 && i.Flags&net.FlagUp != 0 {
			f.Ifaces = append(f.Ifaces, i.Name)
		}
	}
	return f
}
