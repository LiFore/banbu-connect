package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"boxctl/internal/dist"
	"boxctl/internal/gen"
	"boxctl/internal/groups"
	"boxctl/internal/killswitch"
	"boxctl/internal/nodes"
	"boxctl/internal/trust"
)

const (
	configFile      = "config.yaml"
	candidateFile   = "candidate.yaml"
	coreLogFile     = "core.log"
	ControllerAddr  = "127.0.0.1:19190" // 桌面客户端的默认值；路由盒用 SetPorts 换成自己的
	strictProbeBase = 19200             // 严格组探测入口的起始端口，只监听本机
	applyDebounce   = 800 * time.Millisecond
	restartBackoff  = 3 * time.Second
	stopGrace       = 5 * time.Second
	validateTimeout = 3 * time.Minute // 第一次校验要下载 geo 数据
	tunCheckPeriod  = 5 * time.Second
	lastGoodFile    = "last-good.yaml"
	confirmTimeout  = 8 * time.Second  // 单次探测
	baselineWait    = 10 * time.Second // 变更前的基线探测
)

// 应用后健康确认（M8-06）：60 秒内回滚。探测从应用后 confirmFirst 开始，每 confirmEvery 一次，
// 到 confirmWindow 仍没有一次成功就判定失败。测试里调短。
var (
	confirmFirst  = 3 * time.Second
	confirmEvery  = 5 * time.Second
	confirmWindow = 45 * time.Second
)

// ConfirmState 是一份新配置应用后的确认结果。
type ConfirmState string

const (
	ConfirmPending    ConfirmState = "pending"     // 正在确认
	ConfirmOK         ConfirmState = "confirmed"   // 出口通
	ConfirmUpstream   ConfirmState = "unconfirmed" // 不通，但变更前也不通：判断为上游故障，不回滚（M8-07）
	ConfirmRolledBack ConfirmState = "rolled_back" // 不通且变更前是通的：已回滚到上一份确认过的配置
	ConfirmFailed     ConfirmState = "failed"      // 不通、应该回滚，但没有可回滚的配置或回滚失败
)

// ConfirmEvent 报告一份配置（按内容摘要标识）的确认结果。
type ConfirmEvent struct {
	Hash   string
	State  ConfirmState
	Reason string
}

// CoreStatus 是内核的运行状态。
type CoreStatus struct {
	Running   bool      `json:"running"`
	Desired   bool      `json:"desired"`
	Mode      string    `json:"mode"`
	TUN       bool      `json:"tun"`        // 设置值
	TUNActive bool      `json:"tun_active"` // 内核里 TUN 实际在运行
	TUNError  string    `json:"tun_error"`  // TUN 启动失败的原因；内核此时继续运行
	MixedPort int       `json:"mixed_port"`
	NodeCount int       `json:"node_count"`
	LastError string    `json:"last_error"`
	Warnings  []string  `json:"warnings"`
	AppliedAt time.Time `json:"applied_at"`
	Applying  bool      `json:"applying"`
	// ConfigHash 是正在运行的配置摘要；Confirm 是它的确认状态（只有设置了 Confirm 的平台有）。
	ConfigHash    string       `json:"config_hash,omitempty"`
	Confirm       ConfirmState `json:"confirm,omitempty"`
	ConfirmReason string       `json:"confirm_reason,omitempty"`
}

// Policy 是生成配置时由平台决定的部分。
type Policy struct {
	Services  []killswitch.Service // 受保护的服务，生成严格规则
	DNSListen string
	// LANDNS 是局域网 DNS 转发口（只用于桌面），见 gen.Input.LANDNS。
	LANDNS string
	// LocalDNS 是内网域名专用的上游，见 gen.Input.LocalDNS。
	LocalDNS []string
	// ClientProxy 见 gen.Input.ClientProxy（路由盒给客户端的代理入口）。
	ClientProxy *gen.ProxyEndpoint
	// Box 见 gen.Input.Box（桌面在路由盒网络上）。
	Box *gen.ProxyEndpoint
	// StrictRoute 只对桌面 TUN 有意义。
	StrictRoute bool
	// Router 为 true 时按路由盒生成：TUN 接管局域网（auto-redirect），DNS 由 dnsmasq 转发进来。
	Router bool
	// RouteExclude 是路由盒 TUN 不接管的目标网段（局域网、管理面）。
	RouteExclude []string
	// LANAllowedIPs 是可以连代理端口的局域网网段（只用于路由盒），设置里打开局域网代理时生效。
	LANAllowedIPs []string
	// Gate 不为空时受保护服务经出口闸门（只用于桌面）。
	Gate     *gen.GateInput
	Warnings []string
	// 局域网设备策略（只用于路由盒）：出口规则、自定义子规则、不经内核的设备 MAC。
	DeviceRules []string
	SubRules    map[string][]string
	ExcludeMAC  []string
}

