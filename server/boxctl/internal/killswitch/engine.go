package killswitch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

const (
	healthInterval    = 5 * time.Second
	reconcileInterval = 30 * time.Second
	maxEvents         = 200
)

// 总体状态。
const (
	StateOff        = "off"        // 未启用
	StateProtecting = "protecting" // 规则就绪，流量经 TUN 走 Mihomo
	StateProxyOnly  = "proxy_only" // Mihomo 在，但 TUN 没接管：只有走系统代理的应用能访问受保护服务
	StateBlocking   = "blocking"   // Mihomo 不可用：受保护服务被断开，这正是 Kill Switch 在起作用
	StateError      = "error"      // 规则没能写到系统上：保护可能不完整
	StateDirect     = "direct"     // 受信网络上直连放行：没有写规则，代理由路由盒负责
	StateHeld       = "held"       // 按网络判断阻断：受保护的域名交给阻断入口
)

type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type Status struct {
	State      string      `json:"state"`
	Enabled    bool        `json:"enabled"`
	Verdict    Verdict     `json:"verdict"`
	DNSGuard   GuardStatus `json:"dns_guard"`
	Health     Health      `json:"health"`
	Rules      Actual      `json:"rules"`
	Desired    Desired     `json:"desired"`
	LastError  string      `json:"last_error"`
	RuleChecks []RuleCheck `json:"rule_checks"`
	Events     []Event     `json:"events"`
	Notices    []Notice    `json:"notices"` // 按时间先后，托盘外壳据此弹系统通知
}

// NotReadyError 表示启用前检查没有通过：现在启用会切断受保护的服务。
type NotReadyError struct{ Reason string }

func (e *NotReadyError) Error() string { return e.Reason }

type Engine struct {
	path    string
	backend Backend
	prober  Prober
	log     *slog.Logger

	opMu    sync.Mutex // 串行化所有改动系统的操作；耗时操作不阻塞 Status
	guardMu sync.Mutex // 串行化 DNS 防泄漏规则的改动：健康检查和启用、改配置可能同时触发
	kick    chan struct{}

	// policy、holdListen、guard 在 Run 之前设置，之后只读。
	policy     Policy
	holdListen string
	guard      DNSGuard
	guardOn    func() bool // 设置里是否打开了 DNS 防泄漏
	delegate   func() bool // 当前在受信网络上，DNS 交给路由盒；为 nil 时总是 false

	mu         sync.Mutex
	cfg        Config
	ifaces     []string
	rules      Actual
	health     Health
	lastErr    string
	ruleChecks []RuleCheck
	events     []Event
	probed     bool
	verdict    Verdict // 系统上实际生效的路
	guardState GuardStatus
	guardIfs   []string // DNS 防泄漏生效时拦截的网卡
	notices    noticeState
}

func NewEngine(path string, backend Backend, prober Prober, log *slog.Logger) (*Engine, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	return &Engine{path: path, backend: backend, prober: prober, log: log, cfg: cfg,
		kick: make(chan struct{}, 1), policy: proxyOnly{}, verdict: Verdict{Path: PathProxy},
		notices: noticeState{now: time.Now}}, nil
}

// SetPolicy 让 Engine 按网络选择路径。holdListen 是阻断入口（一律拒绝的 DNS），
// 服务不在时端口关闭，解析同样失败。必须在 Run 之前调用。
func (e *Engine) SetPolicy(p Policy, holdListen string) {
	e.policy, e.holdListen = p, holdListen
}

// SetDNSGuard 让 Engine 在 TUN 接管时拦截物理网卡上的 DNS。enabled 读设置。必须在 Run 之前调用。
func (e *Engine) SetDNSGuard(g DNSGuard, enabled func() bool) {
	e.guard, e.guardOn = g, enabled
}

// SetDNSDelegate 设置「当前在受信网络上」的判断：这时 DNS 交给路由盒（它本身防泄漏），不拦截物理网卡上的 DNS。
// 路由盒网络会拦公共 DoH，系统和内核都只能用路由盒的 DNS，拦了就整机无法解析。必须在 Run 之前调用。
// 判断要偏向安全：网络刚变化、还没确认时应返回 false。
func (e *Engine) SetDNSDelegate(onTrusted func() bool) { e.delegate = onTrusted }

