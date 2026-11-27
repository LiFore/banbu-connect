// Package box 是路由盒（iStoreOS）的运行时：在 panel 里托管 Mihomo 内核，
// 严格线路出口检查、管理服务绑定，并把保护策略下发给 guard（ADR-030、ADR-032）。
//
// 界面与接口和桌面客户端对齐：内核启停、线路、设置、管理服务的接口与桌面相同，前缀为 /api/v1/box。
package box

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"boxctl/internal/boxfind"
	"boxctl/internal/buildinfo"
	"boxctl/internal/core"
	"boxctl/internal/dist"
	"boxctl/internal/gen"
	"boxctl/internal/groups"
	"boxctl/internal/guard"
	"boxctl/internal/ipc"
	"boxctl/internal/killswitch"
	"boxctl/internal/logring"
	"boxctl/internal/panel"
	"boxctl/internal/subscriptions"
	"boxctl/internal/trust"
)

const (
	prefix      = "box"
	settingsKey = "box"
	ksKey       = "box_killswitch"
	// DNSListen 是内核的 DNS 监听地址：dnsmasq 把局域网的查询转发到这里。
	DNSListen  = "127.0.0.1:1053"
	syncPeriod = 10 * time.Second
	// 内核控制接口和严格组探测入口：与桌面客户端（19190、19200 起）错开。
	controllerAddr = "127.0.0.1:19290"
	probeBase      = 19300
	ipcTimeout     = 3 * time.Second
)

type Options struct {
	DataDir     string
	GuardSocket string
	// StatusSocket 是本机状态接口（`boxctl status`、LuCI 页面用）；为空时不提供。
	StatusSocket string
	// CoreService 是内核独立服务（ADR-045）；Init 指向的脚本不存在时（开发环境）内核作为面板的子进程运行。
	CoreService core.ServiceOptions
	// RouteExclude 是 TUN 不接管的目标网段：局域网网段，保证管理面板、SSH 始终直达。
	RouteExclude []string
	// DnsmasqRFC6761 为 true 时 init 脚本接管了 dnsmasq 的 boguspriv，guard 的片段要补回它（ADR-081）。
	DnsmasqRFC6761 bool
	Logger         *slog.Logger
	// LANDevice 是局域网网桥（读邻居表），LeasesPath 是 dnsmasq 租约文件；为空时用 br-lan、/tmp/dhcp.leases。
	LANDevice  string
	LeasesPath string
	// Logs 是面板进程的内存日志（M11-03）。
	Logs *logring.Ring
	// Discovery 是客户端发现路由盒的应答端口（ADR-073），Listen 为空时不开。
	Discovery DiscoveryOptions
	// ResolvPaths 是 WAN 口 DNS 的 resolv.conf.auto 位置，为 nil 时用 OpenWrt 的默认位置；测试里替换。
	ResolvPaths []string
	// FetchRuleSet 下载规则集，为 nil 时用 HTTP；测试里替换掉，避免访问网络。
	FetchRuleSet func(ctx context.Context, rawURL, proxy string) ([]byte, error)
}

// DiscoveryOptions 是发现端口的设置。Secret 为空时用管理服务下发的口令（绑定了管理服务时），
// 两者都没有时端口开着但不应答；ID 为空时用主机名。
type DiscoveryOptions struct {
	Listen string
	Secret string
	ID     string
}

// Box 把各部分组装起来。先调用 Extend 交给 panel，再用 Wrap 包装处理器。
type Box struct {
	ctx  context.Context
	opts Options
	log  *slog.Logger

	db        *sql.DB
	subs      *subscriptions.Service
	groups    *groups.Service
	sup       *core.Supervisor
	strict    *trust.StrictChecker
	dist      *dist.Client
	expand    *expander
	monitor   *monitor
	stats     *core.ConnStats // 代理连接统计；没有管理服务客户端时为空
	discovery discovery
	clients   *clientRegistry // 来复查路由盒的 Banbu Connect 客户端（设备页）

	pushMu       sync.Mutex
	pushed       string        // 最近一次成功下发给 guard 的策略版本
	retryDelay   time.Duration // 下发失败后的重试间隔，成功后清零
	retryPending bool
	pushAt       time.Time // 最近一次下发（成功或失败）
	pushErr      string    // 最近一次下发的错误，成功时为空

	netMu      sync.Mutex
	netFP      string    // 当前的网络摘要（默认路由、WAN 地址）
	netChanges int       // 面板运行以来网络变化的次数
	netAt      time.Time // 最近一次网络变化

	health atomic.Value // trust.StrictHealth：严格线路检查的阈值，探测时读取，避免每次查库

	versionMu sync.Mutex // 串行化配置版本的记录和状态更新
}