// Hooks 是平台注入的行为。
type Hooks struct {
	// Policy 按当前设置返回生成配置用的平台部分。必须设置。
	Policy func(Settings) Policy
	// OnApplied 在新配置生效后调用；reloaded 表示是热重载（内核原来就在运行）。
	OnApplied func(ctx context.Context, reloaded bool)
	// Confirm 探测出口是否通。设置后每次应用新配置都要在 60 秒内确认，变更引入的失败自动回滚（M8-06、M8-07）。
	Confirm func(ctx context.Context) error
	// OnConfirm 报告确认结果。
	OnConfirm func(ConfirmEvent)
	// BeforeStop 在用户停止内核、真正停之前调用（设置已保存为不运行）：
	// 路由盒先告诉 guard 内核是用户停的，免得 guard 看到内核离线就阻断局域网。
	BeforeStop func(ctx context.Context)
}

// ConfigHash 是配置内容的摘要，用来关联配置版本。
func ConfigHash(yaml []byte) string {
	if len(yaml) == 0 {
		return ""
	}
	sum := sha256.Sum256(yaml)
	return hex.EncodeToString(sum[:8])
}

// Data 是生成配置用的数据。
type Data struct {
	DB     *sql.DB
	Nodes  *nodes.Service
	Groups *groups.Service
}

// Supervisor 负责生成配置、校验、运行内核和热重载。
type Supervisor struct {
	// paused 是客户端「退出」后的暂停（Pause）：内核停着但设置不变，不落盘，服务重启即恢复。
	paused   atomic.Bool
	data     Data
	settings Store
	hooks    Hooks
	self     string
	coreDir  string
	secret   string
	log      *slog.Logger
	// Dist 是管理服务下发的配置；为 nil 或没有绑定时只用本地配置。在 Run 之前设置。
	Dist *dist.Client
	// OnLines 在线路可能换了出口时调用：内核换了配置、切换了线路成员。在 Run 之前设置。
	OnLines func()

	controller string
	probeBase  int

	trigger chan struct{}
	opMu    sync.Mutex // 串行化生成、校验和进程操作

	mu      sync.Mutex
	child   *exec.Cmd
	childAt time.Time // 内核进程的启动时间
	stdin   io.WriteCloser
	exited  chan struct{}
	applied []byte
	probes  map[string]int // 当前配置里严格组的探测入口端口
	routes  map[string]int // 当前配置里每个代理组（和 DIRECT）的线路探测入口端口
	gateDir int            // 当前配置里内核 DIRECT 入口的端口；没开闸门时为 0
	// boxRoutes 是当前配置里「路由盒切换」选择组 → 原目标（ADR-073）；没开闸门时为空。
	boxRoutes map[string]string
	holdKey   string // 最近一次成功下发的 hold 开关（内核 PID + 配置摘要 + 状态），相同时不重复下发
	holdOn    bool   // 内核里的 hold 副本当前是否打开

	// 路由盒：内核是 procd 的独立服务（UseService）；svc 为 nil 时内核是面板的子进程（桌面）。
	svc      *coreService
	online   bool          // 独立服务：最近一次检查控制接口能访问
	health   ServiceHealth // 独立服务：最近一次检查的结果
	logPath  string        // 内核日志，读 TUN 启动失败的原因
	coreGen  int           // 每次内核进程上线加一：新进程里的 hold 副本默认生效，要重新下发
	status   CoreStatus
	stopping bool

	confirmGen int    // 每次应用加一；确认任务据此判断自己是否已过时
	lastGood   []byte // 最近一份确认过的配置，回滚目标
	rejected   string // 被回滚的配置摘要：内容不变时不再应用
	rejectMsg  string
}