// syncGuard 让 DNS 防泄漏与 TUN 状态一致：只有 TUN 接管时才拦截，否则整机无法解析域名。
func (e *Engine) syncGuard(ctx context.Context, h Health) {
	if e.guard == nil {
		return
	}
	e.guardMu.Lock()
	defer e.guardMu.Unlock()
	e.mu.Lock()
	ifaces := slices.Clone(e.ifaces)
	cur, curIfs := e.guardState, slices.Clone(e.guardIfs)
	e.mu.Unlock()
	enabled := e.guardOn()
	delegated := enabled && e.delegate != nil && e.delegate()
	want := enabled && !delegated && h.TUNOK && len(ifaces) > 0
	next := GuardStatus{Enabled: enabled, Active: cur.Active, Delegated: delegated}
	switch {
	case want && (!cur.Active || !slices.Equal(curIfs, ifaces)):
		if err := e.guard.Apply(ctx, ifaces); err != nil {
			next.Error = "DNS 防泄漏规则写入失败：" + err.Error()
			_ = e.guard.Remove(ctx)
			next.Active = false
			if cur.Error != next.Error {
				e.event("error", next.Error)
			}
			break
		}
		next.Active = true
		if !cur.Active {
			e.event("info", "已拦截物理网卡上的 DNS（53、853 端口），系统查询只经 TUN")
		}
	case !want && cur.Active:
		if err := e.guard.Remove(ctx); err != nil {
			next.Error = "撤销 DNS 防泄漏规则失败：" + err.Error()
			e.event("error", next.Error)
			break
		}
		next.Active = false
		if delegated {
			e.event("info", "受信网络：DNS 交给路由盒，已撤销物理网卡上的 DNS 拦截")
		} else {
			e.event("info", "已撤销物理网卡上的 DNS 拦截")
		}
	default:
		next.Error = cur.Error
		if !want {
			next.Error = ""
		}
	}
	e.mu.Lock()
	e.guardState = next
	if next.Active {
		e.guardIfs = ifaces
	} else {
		e.guardIfs = nil
	}
	e.mu.Unlock()
}

// Kick 让 Run 立即核对一次，用于 Policy 的判断变化后。
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Record 记一条事件，供 Policy 报告网络变化。
func (e *Engine) Record(level, msg string) { e.event(level, msg) }

// liveVerdict 按最新的健康检查结果询问 Policy。不能在持有 e.mu 时调用。
func (e *Engine) liveVerdict() Verdict {
	e.mu.Lock()
	tun := e.health.TUNOK
	e.mu.Unlock()
	return e.policy.Verdict(tun)
}

// inspectFor 读取系统规则并与某条路的期望比较。直连时只要没有残留规则就算一致。
func (e *Engine) inspectFor(ctx context.Context, cfg Config, p Path, ifaces []string) (Actual, Desired, error) {
	d := cfg.desiredFor(p, e.holdListen, ifaces)
	a, err := e.backend.Inspect(ctx, d)
	if err != nil {
		return a, d, err
	}
	if p == PathDirect {
		a.InSync, a.Diffs = !a.Installed, nil
		if a.Installed {
			a.Diffs = []string{"受信网络上不应有规则"}
		}
	}
	return a, d, nil
}

// Run 循环检查。退出时不撤销规则：程序不在，保护仍在。
func (e *Engine) Run(ctx context.Context) {
	if e.guard != nil {
		// 上次崩溃可能留下了拦截规则；内核还没起来，先撤销，否则整机无法解析域名。
		if err := e.guard.Remove(ctx); err != nil {
			e.event("warn", "撤销残留的 DNS 拦截失败："+err.Error())
		}
		defer func() { _ = e.guard.Remove(context.Background()) }()
	}
	e.Reconcile(ctx)
	e.ProbeOnce(ctx)
	// 健康检查单独一个循环：它最长要等 DNS 探测 5 秒、控制接口 3 秒，
	// 不能让换网络后的规则核对排在它后面（离开受信网络时，这段时间受保护的服务没有任何规则）。
	go func() {
		health := time.NewTicker(healthInterval)
		defer health.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-health.C:
				e.ProbeOnce(ctx)
			}
		}
	}()
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			e.Reconcile(ctx)
		case <-e.kick:
			e.Reconcile(ctx)
		}
	}
}