func New(ctx context.Context, opts Options) *Box {
	return &Box{ctx: ctx, opts: opts, log: opts.Logger}
}

func defaultSettings() core.Settings {
	return core.Settings{Mode: "rule", TUN: true, MixedPort: 7890, CustomRules: core.DefaultCustomRules(), ConnStats: true}
}

// Extend 是 panel.Options.Extend：组装运行时、启动后台任务、注册接口。
func (b *Box) Extend(svc panel.Services) (func(*http.ServeMux, func(http.HandlerFunc) http.Handler), func(*http.ServeMux)) {
	b.db, b.subs, b.groups = svc.DB, svc.Subs, svc.Groups
	b.discovery = defaultDiscovery(b.opts.LANDevice)
	b.clients = newClientRegistry()
	if b.opts.LeasesPath != "" {
		b.discovery.leasesPath = b.opts.LeasesPath
	}
	if err := core.SeedGroups(b.ctx, svc.DB, svc.Groups); err != nil {
		b.log.Error("写入默认代理组失败", "err", err)
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	secret, err := ensureSecret(filepath.Join(b.opts.DataDir, "core-secret"))
	if err != nil {
		b.log.Error("生成内核控制密钥失败", "err", err)
	}
	store := core.Store{DB: svc.DB, Key: settingsKey, Defaults: defaultSettings}
	b.sup = core.NewSupervisor(core.Data{DB: svc.DB, Nodes: svc.Nodes, Groups: svc.Groups}, store, core.Hooks{
		Policy: func(core.Settings) core.Policy {
			cfg, _ := b.ksConfig(context.Background())
			rules, sub, exclude := b.deviceConfig(context.Background())
			return core.Policy{Services: cfg.Services, DNSListen: DNSListen, Router: true, RouteExclude: b.opts.RouteExclude,
				LANAllowedIPs: b.opts.RouteExclude, // LAN 网段和经 VPN 等接口进来的网段
				DeviceRules:   rules, SubRules: sub, ExcludeMAC: exclude,
				LocalDNS:    b.localDNS(), // 内网域名问上级本地 DNS
				ClientProxy: b.clientProxy()}
		},
		OnApplied:  func(context.Context, bool) { go b.push() },
		BeforeStop: func(context.Context) { b.push() },
		Confirm:    b.confirmEgress,
		OnConfirm:  b.onConfirm,
	}, self, filepath.Join(b.opts.DataDir, "core"), secret, b.log)
	b.sup.SetPorts(controllerAddr, probeBase)
	if init := b.opts.CoreService.Init; init != "" {
		if _, err := os.Stat(init); err == nil {
			b.sup.UseService(b.opts.CoreService)
		} else {
			b.log.Warn("没有内核服务脚本，内核作为面板的子进程运行（开发环境）", "init", init)
		}
	}

	if c, err := dist.NewClient(b.opts.DataDir, buildinfo.Version, b.log); err != nil {
		b.log.Error("管理服务客户端初始化失败", "err", err)
	} else {
		b.dist = c
		b.sup.Dist = c
	}
	b.strict = trust.NewStrictChecker(b.sup.StrictTargets, b.sup.StrictEgress, trust.DefaultProbeURLs)
	if cfg, err := b.ksConfig(b.ctx); err == nil {
		b.health.Store(cfg.Health)
	}
	b.strict.SetHealth(func() trust.StrictHealth {
		h, _ := b.health.Load().(trust.StrictHealth)
		return h
	})
	b.sup.OnLines = b.strict.Invalidate

	b.expand = newExpander(b.opts.DataDir, b.sup.StrictRuleSets, b.sup.MixedProxy)
	if b.opts.FetchRuleSet != nil {
		b.expand.fetch = b.opts.FetchRuleSet
	}

	go b.sup.Run(b.ctx)
	go b.watchUpstreamDNS(b.ctx)
	b.serveDiscovery()
	go b.expand.Run(b.ctx, func() { go b.push() })
	b.monitor = newMonitor(b.sup)
	if b.dist != nil {
		// 代理连接统计（S1.8）：和连接页共用每秒的采样；队列放在内存盘，不写闪存（重启后丢失未上报的部分）。
		st, _ := store.Load(b.ctx)
		b.stats = core.NewConnStats(filepath.Join(os.TempDir(), "boxctl-conn-stats.json"), "box", b.dist, st.ConnStats, b.log)
		b.stats.SetDecorate(b.decorateConns)
		b.monitor.stats = b.stats
		go b.stats.Run(b.ctx)
	}
	go b.monitor.Run(b.ctx)
	go b.strict.Run(b.ctx, func() { go b.push() })
	if b.dist != nil {
		go b.dist.Run(b.ctx, b.sup.Trigger)
	}
	go b.syncGuard()
	go b.watchNetwork()
	if b.opts.StatusSocket != "" {
		go b.serveStatus(b.opts.StatusSocket)
	}
	if svc.Subs != nil {
		// 订阅定时更新（M2-11）：节点变了就重新生成配置。
		go svc.Subs.RunScheduler(b.ctx, b.log, b.sup.Trigger)
	}

	admin := func(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
		b.routes(mux, authed)
		b.versionRoutes(mux, authed)
		b.monitorRoutes(mux, authed)
		b.logRoutes(mux, authed)
		b.deviceRoutes(mux, authed)
		b.diagnosticsRoutes(mux, authed)
		core.Routes(mux, authed, prefix, b.sup, b.strict, func() { go b.push() })
		core.SettingsRoutes(mux, authed, prefix, b.sup, false)
		if b.dist != nil {
			core.ServerRoutes(mux, authed, prefix, b.dist, b.sup, core.Device{Name: deviceName, OSVersion: osVersion})
			core.ConnStatsRoutes(mux, authed, prefix, b.stats, b.sup)
		}
	}
	return admin, nil
}

// Wrap 是 panel.Options.Wrap：数据变化后自动重新生成配置，内核启停后更新 guard 策略。
func (b *Box) Wrap(next http.Handler) http.Handler {
	apply := core.AutoApply(next, b.sup, "/api/v1/subscriptions", "/api/v1/nodes", "/api/v1/groups",
		"/api/v1/box/settings", "/api/v1/box/killswitch/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apply.ServeHTTP(w, r)
		if r.Method == http.MethodPost && (r.URL.Path == "/api/v1/box/start" || r.URL.Path == "/api/v1/box/stop") {
			go b.push()
		}
	})
}