func NewSupervisor(data Data, settings Store, hooks Hooks, self, coreDir, secret string, log *slog.Logger) *Supervisor {
	s := &Supervisor{data: data, settings: settings, hooks: hooks, self: self, coreDir: coreDir, secret: secret, log: log,
		controller: ControllerAddr, probeBase: strictProbeBase, trigger: make(chan struct{}, 1),
		logPath: filepath.Join(coreDir, coreLogFile)}
	if hooks.Confirm != nil {
		s.lastGood, _ = os.ReadFile(filepath.Join(coreDir, lastGoodFile))
	}
	return s
}

// SetPorts 换内核控制接口和严格组探测入口的端口，在 Run 之前调用。
// 路由盒与桌面客户端用不同的端口，两者在同一台机器上开发时不冲突。
func (s *Supervisor) SetPorts(controller string, probeBase int) {
	s.controller, s.probeBase = controller, probeBase
}

// NewTestSupervisor 只用于测试中间件：没有数据，不能 Run。
func NewTestSupervisor() *Supervisor { return &Supervisor{trigger: make(chan struct{}, 1)} }

// Triggered 读走一次待处理的触发，用于测试。
func (s *Supervisor) Triggered() bool {
	select {
	case <-s.trigger:
		return true
	default:
		return false
	}
}

// Settings 返回设置存储。
func (s *Supervisor) Settings() Store { return s.settings }

// ControllerURL 是内核控制接口的地址。
func (s *Supervisor) ControllerURL() string { return "http://" + s.controller }

// LinesChanged 报告线路可能换了出口，严格线路要重新确认。
func (s *Supervisor) LinesChanged() {
	if s.OnLines != nil {
		s.OnLines()
	}
}

// Trigger 请求重新生成配置；连续的修改合并成一次。
func (s *Supervisor) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run 处理配置变化，并在需要时启动内核。ctx 结束时停止内核。
func (s *Supervisor) Run(ctx context.Context) {
	if s.svc != nil {
		// 先看一次内核是不是已经在运行（面板重启、升级后接管），再决定要不要应用配置。
		s.checkService(ctx)
		go s.watchService(ctx)
	}
	s.RefreshStatus(ctx)
	settings, err := s.settings.Load(ctx)
	if err == nil && settings.Running {
		s.Trigger()
	}
	tunCheck := time.NewTicker(tunCheckPeriod)
	defer tunCheck.Stop()
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-tunCheck.C:
			s.refreshTUN(ctx)
		case <-s.trigger:
			select {
			case <-time.After(applyDebounce):
			case <-ctx.Done():
				s.shutdown()
				return
			}
			if err := s.Apply(ctx); err != nil {
				s.log.Warn("apply failed", "err", err)
			}
		}
	}
}

// Apply 让内核跑上最新配置。校验失败时当前配置保持不动。
func (s *Supervisor) Apply(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.setApplying(true)
	defer s.setApplying(false)

	err := s.apply(ctx)
	s.mu.Lock()
	if err != nil {
		s.status.LastError = err.Error()
	} else {
		s.status.LastError = ""
	}
	s.mu.Unlock()
	s.RefreshStatus(ctx)
	s.refreshTUN(ctx)
	return err
}