// Reconcile 让系统规则回到配置状态：规则被删、新接入网卡都在这里恢复。
// TODO(verify:V17) 手动删规则也会被恢复，要确认这符合使用习惯。
func (e *Engine) Reconcile(ctx context.Context) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	defer e.trackBlocked()
	cfg := e.config()
	v := e.liveVerdict()

	ifaces, err := e.backend.Interfaces(ctx)
	if err != nil {
		e.fail("读取网卡失败：" + err.Error())
		return
	}
	actual, d, err := e.inspectFor(ctx, cfg, v.Path, ifaces)
	if err != nil {
		e.fail("读取系统规则失败：" + err.Error())
		return
	}

	e.mu.Lock()
	switched := cfg.Enabled && e.verdict.Path != v.Path
	e.mu.Unlock()

	switch {
	case cfg.Enabled && v.Path == PathDirect && actual.Installed:
		if err := e.backend.Remove(ctx); err != nil {
			e.fail("撤销规则失败：" + err.Error())
			e.setRules(ifaces, actual)
			return
		}
		if !switched {
			e.event("warn", "受信网络上出现了规则，已撤销")
		}
	case cfg.Enabled && v.Path != PathDirect && !actual.InSync:
		if err := e.applyDesired(ctx, d); err != nil {
			e.fail("写入规则失败：" + err.Error())
			e.setRules(ifaces, actual)
			return
		}
		if !switched {
			e.event("warn", fmt.Sprintf("系统规则与配置不一致，已恢复（%v）", actual.Diffs))
		}
	case !cfg.Enabled && actual.Installed:
		if err := e.backend.Remove(ctx); err != nil {
			e.fail("撤销规则失败：" + err.Error())
			return
		}
		e.event("info", "保护已关闭，已撤销残留规则")
	}
	if actual, _, err = e.inspectFor(ctx, cfg, v.Path, ifaces); err != nil {
		e.fail("读取系统规则失败：" + err.Error())
		return
	}

	e.mu.Lock()
	e.lastErr = ""
	e.verdict = v
	e.mu.Unlock()
	if switched {
		e.event(pathEvent(v))
		// 换路之前解析到的 IP 不该继续使用：比如离开受信网络后，缓存里的真实 IP 会绕过 DNS 层。
		if err := e.backend.FlushDNS(ctx); err != nil {
			e.event("warn", "清理 DNS 缓存失败："+err.Error())
		}
	}
	e.setRules(ifaces, actual)
	// 换网络时 Kick 走到这里：DNS 拦截跟着受信网络立即切换，不等下一轮健康检查。
	e.mu.Lock()
	health, probed := e.health, e.probed
	e.mu.Unlock()
	if probed {
		e.syncGuard(ctx, health)
	}
	e.setRuleChecks(checkRules(ctx, cfg))
}

// ProbeOnce 探测 Mihomo 通路并记录状态切换。
func (e *Engine) ProbeOnce(ctx context.Context) {
	defer e.trackBlocked() // 每次健康检查都看一次：短暂阻断要等持续一段时间才通知
	h := e.probe(ctx)
	e.mu.Lock()
	prev, first := e.health, !e.probed
	e.health, e.probed = h, true
	enabled := e.cfg.Enabled
	applied := e.verdict.Path
	e.mu.Unlock()
	e.syncGuard(ctx, h)
	// TUN 接管与否会改变 Policy 的判断。
	if enabled && e.policy.Verdict(h.TUNOK).Path != applied {
		e.Kick()
	}
	// 国内域名第一次检查就失败也要提示：这时整机多半上不了网，而保护状态看起来一切正常。
	if (first || prev.DomesticOK) && !h.DomesticOK {
		e.event("warn", h.DomesticDetail)
	}
	if !first && !prev.DomesticOK && h.DomesticOK {
		e.event("info", "国内域名解析已恢复")
	}
	if first {
		return
	}
	if prev.DNSOK && !h.DNSOK {
		e.event("warn", h.DNSDetail)
		// 应用拿到的是真实 IP：不清缓存的话，缓存有效期内还能绕过 DNS 层直连。
		if enabled {
			if err := e.backend.FlushDNS(ctx); err != nil {
				e.event("warn", "清理 DNS 缓存失败："+err.Error())
			}
		}
	}
	if !prev.DNSOK && h.DNSOK {
		e.event("info", "Mihomo DNS 已恢复")
		// 系统可能缓存了解析失败的结果，不清掉的话恢复后还要等一会儿。
		if enabled {
			if err := e.backend.FlushDNS(ctx); err != nil {
				e.event("warn", "清理 DNS 缓存失败："+err.Error())
			}
		}
	}
	if prev.TUNOK != h.TUNOK {
		if h.TUNOK {
			e.event("info", "TUN 已接管流量（"+h.Interface+"）")
		} else {
			e.event("warn", "TUN 未接管流量（当前出口："+h.Interface+"）")
		}
	}
}