// ---- Kill Switch 配置 ----

// KillSwitch 是路由盒的保护配置。启用时 guard 拦截受保护服务的直连（ADR-032）。
type KillSwitch struct {
	Enabled  bool                 `json:"enabled"`
	Services []killswitch.Service `json:"services"`
	// DNSListen 只为与桌面的接口形状一致，固定为内核的 DNS 监听地址。
	DNSListen string `json:"dns_listen"`
	// Health 是严格线路检查的阈值（M9-05）。
	Health trust.StrictHealth `json:"health"`
}

func (b *Box) ksConfig(ctx context.Context) (KillSwitch, error) {
	cfg := KillSwitch{Enabled: true, Services: killswitch.DefaultServices(), DNSListen: DNSListen, Health: trust.DefaultStrictHealth()}
	var raw string
	err := b.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, ksKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	cfg.DNSListen = DNSListen
	cfg.Health = cfg.Health.Normalize()
	return cfg, nil
}

func (b *Box) saveKS(ctx context.Context, cfg KillSwitch) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = b.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`, ksKey, string(raw))
	return err
}

// ---- guard 策略 ----

// Policy 由当前配置和状态算出下发给 guard 的策略。
func (b *Box) Policy(ctx context.Context) (guard.Policy, error) {
	cfg, err := b.ksConfig(ctx)
	if err != nil {
		return guard.Policy{}, err
	}
	running := b.sup.Status().Running
	// 内核应不应该在运行直接读设置：面板刚启动时状态还没刷新，不能让 guard 以为内核是用户停掉的而解除阻断。
	expected := false
	if st, err := b.sup.Settings().Load(ctx); err == nil {
		expected = st.Running
	}
	p := guard.Policy{DNSUpstream: DNSListen, ForwardAll: running, RFC6761: b.opts.DnsmasqRFC6761,
		Suffixes: []string{}, CIDRs: []string{},
		CoreExpected: expected, CoreController: controllerAddr}
	// 设备断网与时段管控与受保护服务无关，关闭 Kill Switch 时仍然生效（ADR-036）。
	p.Blocks = b.deviceBlocks(ctx)
	if !cfg.Enabled {
		return p, nil
	}
	for _, s := range cfg.Services {
		if !s.Enabled {
			continue
		}
		p.Suffixes = append(p.Suffixes, s.Suffixes...)
		p.CIDRs = append(p.CIDRs, s.CIDRs...)
	}
	// 目标为严格组的规则集里的域名和网段也受保护（ADR-039）。
	b.expand.Merge(&p)
	// 内核不在运行时受保护的地址本来就被拦着；在运行时严格线路的出口不符就整体阻断。
	if st := b.strict.Status(); running && st.Reason != "" {
		p.Hold, p.Reason = true, st.Reason
	}
	return p, nil
}

// push 把策略下发给 guard。guard 不可达时只记日志，下一轮同步时重试。
func (b *Box) push() {
	b.pushMu.Lock()
	defer b.pushMu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, ipcTimeout)
	defer cancel()
	p, err := b.Policy(ctx)
	if err != nil {
		b.log.Warn("计算保护策略失败", "err", err)
		return
	}
	// 先切内核里的 hold 副本：它覆盖 guard 的严格集合拦不到的连接（经加密 DNS 拿到的地址）。
	failed := false
	var errs []string
	if err := b.sup.SetHold(ctx, p.Hold); err != nil {
		b.log.Warn("切换内核里的阻断规则失败", "err", err)
		failed = true
		errs = append(errs, err.Error())
	}
	if err := b.recordVersion(ctx, b.sup.AppliedConfig(), p); err != nil {
		b.log.Warn("记录配置版本失败", "err", err)
	}
	var st guard.Status
	if err := ipc.Call(ctx, b.opts.GuardSocket, guard.MethodApplyPolicy, p, &st); err != nil {
		if b.pushed != "" || !errors.Is(err, ipc.ErrUnreachable) {
			b.log.Warn("下发保护策略失败", "err", err)
		}
		b.pushed = ""
		failed = true
		errs = append(errs, "下发给 guard 失败："+err.Error())
	} else {
		b.pushed = st.PolicyVersion
	}
	b.pushAt, b.pushErr = time.Now().UTC(), strings.Join(errs, "；")
	if failed {
		b.retryPush()
	} else {
		b.retryDelay = 0
	}
}

// retryPush 下发失败时很快重试（1、2、4、8 秒），不等 10 秒一次的定期同步：
// 阻断没送到 guard 或内核的这段时间，受保护的服务仍按旧状态放行。调用方持有 pushMu。
func (b *Box) retryPush() {
	if b.retryPending {
		return
	}
	b.retryDelay = min(max(b.retryDelay*2, time.Second), 8*time.Second)
	b.retryPending = true
	time.AfterFunc(b.retryDelay, func() {
		b.pushMu.Lock()
		b.retryPending = false
		b.pushMu.Unlock()
		if b.ctx.Err() == nil {
			b.push()
		}
	})
}

// syncGuard 定期下发：guard 重启后没有收到最新的状态（例如 hold 解除）时能自己跟上。
func (b *Box) syncGuard() {
	b.push()
	t := time.NewTicker(syncPeriod)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
			b.push()
		}
	}
}

func (b *Box) guardStatus(ctx context.Context) (*guard.Status, string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var st guard.Status
	switch err := ipc.Call(ctx, b.opts.GuardSocket, guard.MethodStatus, nil, &st); {
	case err == nil:
		return &st, ""
	case errors.Is(err, ipc.ErrUnreachable):
		return nil, "guard 没有运行"
	default:
		return nil, err.Error()
	}
}

// ---- 接口 ----

// Summary 是概览页用的汇总状态，与桌面的 /desktop/status 对应。
type Summary struct {
	Version    string             `json:"version"`
	Core       core.CoreStatus    `json:"core"`
	Guard      *guard.Status      `json:"guard"`
	GuardError string             `json:"guard_error,omitempty"`
	KillSwitch KillSwitch         `json:"killswitch"`
	Strict     trust.StrictStatus `json:"strict"`
	Server     *dist.ClientStatus `json:"server,omitempty"`
	// StrictExpansion 是目标为严格组的规则集展开进保护范围的结果（ADR-039）。
	StrictExpansion StrictExpansion `json:"strict_expansion"`
	// SubscriptionErrors 是最近一次更新失败的订阅，首页显示（M2-11）。
	SubscriptionErrors []SubscriptionError `json:"subscription_errors"`
	// Subscriptions 是各订阅的用量和到期；ConfigVersion 是当前运行配置的版本号（M11-01）。
	Subscriptions []SubscriptionUsage `json:"subscriptions"`
	ConfigVersion int64               `json:"config_version,omitempty"`
}

// SubscriptionUsage 是一个订阅的用量和到期（来自订阅响应头 subscription-userinfo）。
type SubscriptionUsage struct {
	Name      string     `json:"name"`
	Nodes     int        `json:"nodes"`
	Used      int64      `json:"used"`  // 已用流量（上传 + 下载），字节
	Total     int64      `json:"total"` // 总流量，0 表示未知
	Expire    *time.Time `json:"expire,omitempty"`
	UpdatedAt string     `json:"updated_at"`
}

// SubscriptionError 是一个订阅最近一次更新失败的原因。
type SubscriptionError struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

func (b *Box) routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/v1/box/status", authed(func(w http.ResponseWriter, r *http.Request) {
		core.WriteJSON(w, http.StatusOK, b.summary(r.Context()))
	}))
	mux.Handle("GET /api/v1/box/killswitch/config", authed(func(w http.ResponseWriter, r *http.Request) {
		cfg, err := b.ksConfig(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, cfg)
	}))
	mux.Handle("PUT /api/v1/box/killswitch/config", authed(func(w http.ResponseWriter, r *http.Request) {
		var in KillSwitch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "请求内容无效"})
			return
		}
		services, err := killswitch.NormalizeServices(in.Services)
		if err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "invalid", Message: err.Error()})
			return
		}
		cfg, err := b.ksConfig(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		cfg.Services = services
		if err := b.saveKS(r.Context(), cfg); err != nil {
			core.Fail(w, err)
			return
		}
		go b.push()
		core.WriteJSON(w, http.StatusOK, cfg)
	}))
	toggle := func(enabled bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			cfg, err := b.ksConfig(r.Context())
			if err != nil {
				core.Fail(w, err)
				return
			}
			cfg.Enabled = enabled
			if err := b.saveKS(r.Context(), cfg); err != nil {
				core.Fail(w, err)
				return
			}
			b.push()
			st, msg := b.guardStatus(r.Context())
			core.WriteJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "guard": st, "guard_error": msg})
		}
	}
	mux.Handle("POST /api/v1/box/killswitch/enable", authed(toggle(true)))
	mux.Handle("POST /api/v1/box/killswitch/disable", authed(toggle(false)))
	mux.Handle("PUT /api/v1/box/killswitch/health", authed(func(w http.ResponseWriter, r *http.Request) {
		var in trust.StrictHealth
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "请求内容无效"})
			return
		}
		cfg, err := b.ksConfig(r.Context())
		if err != nil {
			core.Fail(w, err)
			return
		}
		cfg.Health = in.Normalize()
		if err := b.saveKS(r.Context(), cfg); err != nil {
			core.Fail(w, err)
			return
		}
		b.health.Store(cfg.Health)
		b.strict.Kick()
		core.WriteJSON(w, http.StatusOK, cfg)
	}))
	mux.Handle("GET /api/v1/box/guard", authed(func(w http.ResponseWriter, r *http.Request) {
		st, msg := b.guardStatus(r.Context())
		core.WriteJSON(w, http.StatusOK, map[string]any{"guard": st, "guard_error": msg})
	}))
}

// summary 汇总概览页的状态（M11-01），诊断包也用它。
func (b *Box) summary(ctx context.Context) Summary {
	st := b.sup.Status()
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_nodes WHERE enabled = 1`).Scan(&n); err == nil {
		st.NodeCount = n
	}
	ks, err := b.ksConfig(ctx)
	if err != nil {
		b.log.Warn("读取保护配置失败", "err", err)
	}
	sum := Summary{Version: buildinfo.Version, Core: st, KillSwitch: ks, Strict: b.strict.Status(),
		StrictExpansion: b.expand.Result(), SubscriptionErrors: []SubscriptionError{}, Subscriptions: []SubscriptionUsage{}}
	if b.subs != nil {
		if list, err := b.subs.List(ctx); err == nil {
			for _, sb := range list {
				if !sb.Enabled {
					continue
				}
				name := sb.DisplayName
				if name == "" {
					name = sb.Name
				}
				if sb.LastError != "" {
					sum.SubscriptionErrors = append(sum.SubscriptionErrors, SubscriptionError{Name: name, Error: sb.LastError})
				}
				u := SubscriptionUsage{Name: name, Nodes: sb.NodeCount, UpdatedAt: sb.LastFetchAt}
				if ui := sb.UserInfo; ui != nil {
					u.Used, u.Total = ui.Upload+ui.Download, ui.Total
					if ui.Expire > 0 {
						t := time.Unix(ui.Expire, 0).UTC()
						u.Expire = &t
					}
				}
				sum.Subscriptions = append(sum.Subscriptions, u)
			}
		}
	}
	if st.ConfigHash != "" {
		_ = b.db.QueryRowContext(ctx, `SELECT id FROM config_versions WHERE config_hash = ? ORDER BY id DESC LIMIT 1`, st.ConfigHash).Scan(&sum.ConfigVersion)
	}
	sum.Guard, sum.GuardError = b.guardStatus(ctx)
	if b.dist != nil {
		s := b.dist.Status()
		sum.Server = &s
	}
	return sum
}