func (s *Supervisor) apply(ctx context.Context) error {
	settings, err := s.settings.Load(ctx)
	if err != nil {
		return err
	}
	if !settings.Running || s.paused.Load() {
		return nil
	}
	nodes, err := s.data.Nodes.Export(ctx)
	if err != nil {
		return err
	}
	groups, err := s.data.Groups.Export(ctx)
	if err != nil {
		return err
	}
	rules := settings.CustomRules
	var ruleSets []gen.RuleSet // nil：用内置默认
	var distWarnings []string
	profile, managed := s.distProfile()
	if managed {
		m := dist.Merge(profile, nodes, groups, rules)
		nodes, groups, rules, ruleSets, distWarnings = m.Nodes, m.Groups, m.Rules, m.RuleSets, m.Warnings
	}
	policy := s.hooks.Policy(settings)
	result, err := gen.Build(gen.Input{
		Nodes: nodes, Groups: groups, Services: policy.Services, CustomRules: rules, RuleSets: ruleSets,
		DNSListen: policy.DNSListen, LANDNS: policy.LANDNS, LocalDNS: policy.LocalDNS, Mode: settings.Mode, TUN: settings.TUN || policy.Router, StrictRoute: policy.StrictRoute,
		Router: policy.Router, RouteExclude: policy.RouteExclude,
		MixedPort: settings.MixedPort, AllowLAN: settings.AllowLAN, LANAllowedIPs: policy.LANAllowedIPs, Controller: s.controller, Secret: s.secret,
		ProbePortBase: s.probeBase, Gate: policy.Gate, Mirror: settings.Mirror, HoldRules: policy.Router,
		DeviceRules: policy.DeviceRules, SubRules: policy.SubRules, ExcludeMAC: policy.ExcludeMAC,
		ClientProxy: policy.ClientProxy, Box: policy.Box,
	})
	if err != nil {
		return err
	}
	result.Warnings = append(result.Warnings, distWarnings...)
	result.Warnings = append(result.Warnings, policy.Warnings...)
	s.mu.Lock()
	s.status.Warnings = result.Warnings
	unchanged := bytes.Equal(s.applied, result.YAML) && s.aliveLocked()
	rejected, rejectMsg := s.rejected, s.rejectMsg
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	if hash := ConfigHash(result.YAML); hash != "" && hash == rejected {
		// 刚回滚掉的配置：内容没变就不再应用，否则会在应用和回滚之间来回（M8-07）。
		// 内核这时退出了（崩溃重启）就用磁盘上回滚后的配置拉起来。
		if !s.Status().Running {
			if err := s.startCore(ctx); err != nil {
				return err
			}
			s.mu.Lock()
			s.applied = s.lastGood
			s.mu.Unlock()
		}
		return errors.New(rejectMsg)
	}

	if err := os.MkdirAll(s.coreDir, 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	proxy := ""
	if s.aliveLocked() {
		proxy = fmt.Sprintf("http://127.0.0.1:%d", settings.MixedPort)
	}
	s.mu.Unlock()
	// 内核自己下载 geo 数据只认一个地址，国内直连常失败；先由服务准备好。
	if err := ensureGeoData(ctx, filepath.Dir(s.self), s.coreDir, proxy, settings.Mirror); err != nil {
		return err
	}
	candidate := filepath.Join(s.coreDir, candidateFile)
	if err := os.WriteFile(candidate, result.YAML, 0o600); err != nil {
		return err
	}
	defer os.Remove(candidate)
	if err := s.validate(ctx, candidate); err != nil {
		return err
	}
	final := filepath.Join(s.coreDir, configFile)
	if err := os.Rename(candidate, final); err != nil {
		return err
	}

	s.mu.Lock()
	running := s.aliveLocked()
	s.mu.Unlock()
	// 变更前的出口是否通：变更后不通时，只有原来通的才算这次变更引入的失败（M8-07）。
	baseline := false
	if s.hooks.Confirm != nil && running {
		bctx, cancel := context.WithTimeout(ctx, baselineWait)
		baseline = s.hooks.Confirm(bctx) == nil
		cancel()
	}
	if running {
		if err := s.reload(ctx, final); err != nil {
			return fmt.Errorf("热重载失败：%w", err)
		}
	} else if err := s.startCore(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.applied = result.YAML
	s.probes = result.Probes
	s.routes = result.Routes
	s.gateDir = result.GateDirect
	s.boxRoutes = result.BoxRoutes
	s.status.AppliedAt = time.Now().UTC()
	s.confirmGen++
	gen := s.confirmGen
	s.status.ConfigHash = ConfigHash(result.YAML)
	if s.hooks.Confirm != nil {
		s.status.Confirm, s.status.ConfirmReason = ConfirmPending, ""
	}
	s.mu.Unlock()
	if s.hooks.Confirm != nil {
		s.emitConfirm(ConfirmEvent{Hash: ConfigHash(result.YAML), State: ConfirmPending})
		// 内核原来没在运行（开机、手动启动）时没有变更前的基线：按上一份确认过的配置是好的处理。
		go s.confirm(context.WithoutCancel(ctx), gen, result.YAML, baseline || !running)
	}
	s.LinesChanged()
	if s.hooks.OnApplied != nil {
		s.hooks.OnApplied(ctx, running)
	}
	if managed && profile.Version != settings.AppliedProfile && len(profile.Selections) > 0 {
		go s.applySelections(context.WithoutCancel(ctx), profile)
	}
	return nil
}

func (s *Supervisor) emitConfirm(ev ConfirmEvent) {
	if s.hooks.OnConfirm != nil {
		s.hooks.OnConfirm(ev)
	}
}

// confirm 在新配置应用后确认出口；失败且是这次变更引入的就回滚到上一份确认过的配置（M8-06、M8-07）。
func (s *Supervisor) confirm(ctx context.Context, gen int, yaml []byte, baseline bool) {
	hash := ConfigHash(yaml)
	stale := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.confirmGen != gen
	}
	var lastErr error
	deadline := time.Now().Add(confirmWindow)
	wait := confirmFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if stale() {
			return // 又应用了新配置，由新的确认任务负责
		}
		pctx, cancel := context.WithTimeout(ctx, confirmTimeout)
		lastErr = s.hooks.Confirm(pctx)
		cancel()
		if lastErr == nil {
			s.confirmed(gen, yaml)
			return
		}
		if time.Now().After(deadline) {
			break
		}
		wait = confirmEvery
	}
	if stale() {
		return
	}
	reason := lastErr.Error()
	if !baseline {
		s.setConfirm(gen, ConfirmUpstream, "变更前出口也不通，判断为上游故障，不回滚："+reason)
		s.emitConfirm(ConfirmEvent{Hash: hash, State: ConfirmUpstream, Reason: reason})
		return
	}
	s.rollback(ctx, gen, hash, reason)
}

