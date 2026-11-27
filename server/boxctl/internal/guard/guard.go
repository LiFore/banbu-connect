// Package guard 是 proxy-guard 守护进程：维护独立的 nftables 保护规则，
// 与内核和 panel 的生命周期解耦（ADR-002、ADR-032）。
//
// 规则常驻：受保护服务的地址（固定网段 + dnsmasq 解析时写入的地址）不允许从局域网或本机直连，
// 只能经内核的代理出去。内核、panel 停止或崩溃时规则仍在，所以不需要健康状态机来切换放行/阻断；
// guard 只负责把规则装上、保持住（每 5 秒自检，fw4 reload 后自动恢复），并按 panel 的要求阻断（hold）。
package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"boxctl/internal/buildinfo"
	"boxctl/internal/ipc"
	"boxctl/internal/logring"
)

// State 是保护状态。
type State string

const (
	StateInit    State = "init"    // 尚未装上规则
	StateBlocked State = "blocked" // 规则已装，受保护服务被阻断（hold）
	StateHealthy State = "healthy" // 规则已装，受保护服务只能经代理访问
	StateError   State = "error"   // 装规则失败
)

// IPC 方法名。
const (
	MethodStatus      = "status"
	MethodApplyPolicy = "apply_policy"
	MethodLogs        = "logs"    // 参数 {"level": "WARN"}，返回 []logring.Entry
	MethodRuleset     = "ruleset" // 返回系统上 guard 表的内容（诊断包用）
)

const (
	checkPeriod = 5 * time.Second
	// sysprepEvery 是每多少次自检检查一次系统设置（约 1 分钟）。
	sysprepEvery = 12
	maxEvents    = 30
	policyFile   = "guard-policy.json"
)

// Event 是一条保护事件，显示在界面上。
type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // info / warn / error
	Message string    `json:"message"`
}