// ---- 本机信息 ----

func deviceName() string {
	if b, err := os.ReadFile("/proc/sys/kernel/hostname"); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return h
}

// osVersion 从 /etc/openwrt_release 读系统版本，如 iStoreOS 24.10.2。
func osVersion() string {
	b, err := os.ReadFile("/etc/openwrt_release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "DISTRIB_DESCRIPTION="); ok {
			return strings.Trim(strings.TrimSpace(v), `'"`)
		}
	}
	return "OpenWrt"
}

// serveDiscovery 开客户端发现路由盒的应答端口（ADR-073）。客户端问到 boxfind.Name（dnsmasq 记录，由 init 脚本写）
// 后连这个端口，验证通过就把流量交给路由盒出口。
// clientProxy 是给客户端的代理入口（ADR-084）：开着发现时在 LAN 地址上监听，密码由发现口令派生；
// 口令为空（没绑定管理服务也没手填）时不开。
func (b *Box) clientProxy() *gen.ProxyEndpoint {
	host, _, err := net.SplitHostPort(b.opts.Discovery.Listen)
	if err != nil || host == "" {
		return nil
	}
	pass := boxfind.ProxyPassword(b.discoverySecret())
	if pass == "" {
		return nil
	}
	return &gen.ProxyEndpoint{Server: host, Port: boxfind.ProxyPort, User: boxfind.ProxyUser, Password: pass}
}

// discoverySecret 是发现口令：init 脚本手填的优先，否则用管理服务下发的。
func (b *Box) discoverySecret() string {
	if s := b.opts.Discovery.Secret; s != "" {
		return s
	}
	if b.sup == nil {
		return ""
	}
	return b.sup.BoxSecret()
}

func (b *Box) serveDiscovery() {
	d := b.opts.Discovery
	if d.Listen == "" {
		return
	}
	id := d.ID
	if id == "" {
		host, _ := os.Hostname()
		id = boxfind.DefaultID(host)
	}
	if err := boxfind.Serve(b.ctx, d.Listen, id, b.discoverySecret, b.log, b.clients); err != nil {
		b.log.Warn("路由盒发现端口启动失败", "addr", d.Listen, "err", err)
		return
	}
	b.log.Info("路由盒发现端口已开启", "addr", d.Listen, "id", id, "secret_from_server", d.Secret == "")
}