func (s *Supervisor) setConfirm(gen int, st ConfirmState, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.confirmGen == gen {
		s.status.Confirm, s.status.ConfirmReason = st, reason
	}
}

func (s *Supervisor) confirmed(gen int, yaml []byte) {
	s.mu.Lock()
	if s.confirmGen != gen {
		s.mu.Unlock()
		return
	}
	s.status.Confirm, s.status.ConfirmReason = ConfirmOK, ""
	s.lastGood = yaml
	s.rejected, s.rejectMsg = "", ""
	s.mu.Unlock()
	if err := writeFileAtomic(filepath.Join(s.coreDir, lastGoodFile), yaml); err != nil {
		s.log.Warn("保存确认过的配置失败", "err", err)
	}
	s.emitConfirm(ConfirmEvent{Hash: ConfigHash(yaml), State: ConfirmOK})
}

// rollback 换回上一份确认过的配置，并拒绝再次应用这份配置，直到内容变化。
func (s *Supervisor) rollback(ctx context.Context, gen int, hash, reason string) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if s.confirmGen != gen {
		s.mu.Unlock()
		return
	}
	good := s.lastGood
	s.mu.Unlock()
	if len(good) == 0 || ConfigHash(good) == hash {
		msg := "新配置应用后出口不通，没有可回滚的配置：" + reason
		s.setConfirm(gen, ConfirmFailed, msg)
		s.emitConfirm(ConfirmEvent{Hash: hash, State: ConfirmFailed, Reason: reason})
		return
	}
	final := filepath.Join(s.coreDir, configFile)
	err := writeFileAtomic(final, good)
	if err == nil {
		if s.Status().Running {
			err = s.reload(ctx, final)
		} else {
			err = s.startCore(ctx)
		}
	}
	if err != nil {
		msg := "新配置应用后出口不通，回滚失败：" + err.Error()
		s.setConfirm(gen, ConfirmFailed, msg)
		s.emitConfirm(ConfirmEvent{Hash: hash, State: ConfirmFailed, Reason: msg})
		return
	}
	msg := "新配置应用后出口不通，已回滚到上一版：" + reason
	s.mu.Lock()
	s.applied = good
	s.rejected, s.rejectMsg = hash, "这份配置刚被回滚（"+reason+"），修改配置后会重新应用"
	s.status.ConfigHash = ConfigHash(good)
	s.status.Confirm, s.status.ConfirmReason = ConfirmRolledBack, msg
	s.status.LastError = msg
	s.mu.Unlock()
	s.log.Warn("配置已回滚", "hash", hash, "reason", reason)
	s.emitConfirm(ConfirmEvent{Hash: hash, State: ConfirmRolledBack, Reason: reason})
	s.LinesChanged()
	if s.hooks.OnApplied != nil {
		s.hooks.OnApplied(ctx, true)
	}
}