// Status 是 status 方法的返回值，panel 侧按此结构解码。
type Status struct {
	State         State     `json:"state"`
	PolicyVersion string    `json:"policy_version"`
	Since         time.Time `json:"since"`
	Version       string    `json:"version"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	Installed     bool      `json:"installed"`
	Hold          bool      `json:"hold"`
	Reason        string    `json:"reason,omitempty"`
	Domains       int       `json:"domains"`
	CIDRs         int       `json:"cidrs"`
	LANDevice     string    `json:"lan_device"`
	LastError     string    `json:"last_error,omitempty"`
	LastCheck     time.Time `json:"last_check"`
	Fake          bool      `json:"fake"`
	Events        []Event   `json:"events"`
	// Core 是 guard 每 500 毫秒对内核的检查（ADR-045）。
	Core CoreHealth `json:"core"`
	// Counters 是各条链丢弃的包数（nft 计数器），键为链名。
	Counters map[string]uint64 `json:"counters,omitempty"`
	// EncryptedDNS 是正在拦截加密 DNS（DoT、DoQ、知名 DoH 地址）。
	EncryptedDNS bool `json:"encrypted_dns"`
	// Checks 是规则自检的次数；Watching 是 nft monitor 在监听表被删。
	Checks   int  `json:"checks"`
	Watching bool `json:"watching"`
	// SavedSets、SavedAt 是最近一次保存或恢复的严格集合地址数和时间。
	SavedSets int       `json:"saved_sets"`
	SavedAt   time.Time `json:"saved_at,omitempty"`
}

type Options struct {
	Socket  string
	DataDir string // 策略文件所在目录
	LANDev  string // 局域网网桥，如 br-lan
	Fake    bool   // 不改动系统，用于开发
	Logger  *slog.Logger
	Logs    *logring.Ring // 为 nil 时 logs 方法返回空
	// CoreSecretFile 是内核控制接口的密钥（面板生成）；为空时用 DataDir/core-secret。
	CoreSecretFile string
}

// Engine 维护规则。
type Engine struct {
	sys     System
	uci     UCI // 为 nil 时不检查系统设置（开发模式）
	dataDir string
	path    string
	lanDev  string
	fake    bool
	log     *slog.Logger

	// instMu 串行化装规则：面板下发、5 秒自检、500 毫秒内核检查都会装，后装的必须是最新的策略。
	instMu sync.Mutex

	mu     sync.Mutex
	policy Policy
	status Status
	events []Event
	checks int
	start  time.Time
	// savedSets 是最近一次保存或恢复的严格集合内容摘要，内容没变时不重复写盘。
	savedSets string
	savedN    int
	savedAt   time.Time
	watching  bool

	// 内核检查（coremon.go）
	core       CoreHealth
	coreDown   bool // 正在因内核异常阻断
	okStreak   int
	coreProbe  coreProber
	secretFile string
}

func NewEngine(sys System, dataDir, lanDev string, fake bool, log *slog.Logger) *Engine {
	return &Engine{sys: sys, dataDir: dataDir, path: filepath.Join(dataDir, policyFile), lanDev: lanDev, fake: fake, log: log, start: time.Now().UTC(),
		status: Status{State: StateInit, Since: time.Now().UTC(), Version: buildinfo.Version}}
}

// Start 加载上一份已知可用的策略并装上（M9-02）。没有时装一份空策略：表和 IPv6 规则先就位。
func (e *Engine) Start(ctx context.Context) {
	p, ok := loadPolicy(e.path)
	if !ok {
		p, _ = Policy{}.Normalize()
	}
	e.mu.Lock()
	e.policy = p
	e.mu.Unlock()
	if ok {
		e.record("info", fmt.Sprintf("已加载上次的策略（%d 个域名、%d 个网段）", len(p.Suffixes), len(p.CIDRs)))
	}
	e.mu.Lock()
	eff := e.effectiveLocked(p)
	e.mu.Unlock()
	if e.install(ctx, eff, p.Version()) == nil {
		e.restoreSets(ctx)
	}
	e.prepare(ctx)
}

// SetUCI 设置读写系统设置的接口，在 Start 之前调用。
func (e *Engine) SetUCI(u UCI) { e.uci = u }

// prepare 确保流量卸载关闭、局域网不下发 IPv6 地址（M9-06、M9-08）。
func (e *Engine) prepare(ctx context.Context) {
	if e.uci == nil {
		return
	}
	changed, err := prepareSystem(ctx, e.uci, e.dataDir)
	for _, c := range changed {
		e.record("warn", "已修改系统设置："+c)
	}
	if err != nil {
		e.record("error", "修改系统设置失败："+err.Error())
	}
}

// Apply 应用 panel 下发的策略并保存。
func (e *Engine) Apply(ctx context.Context, in Policy) error {
	p, err := in.Normalize()
	if err != nil {
		return err
	}
	e.instMu.Lock()
	e.mu.Lock()
	same := p.Version() == e.policy.Version() && e.status.Installed
	prev := e.policy
	eff := e.effectiveLocked(p)
	e.mu.Unlock()
	if same {
		e.instMu.Unlock()
		return nil
	}
	if err := e.install(ctx, eff, p.Version()); err != nil {
		e.instMu.Unlock()
		return err
	}
	e.mu.Lock()
	e.policy = p
	e.mu.Unlock()
	e.instMu.Unlock()
	if err := savePolicy(e.path, p); err != nil {
		e.record("warn", "保存策略失败："+err.Error())
	}
	switch {
	case p.Hold && !prev.Hold:
		msg := "已阻断受保护的服务"
		if p.Reason != "" {
			msg += "：" + p.Reason
		}
		e.record("warn", msg)
	case !p.Hold && prev.Hold:
		e.record("info", "已解除阻断")
	case p.ForwardAll != prev.ForwardAll && slices.Equal(p.Suffixes, prev.Suffixes) && slices.Equal(p.CIDRs, prev.CIDRs):
		if p.ForwardAll {
			e.record("info", "内核已启动，局域网的 DNS 交给内核解析")
		} else {
			e.record("info", "内核已停止，局域网的 DNS 回到原来的上游；受保护的服务继续被拦截")
		}
	default:
		e.record("info", fmt.Sprintf("策略已更新（%d 个域名、%d 个网段）", len(p.Suffixes), len(p.CIDRs)))
	}
	return nil
}

// effectiveLocked 是实际装上的策略：面板的策略加上 guard 自己对内核的判断（内核异常时阻断）。
func (e *Engine) effectiveLocked(p Policy) Policy {
	p.CoreDown = false
	if e.coreDown {
		p.CoreDown = true
		if !p.Hold {
			p.Hold, p.Reason = true, e.core.Reason
		}
	}
	return p
}

// install 装上实际策略 p；base 是面板策略的版本（状态里显示，面板据此判断是否同步）。
func (e *Engine) install(ctx context.Context, p Policy, base string) error {
	err := e.sys.Apply(ctx, Ruleset(p, e.lanDev))
	if err == nil {
		if derr := e.sys.WriteDnsmasq(ctx, DnsmasqConf(p)); derr != nil {
			// nft 规则已生效，受保护的地址仍然被拦；只是新解析出的地址进不了集合。
			e.record("error", "写入 dnsmasq 配置失败："+derr.Error())
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.status.State
	e.status.LastCheck = time.Now().UTC()
	if err != nil {
		e.status.State, e.status.Installed, e.status.LastError = StateError, false, err.Error()
	} else {
		e.status.Installed, e.status.LastError = true, ""
		e.status.State = StateHealthy
		if p.Hold {
			e.status.State = StateBlocked
		}
		e.status.PolicyVersion, e.status.Hold, e.status.Reason = base, p.Hold, p.Reason
		e.status.EncryptedDNS = len(p.Suffixes) > 0 && iifMatch(e.lanDev) != ""
		e.status.Domains, e.status.CIDRs = len(p.Suffixes), len(p.CIDRs)
	}
	if prev != e.status.State {
		e.status.Since = time.Now().UTC()
	}
	if err != nil {
		e.log.Error("装规则失败", "err", err)
		e.events = appendEvent(e.events, Event{time.Now().UTC(), "error", "装规则失败：" + err.Error()})
	}
	return err
}

// Check 自检：规则缺失或不是当前这份时重装并记事件（M9-07）。
func (e *Engine) Check(ctx context.Context) {
	e.instMu.Lock()
	defer e.instMu.Unlock()
	e.mu.Lock()
	p := e.policy
	eff := e.effectiveLocked(p)
	e.checks++
	sysprep := e.checks%sysprepEvery == 0
	save := e.checks%saveEvery == 0
	e.mu.Unlock()
	if sysprep {
		e.prepare(ctx)
	}
	if save {
		e.saveSets(ctx)
	}
	ok, err := e.sys.Installed(ctx, eff.Version())
	if err == nil && ok {
		e.mu.Lock()
		e.status.LastCheck = time.Now().UTC()
		e.mu.Unlock()
		return
	}
	if e.install(ctx, eff, p.Version()) == nil {
		e.record("warn", "保护规则缺失（可能是防火墙重载），已恢复")
	}
}

func (e *Engine) record(level, msg string) {
	e.log.Info("guard event", "level", level, "msg", msg)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = appendEvent(e.events, Event{time.Now().UTC(), level, msg})
}

func appendEvent(list []Event, ev Event) []Event {
	list = append(list, ev)
	if len(list) > maxEvents {
		list = list[len(list)-maxEvents:]
	}
	return list
}

// Status 返回当前状态，事件按时间倒序。
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.status
	st.LANDevice, st.Fake = e.lanDev, e.fake
	st.PID, st.StartedAt = os.Getpid(), e.start
	st.Core, st.Checks, st.Watching = e.core, e.checks, e.watching
	st.SavedSets, st.SavedAt = e.savedN, e.savedAt
	st.Events = make([]Event, 0, len(e.events))
	for i := len(e.events) - 1; i >= 0; i-- {
		st.Events = append(st.Events, e.events[i])
	}
	return st
}

// Run 运行 guard：装规则、定期自检、提供 IPC。
func Run(ctx context.Context, opts Options) error {
	var sys System = NewSystem()
	fake := opts.Fake || runtime.GOOS != "linux"
	if fake {
		sys = &FakeSystem{}
	}
	e := NewEngine(sys, opts.DataDir, opts.LANDev, fake, opts.Logger)
	if !fake {
		e.SetUCI(execUCI{})
	}
	secret := opts.CoreSecretFile
	if secret == "" {
		secret = filepath.Join(opts.DataDir, "core-secret")
	}
	e.coreProbe = httpCoreProbe(secret, fake)
	e.Start(ctx)
	go e.RunCoreMonitor(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(checkPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				// 关机、重启服务时保存严格集合，开机后立即写回。
				sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				e.saveSets(sctx)
				cancel()
				return
			case <-t.C:
				e.Check(ctx)
			}
		}
	}()
	if w, ok := sys.(watcher); ok {
		e.mu.Lock()
		e.watching = true
		e.mu.Unlock()
		// 表或链被删（fw4 restart、其他插件 nft flush ruleset）时立即恢复，不等下一次自检。
		go w.Watch(ctx, func() { e.Check(ctx) })
	}

	srv := &ipc.Server{
		Path:   opts.Socket,
		Logger: opts.Logger,
		Handlers: map[string]ipc.Handler{
			MethodStatus: func(ctx context.Context, _ json.RawMessage) (any, error) {
				st := e.Status()
				if c, ok := sys.(counterReader); ok {
					st.Counters = c.Counters(ctx)
				}
				return st, nil
			},
			MethodLogs: func(_ context.Context, params json.RawMessage) (any, error) {
				var in struct {
					Level string `json:"level"`
				}
				_ = json.Unmarshal(params, &in)
				if opts.Logs == nil {
					return []logring.Entry{}, nil
				}
				return opts.Logs.Entries(in.Level), nil
			},
			MethodRuleset: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return sys.Dump(ctx), nil
			},
			MethodApplyPolicy: func(ctx context.Context, params json.RawMessage) (any, error) {
				var p Policy
				if err := json.Unmarshal(params, &p); err != nil {
					return nil, fmt.Errorf("策略格式无效：%w", err)
				}
				if err := e.Apply(ctx, p); err != nil {
					return nil, err
				}
				return e.Status(), nil
			},
		},
	}
	opts.Logger.Info("guard started", "socket", opts.Socket, "lan", opts.LANDev, "fake", fake, "version", buildinfo.String())
	err := srv.Serve(ctx)
	if ctx.Err() != nil {
		// 等集合保存完再退出（procd 停服务时给的时间足够）。
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
		}
	}
	return err
}

// Uninstall 删除规则和 dnsmasq 片段，恢复改过的系统设置（`boxctl guard -uninstall`，卸载包时调用）。
func Uninstall(ctx context.Context, dataDir string) error {
	return errors.Join(NewSystem().Remove(ctx), restoreSystem(ctx, execUCI{}, dataDir))
}