func (e *Engine) probe(ctx context.Context) Health {
	cfg := e.config()
	e.mu.Lock()
	ifaces := slices.Clone(e.ifaces)
	e.mu.Unlock()
	d := cfg.desired(ifaces)
	suffix := ""
	if len(d.Suffixes) > 0 {
		suffix = d.Suffixes[0]
	}
	h := e.prober.Probe(ctx, suffix, cfg.DNSListen, probeTarget(d), ifaces)
	// 只看出口不够：其他 VPN 的网卡也不是物理网卡，会被当成 TUN 已接管。
	if h.TUNOK && cfg.Controller != "" && !tunListening(ctx, cfg) {
		h.TUNOK = false
	}
	return h
}

// Enable 启用保护。没有 Mihomo 通路时默认拒绝，否则会切断受保护的服务。
func (e *Engine) Enable(ctx context.Context, force bool) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	cfg := e.config()
	ifaces, err := e.backend.Interfaces(ctx)
	if err != nil {
		return err
	}
	d := cfg.desired(ifaces)
	if len(d.Suffixes) == 0 && len(d.CIDRs) == 0 {
		return errors.New("没有启用的服务")
	}
	if err := e.backend.ValidateListen(d.ListenIP, d.ListenPort); err != nil {
		return err
	}
	e.mu.Lock()
	e.ifaces = ifaces
	e.mu.Unlock()
	h := e.probe(ctx)
	v := e.policy.Verdict(h.TUNOK)
	if !force {
		switch v.Path {
		case PathHold:
			return &NotReadyError{Reason: v.Reason + "。现在启用会立即阻断受保护的服务。"}
		case PathProxy:
			if !h.DNSOK {
				return &NotReadyError{Reason: h.DNSDetail + "。请先在概览打开代理。"}
			}
			if !h.TUNOK {
				return &NotReadyError{Reason: "TUN 没有接管流量（当前出口：" + h.Interface + "）。现在启用会切断受保护的服务。"}
			}
		}
	}
	if v.Path != PathDirect {
		if err := e.applyDesired(ctx, cfg.desiredFor(v.Path, e.holdListen, ifaces)); err != nil {
			_ = e.backend.Remove(ctx)
			return fmt.Errorf("写入规则失败，已撤销：%w", err)
		}
	}
	cfg.Enabled = true
	if err := e.store(cfg); err != nil {
		_ = e.backend.Remove(ctx)
		return err
	}
	e.mu.Lock()
	e.verdict = v
	e.mu.Unlock()
	e.event("info", "保护已启用")
	e.event(pathEvent(v))
	e.refresh(ctx, cfg, ifaces)
	return nil
}

// Disable 关闭保护并撤销规则。Mihomo 不在时也能执行。
func (e *Engine) Disable(ctx context.Context) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := e.backend.Remove(ctx); err != nil {
		return err
	}
	cfg := e.config()
	cfg.Enabled = false
	if err := e.store(cfg); err != nil {
		return err
	}
	e.event("info", "保护已关闭")
	ifaces, err := e.backend.Interfaces(ctx)
	if err != nil {
		return err
	}
	e.refresh(ctx, cfg, ifaces)
	return nil
}