// AppliedConfig 返回正在运行的配置；内核没在运行时为空。
func (s *Supervisor) AppliedConfig() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aliveLocked() {
		return nil
	}
	return s.applied
}

// StrictRuleSet 是一个目标为严格组的远程规则集，供 guard 展开（ADR-039）。
type StrictRuleSet struct {
	gen.RuleSet
	URL string // 已按下载镜像改写
}

// StrictRuleSets 返回当前配置里目标为严格组、已启用的远程规则集（与生成配置用同一份数据），以及下载镜像。
func (s *Supervisor) StrictRuleSets(ctx context.Context) ([]StrictRuleSet, error) {
	settings, err := s.settings.Load(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := s.data.Groups.Export(ctx)
	if err != nil {
		return nil, err
	}
	var ruleSets []gen.RuleSet
	if profile, managed := s.distProfile(); managed {
		nodes, err := s.data.Nodes.Export(ctx)
		if err != nil {
			return nil, err
		}
		m := dist.Merge(profile, nodes, groups, settings.CustomRules)
		groups, ruleSets = m.Groups, m.RuleSets
	}
	if ruleSets == nil {
		ruleSets = gen.DefaultRuleSets()
	}
	strict := map[string]bool{}
	for _, g := range groups {
		if g.Strict {
			strict[g.Name] = true
		}
	}
	var out []StrictRuleSet
	for _, r := range ruleSets {
		if r.Disabled || r.Type == gen.TypeGeoIP || !strict[r.Target] || r.URL == "" {
			continue
		}
		out = append(out, StrictRuleSet{RuleSet: r, URL: gen.MirrorURL(r.URL, settings.Mirror)})
	}
	return out, nil
}

// MixedProxy 是内核的本机代理地址；内核没在运行时为空。
func (s *Supervisor) MixedProxy(ctx context.Context) string {
	if !s.Status().Running {
		return ""
	}
	settings, err := s.settings.Load(ctx)
	if err != nil || settings.MixedPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", settings.MixedPort)
}

// StrictTargets 返回严格组的探测入口，按端口排序；内核没在运行时为空。
func (s *Supervisor) StrictTargets() []trust.StrictTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aliveLocked() {
		return nil
	}
	out := make([]trust.StrictTarget, 0, len(s.probes))
	for g, p := range s.probes {
		out = append(out, trust.StrictTarget{Group: g, Port: p})
	}
	slices.SortFunc(out, func(a, b trust.StrictTarget) int { return a.Port - b.Port })
	return out
}

// RouteProxy 返回经某个代理组（或 DIRECT）出去的本机入口地址；内核没在运行或没有这个组时为 nil。
func (s *Supervisor) RouteProxy(group string) *url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.routes[group]
	if !s.aliveLocked() || p == 0 {
		return nil
	}
	return &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(p)}
}

// GatePorts 返回内核 DIRECT 入口和严格组的探测入口端口，供出口闸门转发；内核没在运行时都为空。
func (s *Supervisor) GatePorts() (int, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aliveLocked() {
		return 0, nil
	}
	return s.gateDir, maps.Clone(s.probes)
}

// BoxRoutes 返回当前配置里的「路由盒切换」选择组 → 原目标；内核没在运行时为空。
func (s *Supervisor) BoxRoutes() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aliveLocked() {
		return nil
	}
	return maps.Clone(s.boxRoutes)
}

// GateDirectURL 返回内核 DIRECT 入口的地址，经它探测的就是闸门直连的那条路；没有时为 nil。
func (s *Supervisor) GateDirectURL() *url.URL {
	d, _ := s.GatePorts()
	if d == 0 {
		return nil
	}
	return &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(d)}
}

// StrictEgress 返回管理服务下发的严格线路允许出口；没有绑定时为空。
func (s *Supervisor) StrictEgress() []string {
	p, ok := s.distProfile()
	if !ok {
		return nil
	}
	return p.StrictEgress
}

// BoxSecret 返回管理服务下发的路由盒发现口令（ADR-073）；没有绑定或没设置时为空。
func (s *Supervisor) BoxSecret() string {
	p, ok := s.distProfile()
	if !ok {
		return ""
	}
	return p.BoxSecret
}

func (s *Supervisor) distProfile() (dist.Profile, bool) {
	if s.Dist == nil {
		return dist.Profile{}, false
	}
	return s.Dist.Profile()
}