// UpdateConfig 保存配置。启用状态下立即应用；失败时恢复原配置，不留半套规则。
func (e *Engine) UpdateConfig(ctx context.Context, next Config) error {
	if err := next.normalize(); err != nil {
		return &ValidationError{err.Error()}
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	cur := e.config()
	next.Enabled = cur.Enabled
	ifaces, err := e.backend.Interfaces(ctx)
	if err != nil {
		return err
	}
	d := next.desired(ifaces)
	if err := e.backend.ValidateListen(d.ListenIP, d.ListenPort); err != nil {
		return &ValidationError{err.Error()}
	}
	e.mu.Lock()
	p := e.verdict.Path
	e.mu.Unlock()
	if next.Enabled && p != PathDirect {
		if err := e.applyDesired(ctx, next.desiredFor(p, e.holdListen, ifaces)); err != nil {
			if undo := e.applyDesired(ctx, cur.desiredFor(p, e.holdListen, ifaces)); undo != nil {
				e.event("error", "恢复原规则失败："+undo.Error())
			}
			return fmt.Errorf("应用配置失败，已恢复原配置：%w", err)
		}
	}
	if err := e.store(next); err != nil {
		return err
	}
	e.event("info", "配置已保存")
	e.refresh(ctx, next, ifaces)
	return nil
}

func (e *Engine) applyDesired(ctx context.Context, d Desired) error {
	if len(d.CIDRs) > 0 && len(d.Interfaces) == 0 {
		return errNoInterfaces
	}
	return e.backend.Apply(ctx, d)
}

func (e *Engine) refresh(ctx context.Context, cfg Config, ifaces []string) {
	e.mu.Lock()
	p := e.verdict.Path
	e.mu.Unlock()
	actual, _, err := e.inspectFor(ctx, cfg, p, ifaces)
	if err != nil {
		e.fail("读取系统规则失败：" + err.Error())
		return
	}
	e.mu.Lock()
	e.lastErr = ""
	e.mu.Unlock()
	e.setRules(ifaces, actual)
	e.setRuleChecks(checkRules(ctx, cfg))
	e.ProbeOnce(ctx)
}

func (e *Engine) Status() Status {
	live := e.liveVerdict()
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.verdict
	if live.Path == v.Path {
		v.Reason, v.Pending = live.Reason, live.Pending // 同一条路上原因会变，比如「正在探测出口」变成「探测失败」
	}
	s := Status{
		Enabled:   e.cfg.Enabled,
		Verdict:   v,
		DNSGuard:  e.guardState,
		Health:    e.health,
		Rules:     e.rules,
		Desired:   e.cfg.desiredFor(v.Path, e.holdListen, e.ifaces),
		LastError: e.lastErr,
		// 列表一律给 []，不给 null：托盘外壳按列表解析，null 会让它把整个状态当成连不上服务。
		RuleChecks: append([]RuleCheck{}, e.ruleChecks...),
		Events:     append([]Event{}, e.events...),
		Notices:    append([]Notice{}, e.notices.list...),
	}
	if s.Rules.Warnings == nil {
		s.Rules.Warnings = []string{}
	}
	slices.Reverse(s.Events)
	s.State = computeState(s)
	return s
}

func computeState(s Status) string {
	switch {
	case !s.Enabled:
		return StateOff
	case s.LastError != "" || !s.Rules.InSync:
		return StateError
	case s.Verdict.Path == PathDirect:
		return StateDirect
	case s.Verdict.Path == PathHold:
		return StateHeld
	case !s.Health.DNSOK:
		return StateBlocking
	case !s.Health.TUNOK:
		return StateProxyOnly
	default:
		return StateProtecting
	}
}

func (e *Engine) Config() Config { return e.config() }

func (e *Engine) config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneConfig(e.cfg)
}

func cloneConfig(c Config) Config {
	c.Services = slices.Clone(c.Services)
	for i := range c.Services {
		c.Services[i].Suffixes = slices.Clone(c.Services[i].Suffixes)
		c.Services[i].CIDRs = slices.Clone(c.Services[i].CIDRs)
	}
	return c
}

func (e *Engine) store(c Config) error {
	if err := saveConfig(e.path, c); err != nil {
		return fmt.Errorf("保存配置失败：%w", err)
	}
	e.mu.Lock()
	e.cfg = c
	e.mu.Unlock()
	return nil
}

func (e *Engine) setRules(ifaces []string, a Actual) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ifaces, e.rules = ifaces, a
}

func (e *Engine) setRuleChecks(c []RuleCheck) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ruleChecks = c
}

func (e *Engine) fail(msg string) {
	e.mu.Lock()
	changed := e.lastErr != msg
	e.lastErr = msg
	e.mu.Unlock()
	if changed {
		e.event("error", msg)
	}
}

func (e *Engine) event(level, msg string) {
	if e.log != nil {
		e.log.Info(msg, "kind", level)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, Event{Time: time.Now().UTC(), Level: level, Message: msg})
	if len(e.events) > maxEvents {
		e.events = e.events[len(e.events)-maxEvents:]
	}
}

// ValidationError 是用户输入错误，API 返回 400。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