// applySelections 在新版本下发后把线路切到服务端指定的成员，每个版本只做一次：
// 之后用户在「线路」里手动切换的结果不会被覆盖。
func (s *Supervisor) applySelections(ctx context.Context, p dist.Profile) {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if code, _, err := s.CallCore(ctx, http.MethodGet, "/version", nil); err == nil && code == http.StatusOK {
			break
		}
		time.Sleep(2 * time.Second)
	}
	failed := 0
	for group, member := range p.Selections {
		body, _ := json.Marshal(map[string]string{"name": member})
		code, out, err := s.CallCore(ctx, http.MethodPut, ProxyPath(group), body)
		if err != nil || code >= 300 {
			failed++
			s.log.Warn("切换下发的线路失败", "group", group, "member", member, "code", code, "err", err, "body", string(out))
		}
	}
	if failed < len(p.Selections) {
		s.LinesChanged()
	}
	if failed > 0 {
		return // 下次生成配置时重试
	}
	st, err := s.settings.Load(ctx)
	if err != nil {
		return
	}
	st.AppliedProfile = p.Version
	if err := s.settings.Save(ctx, st); err != nil {
		s.log.Warn("记录线路版本失败", "err", err)
	}
}

func (s *Supervisor) validate(ctx context.Context, path string) error {
	// 先在进程内检查，原因更具体；再用内核自己的 -t 做完整校验（含规则集、geo 数据）。任一失败都不替换配置。
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := parseInProcess(b); err != nil {
		return fmt.Errorf("配置校验失败：%w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.self, "mihomo", "-t", "-d", s.coreDir, "-f", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("配置校验失败：%s", lastLine(string(out), err))
	}
	return nil
}

var logErrRE = regexp.MustCompile(`err="?(?:配置校验失败：)?(.*?)"?$`)

// lastLine 从子进程的输出里取最后一行错误，去掉日志前缀。
func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if l == "" {
		return err.Error()
	}
	if m := logErrRE.FindStringSubmatch(l); m != nil {
		return strings.ReplaceAll(m[1], `\"`, `"`)
	}
	return l
}

func (s *Supervisor) reload(ctx context.Context, path string) error {
	body, _ := json.Marshal(map[string]string{"path": path})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.ControllerURL()+"/configs?force=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	s.authorize(req)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (s *Supervisor) startChild() error {
	cmd := exec.Command(s.self, "mihomo", "-watch-stdin", "-d", s.coreDir, "-f", filepath.Join(s.coreDir, configFile))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动内核失败：%w", err)
	}
	exited := make(chan struct{})
	s.mu.Lock()
	s.child, s.stdin, s.exited, s.stopping = cmd, stdin, exited, false
	s.childAt = time.Now().UTC()
	s.coreGen++
	s.mu.Unlock()
	go func() {
		err := cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		s.mu.Lock()
		stopping := s.stopping
		s.child, s.stdin = nil, nil
		s.applied = nil
		if !stopping {
			s.status.LastError = "内核意外退出，正在重启"
		}
		s.mu.Unlock()
		close(exited)
		if !stopping {
			s.log.Warn("core exited unexpectedly", "err", err)
			time.Sleep(restartBackoff)
			s.Trigger()
		}
	}()
	return nil
}

// shutdown 在面板退出时调用：子进程方式停掉内核；独立服务方式内核继续运行（面板重启、升级不影响它）。
func (s *Supervisor) shutdown() {
	if s.svc == nil {
		s.stopChild()
	}
}

func (s *Supervisor) stopChild() {
	s.mu.Lock()
	child, stdin, exited := s.child, s.stdin, s.exited
	s.stopping = true
	s.mu.Unlock()
	if child == nil {
		return
	}
	// 让内核正常退出，它要撤销 TUN 和路由。
	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(stopGrace):
		_ = child.Process.Kill()
		<-exited
	}
}

// SetRunning 启动或停止内核，并记住这个选择。
func (s *Supervisor) SetRunning(ctx context.Context, running bool) error {
	settings, err := s.settings.Load(ctx)
	if err != nil {
		return err
	}
	settings.Running = running
	if err := s.settings.Save(ctx, settings); err != nil {
		return err
	}
	s.paused.Store(false) // 明确启动或停止后，以设置为准
	// 第一次启动要下载 geo 数据，耗时较长；界面要马上看到“启动中”。
	s.RefreshStatus(ctx)
	if running {
		return s.Apply(ctx)
	}
	if s.hooks.BeforeStop != nil {
		s.hooks.BeforeStop(ctx)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.stopCore()
	s.mu.Lock()
	s.status.LastError = ""
	s.mu.Unlock()
	s.RefreshStatus(ctx)
	return nil
}

func (s *Supervisor) setApplying(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Applying = v
}

// RefreshStatus 让状态马上反映新设置，内核稍后重载。
func (s *Supervisor) RefreshStatus(ctx context.Context) {
	settings, err := s.settings.Load(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Desired, s.status.Mode, s.status.TUN, s.status.MixedPort = settings.Running && !s.paused.Load(), settings.Mode, settings.TUN, settings.MixedPort
}

// Pause 停掉内核但不改设置（客户端「退出」）：服务还在，下次打开客户端（Resume）或服务重启时按设置恢复。
// 和 SetRunning(false) 的区别是不记住「停止」这个选择。
func (s *Supervisor) Pause(ctx context.Context) {
	s.paused.Store(true)
	if s.hooks.BeforeStop != nil {
		s.hooks.BeforeStop(ctx)
	}
	s.opMu.Lock()
	s.stopCore()
	s.opMu.Unlock()
	s.RefreshStatus(ctx)
}

// Resume 取消 Pause，按设置决定要不要启动内核。返回之前是否暂停着。
func (s *Supervisor) Resume(ctx context.Context) bool {
	if !s.paused.Swap(false) {
		return false
	}
	s.RefreshStatus(ctx)
	s.Trigger()
	return true
}

// refreshTUN 读取 TUN 的实际状态。配置校验不会启动 TUN，启动失败只会写进内核日志。
func (s *Supervisor) refreshTUN(ctx context.Context) {
	var general struct {
		TUN struct {
			Enable bool `json:"enable"`
		} `json:"tun"`
	}
	active := false
	if code, body, err := s.CallCore(ctx, http.MethodGet, "/configs", nil); err == nil && code == http.StatusOK {
		active = json.Unmarshal(body, &general) == nil && general.TUN.Enable
	}
	reason := ""
	if !active {
		if b, err := os.ReadFile(s.logPath); err == nil {
			reason = tunFailure(string(b))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.TUNActive = active
	s.status.TUNError = ""
	if s.status.TUN {
		s.status.TUNError = reason
	}
}

var tunFailureRE = regexp.MustCompile(`Start TUN listening error: (.*?)"?\s*$`)

// tunFailure 从内核日志里取最后一次 TUN 启动失败的原因。
func tunFailure(log string) string {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if m := tunFailureRE.FindStringSubmatch(lines[i]); m != nil {
			return strings.ReplaceAll(m[1], `\"`, `"`)
		}
	}
	return ""
}

// Status 返回当前状态。
func (s *Supervisor) Status() CoreStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Running = s.aliveLocked()
	if !st.Running {
		st.TUNActive, st.TUNError = false, ""
	}
	if st.Warnings == nil {
		st.Warnings = []string{}
	}
	return st
}

// Process 返回内核进程的 PID 和启动时间；没有运行时 PID 为 0。
func (s *Supervisor) Process() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.svc != nil {
		if !s.online {
			return 0, time.Time{}
		}
		return s.health.PID, s.health.StartedAt
	}
	if s.child == nil || s.child.Process == nil {
		return 0, time.Time{}
	}
	return s.child.Process.Pid, s.childAt
}

func (s *Supervisor) client() *http.Client {
	// 控制接口在本机，不能被系统代理设置带偏。
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
}

func (s *Supervisor) authorize(r *http.Request) { r.Header.Set("Authorization", "Bearer "+s.secret) }

// ErrCoreDown 表示内核没有运行。
var ErrCoreDown = errors.New("内核没有运行")

// CallCore 转发请求到内核控制接口，返回状态码和响应体。
func (s *Supervisor) CallCore(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if !s.Status().Running {
		return 0, nil, ErrCoreDown
	}
	req, err := http.NewRequestWithContext(ctx, method, s.ControllerURL()+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	s.authorize(req)
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

// ProxyPath 是内核里某个代理或代理组的接口路径。
func ProxyPath(name string) string { return "/proxies/" + url.PathEscape(name) }
