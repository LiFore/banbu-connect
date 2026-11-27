// Package trust 判断当前网络是否受信，供 Kill Switch 选择路径（killswitch.Policy）：
//
//   - 流量已被本机内核的 TUN 接管，或当前网络不是受信的 Wi-Fi：经本机内核（原 Kill Switch）；
//   - 在受信的 Wi-Fi 上，出口探测成功且出口 IP、地区符合要求：直连放行，代理由路由盒负责；
//   - 在受信的 Wi-Fi 上，但出口还没确认或不符合要求：阻断。
//   - 开了「闸门直连」时，TUN 接管也照样判断受信网络：受保护服务的连接经本机内核交给出口闸门（gate.go），
//     闸门在每个新连接前确认出口，符合就经内核的 DIRECT 交给路由盒，否则经代理组。
//
// 这里只读本机状态和探测出口，不碰系统规则；规则由 killswitch.Engine 按判断写入。
package trust

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"time"

	"boxctl/internal/boxfind"
	"boxctl/internal/killswitch"
)

const (
	tickInterval = 3 * time.Second  // 检查网络；只读本机状态，开销很小
	probeAllowed = 15 * time.Second // 直连放行时的出口探测间隔
	probeBlocked = 10 * time.Second // 阻断时的出口探测间隔，用来尽快恢复
	probeRetry   = 3 * time.Second  // 直连时探测失败一次后，很快再试一次
	egressMaxAge = 40 * time.Second // 超过这个时间没有成功探测就阻断
	// maxProbeFailures 次连续失败才算「确定的阻断」（立即通知）。直连时第一次失败就先阻断（按正在确认处理），
	// 不再等第二次：路由盒的代理挂掉、出口变成真实 IP 时，探测通常表现为超时。
	maxProbeFailures = 2
	// 睡眠唤醒的判断（checkWake）：墙上时间比单调时间多走了 wakeJump 以上（macOS 睡眠时单调时间不走），
	// 或者两次检查之间单调时间隔了 wakeGap 以上（Windows 的单调时间含睡眠；平时至少 3 秒检查一次）。
	wakeJump = 10 * time.Second
	wakeGap  = 30 * time.Second
)

// Status 是受信网络的状态，展示用。走哪条路看 killswitch.Status.Verdict。
type Status struct {
	Network  Network `json:"network"`
	Trusted  bool    `json:"trusted"`  // 当前 Wi-Fi 在受信列表里，或发现了路由盒
	Enabled  bool    `json:"enabled"`  // 设置了受信的 Wi-Fi，或打开了路由盒发现
	Probing  bool    `json:"probing"`  // TUN 没有接管，正在这个网络上探测出口
	Egress   Egress  `json:"egress"`   // 最近一次探测，含失败
	Baseline string  `json:"baseline"` // 当前网络（Wi-Fi 或路由盒）记住的出口
	// Box 是路由盒自动发现的状态（ADR-073）。
	Box BoxStatus `json:"box"`
	// Gate 是闸门直连的状态：开了闸门时每个受保护服务最近一次的出口确认。
	Gate GateStatus `json:"gate"`
}

// Notify 记录一条事件，由 Kill Switch 的事件列表展示。调用时不持有任何锁。
type Notify func(level, msg string)

// Alert 发一条「出口探测失败」的系统通知。调用时不持有任何锁。
type Alert func(title, body string)

type Evaluator struct {
	path     string
	detector Detector
	prober   EgressProber
	log      *slog.Logger
	now      func() time.Time
	kick     chan struct{}
	notify   Notify
	alert    Alert
	onNet    func() // 网络变化时调用，在 Run 之前设置
	onLeave  LeaveTrusted
	changed  func() // 判断可能变了，让 Kill Switch 立即核对；Run 设置
	watcher  *Watcher
	// viaProxy 经指定的 HTTP 代理探测出口；闸门直连时经内核的 DIRECT 入口探测。测试替换它。
	viaProxy func(*url.URL) EgressProber
	// direct 返回内核 DIRECT 入口的地址；内核没在运行或没开闸门时为 nil。
	direct func() *url.URL
	// gateURLs 返回严格组 → 该服务自己的出口探测地址（如 https://claude.ai/cdn-cgi/trace）。
	gateURLs func() map[string]string
	// gateRevoke 断开某个严格组经闸门直连的连接（空为全部），由 NewGate 设置；调用时可能持有该组的 gateState.mu。
	gateRevoke func(group string) int

	opMu    sync.Mutex     // 串行化 Tick
	probeWG sync.WaitGroup // 在途的出口探测，测试用来等待结果

	mu       sync.Mutex
	cfg      Config
	net      Network
	netSeen  bool
	tun      bool   // 最近一次 Tick 时 TUN 是否接管
	egress   Egress // 最近一次探测，含失败
	good     Egress // 最近一次成功的探测，网络变化时清空
	failures int    // 连续探测失败的次数
	last     killswitch.Verdict
	// gen 在网络或 TUN 变化时加一：探测在后台进行，回来时网络已经变了的结果直接丢弃。
	gen uint64
	// onTrusted 是最近一次有网络时是否在受信 Wi-Fi 上；断网期间保持不变，
	// 这样「受信 Wi-Fi → 断开 → 其他网络」也算离开受信网络。trustedFrom 是那个受信 Wi-Fi 的名称。
	onTrusted   bool
	trustedFrom string
	probing     bool
	cancelProbe context.CancelFunc
	// quick 是最近的快速指纹（网卡和地址）；路由变化时与它比较，变了就立即作废出口结果。
	quick string
	// unsettled 表示路由变了、还没重新识别网络（Tick 读 SSID 要起子进程，可能要一两秒）。
	unsettled bool
	// clock 返回真实的墙上时间和单调时间（同一时刻的两个读数），wakeWall、wakeMono 是上一次 checkWake 时的读数。
	// e.now 测试会替换，不能用来判断睡眠。
	clock              func() (wall, mono time.Time)
	wakeWall, wakeMono time.Time

	gateMu sync.Mutex
	gates  map[string]*gateState

	// 路由盒发现（ADR-073，见 boxnet.go）。box 是当前网络上验证通过的路由盒，网络变化时清空。
	boxSecret  func() string
	boxServers func(iface string) []netip.Addr
	findBox    BoxFinder
	// boxProxy 确认路由盒的代理入口能用（ADR-084）：客户端经它把流量交给路由盒，旧版路由盒没有它。测试替换。
	boxProxy func(ctx context.Context, addr netip.Addr, secret string) error
	// boxHello 返回要报给路由盒的客户端信息，为 nil 时不报；sendHello 发出去（测试替换）。
	boxHello  func() boxfind.Hello
	sendHello func(ctx context.Context, addr netip.AddrPort, secret string, h boxfind.Hello) error
	box       boxfind.Box
	boxAt     time.Time
	boxErr    string
	boxFails  int
	boxBusy   bool
	boxIface  string        // 最近一次发现用的网卡
	boxDNS    []string      // 最近一次发现问的 DNS 服务器
	boxTrace  boxfind.Trace // 最近一次发现的过程
}

func New(path string, detector Detector, prober EgressProber, log *slog.Logger) (*Evaluator, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	return &Evaluator{
		path: path, detector: detector, prober: prober, log: log,
		now: time.Now, clock: realClock, kick: make(chan struct{}, 1), cfg: cfg, notify: func(string, string) {},
		alert: func(string, string) {}, onNet: func() {}, changed: func() {},
		viaProxy:  func(p *url.URL) EgressProber { return HTTPProber{Proxy: p} },
		direct:    func() *url.URL { return nil },
		gateURLs:  func() map[string]string { return nil },
		gates:     map[string]*gateState{},
		findBox:   boxfind.Discover,
		boxProxy:  boxfind.VerifyProxy,
		sendHello: boxfind.SendHello,
	}, nil
}

// Watch 订阅系统的路由变化，换网络后立即作废出口结果，不等定时检查。必须在 Run 之前调用；
// 失败时只靠定时检查。ctx 结束时停止监听。
func (e *Evaluator) Watch(ctx context.Context) error {
	w, err := NewWatcher(e.RouteChanged)
	if err != nil || w == nil {
		return err
	}
	e.watcher = w
	if err := w.Start(); err != nil {
		e.watcher = nil
		return err
	}
	go func() {
		<-ctx.Done()
		w.Close()
	}()
	return nil
}

// SetGate 设置闸门直连用的内核 DIRECT 入口和各服务的探测地址，必须在 Run 之前调用。
func (e *Evaluator) SetGate(direct func() *url.URL, urls func() map[string]string) {
	e.direct, e.gateURLs = direct, urls
}

// RouteChanged 由 Watcher 在路由变化时调用。网卡或地址变了就立即作废出口结果并通知 Kill Switch，
// 不等 Tick 读出新的 SSID：离开受信网络时，这几秒里直连没有任何规则。
func (e *Evaluator) RouteChanged() {
	fp := quickFingerprint(e.watcher.Physical())
	e.mu.Lock()
	if fp == e.quick {
		e.mu.Unlock()
		return
	}
	e.quick = fp
	e.unsettled = true
	e.resetEgressLocked()
	e.clearBoxLocked()
	e.mu.Unlock()
	e.changed()
	e.Kick()
}

// checkWake 判断是否刚从睡眠中唤醒。出口结果的有效期按单调时间算，macOS 睡眠时单调时间不走，
// 睡了一整夜醒来，睡前的探测结果还算「刚做过」；同一个 Wi-Fi、拿回同一个地址时路由指纹也不变。
// 所以唤醒按网络变化处理：作废出口结果和路由盒，先按「正在确认新网络」阻断，重新识别、重新探测。
func (e *Evaluator) checkWake() bool {
	wall, mono := e.clock()
	e.mu.Lock()
	refWall, refMono := e.wakeWall, e.wakeMono
	e.wakeWall, e.wakeMono = wall, mono
	if refWall.IsZero() || !woke(wall.Sub(refWall), mono.Sub(refMono)) {
		e.mu.Unlock()
		return false
	}
	e.unsettled = true
	e.resetEgressLocked()
	e.clearBoxLocked()
	e.mu.Unlock()
	e.event("info", "系统从睡眠中唤醒（或时钟跳变），重新确认网络和出口")
	e.changed()
	e.Kick()
	return true
}

// woke 按两次检查之间墙上时间、单调时间各走了多少判断中间是否睡过。
func woke(wall, mono time.Duration) bool { return wall-mono >= wakeJump || mono >= wakeGap }

// realClock 的单调读数来自 time.Now 自带的单调时钟；Round(0) 去掉它，剩下墙上时间。
func realClock() (wall, mono time.Time) {
	now := time.Now()
	return now.Round(0), now
}

// resetEgressLocked 作废出口结果和在途探测，调用时持有 e.mu。
func (e *Evaluator) resetEgressLocked() {
	e.good, e.egress, e.failures = Egress{}, Egress{}, 0
	e.gen++
	if e.cancelProbe != nil {
		e.cancelProbe()
	}
	e.probing, e.cancelProbe = false, nil
}

// LeaveTrusted 在从受信 Wi-Fi 切换到不受信的网络时调用（只在设置了 AutoStartProxy 时）。
// from 是离开的网络（「Wi-Fi「名称」」或「路由盒「ID」」），to 是新网络的描述。调用时持有 Tick 的串行锁，回调里不要做耗时操作。
type LeaveTrusted func(from, to string)

// SetNotify 设置事件出口，必须在 Run 之前调用。
func (e *Evaluator) SetNotify(n Notify) { e.notify = n }

// SetAlert 设置系统通知出口，必须在 Run 之前调用。
func (e *Evaluator) SetAlert(a Alert) { e.alert = a }

// OnNetworkChange 设置网络变化（网卡或 Wi-Fi 变了）时的回调，必须在 Run 之前调用。
func (e *Evaluator) OnNetworkChange(f func()) { e.onNet = f }

// OnLeaveTrusted 设置离开受信网络时的回调，必须在 Run 之前调用。
func (e *Evaluator) OnLeaveTrusted(f LeaveTrusted) { e.onLeave = f }

// Run 循环检查。tunActive 报告本机内核是否接管了流量；判断变化时调用 onChange。
func (e *Evaluator) Run(ctx context.Context, tunActive func() bool, onChange func()) {
	e.changed = onChange
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		if e.Tick(ctx, tunActive()) {
			onChange()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		}
	}
}

// Kick 让 Run 立即检查一次，用于配置变化后。
func (e *Evaluator) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Tick 检查一次网络，需要时在后台探测出口（探测最长要 8 秒，不能让它挡住网络检查）。
// 返回判断是否变化；探测完成后会再 Kick 一次，由下一次 Tick 报告。
func (e *Evaluator) Tick(ctx context.Context, tun bool) bool {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.checkWake()
	physical := e.watcher.Physical()
	quick := quickFingerprint(physical)
	n := e.detect(ctx, tun, physical)

	e.mu.Lock()
	prev, first := e.net, !e.netSeen
	tunChanged := e.tun != tun
	// 同一张网卡、同名 Wi-Fi，但网关换了（同网段的另一台路由器）也算换了网络。
	routerMoved := !first && prev.Interface == n.Interface && routerChanged(prev.Router, n.Router)
	if prev.Interface == n.Interface && !routerMoved {
		n.Router = keepRouterMAC(prev.Router, n.Router)
	}
	e.net, e.netSeen, e.tun = n, true, tun
	e.quick, e.unsettled = quick, false
	changed := first || prev.fingerprint() != n.fingerprint() || routerMoved
	if changed || tunChanged {
		// 旧网络上的探测结果对新网络没有意义；TUN 开关前后，流量的出口也不同。
		e.resetEgressLocked()
	}
	if changed {
		e.clearBoxLocked()
	}
	if e.boxDueLocked(n) {
		e.startBoxFindLocked(ctx, n.Interface)
	}
	cfg := e.cfg
	// 离开受信网络：只看网络本身的切换，改设置（把当前 Wi-Fi 移出受信列表）不算；没有网络时不下结论。
	var left bool
	from := e.trustedFrom
	if n.Interface != "" {
		trusted := trustedNetwork(cfg, n, e.box)
		left = changed && !first && e.onTrusted && !trusted
		e.onTrusted = trusted
		if trusted {
			e.trustedFrom = netLabel(n, e.box)
		}
	}
	e.mu.Unlock()
	if changed && !first {
		if !tun {
			e.event("info", "网络变化："+describe(prev)+" → "+describe(n))
		}
		e.onNet()
	}
	if left && cfg.AutoStartProxy && e.onLeave != nil {
		e.onLeave(from, describe(n))
	}

	// TUN 接管时，本机直接发出的探测会经过内核的规则，看到的是代理节点的出口；
	// 开了闸门直连时改经内核的 DIRECT 入口探测，看到的正是闸门直连的那条路。
	e.mu.Lock()
	box := e.box
	e.mu.Unlock()
	if trustedNetwork(cfg, n, box) && e.probeDue() {
		if proxy, ok := e.probePath(cfg, tun); ok {
			e.startProbe(ctx, cfg.ProbeURLs, proxy)
		}
	}

	v := e.Verdict(tun)
	e.mu.Lock()
	moved := v != e.last
	e.last = v
	e.mu.Unlock()
	return moved
}

// detect 读当前网络。本机内核的 TUN 接管时出口网卡是 TUN，改读默认路由所在的物理网卡。
func (e *Evaluator) detect(ctx context.Context, tun bool, physical string) Network {
	var n Network
	if d, ok := e.detector.(InterfaceDetector); ok && tun && physical != "" {
		n = d.DetectOn(ctx, physical)
	} else {
		n = e.detector.Detect(ctx)
	}
	if d, ok := e.detector.(RouterDetector); ok && n.Interface != "" {
		n.Router = d.Router(ctx, n.Interface)
	}
	return n
}

// probePath 决定经哪条路探测出口：TUN 没接管时本机直接探测（proxy 为 nil）；
// TUN 接管且开了闸门直连时经内核的 DIRECT 入口；否则不探测。
func (e *Evaluator) probePath(cfg Config, tun bool) (*url.URL, bool) {
	if !tun {
		return nil, true
	}
	if d := e.direct(); cfg.GateDirect && d != nil {
		return d, true
	}
	return nil, false
}

// gateActive 表示 TUN 接管时仍按受信网络判断（开了闸门直连，内核的 DIRECT 入口可用）。
func (e *Evaluator) gateActive(cfg Config) bool { return cfg.GateDirect && e.direct() != nil }

// startProbe 在后台探测出口，完成后 Kick 让 Run 立即按新结果判断。
func (e *Evaluator) startProbe(ctx context.Context, urls []string, proxy *url.URL) {
	pctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	gen := e.gen
	e.probing, e.cancelProbe = true, cancel
	e.mu.Unlock()
	e.probeWG.Add(1)
	go func() {
		defer e.probeWG.Done()
		defer cancel()
		prober := e.prober
		if proxy != nil {
			prober = e.viaProxy(proxy)
		}
		if e.recordEgress(gen, prober.Probe(pctx, urls)) {
			e.learnBaseline()
			e.Kick()
		}
	}()
}

func (e *Evaluator) probeDue() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.probing {
		return false
	}
	if e.egress.CheckedAt.IsZero() {
		return true
	}
	interval := probeBlocked
	switch {
	case e.last.Path == killswitch.PathDirect && e.egress.OK:
		interval = probeAllowed
	case e.failures == 1 && e.good.OK:
		interval = probeRetry // 直连时第一次失败：已经先阻断了，尽快确认是不是偶发
	}
	return e.now().Sub(e.egress.CheckedAt) >= interval
}

// recordEgress 记下探测结果；探测期间网络变了（gen 不同）时丢弃，返回 false。
func (e *Evaluator) recordEgress(gen uint64, eg Egress) bool {
	e.mu.Lock()
	if gen != e.gen {
		e.mu.Unlock()
		return false
	}
	e.probing, e.cancelProbe = false, nil
	n := e.net
	prev := e.egress
	e.egress = eg
	if eg.OK {
		e.good, e.failures = eg, 0
	} else {
		e.failures++
	}
	e.mu.Unlock()
	switch {
	case !eg.OK && (prev.OK || prev.CheckedAt.IsZero()):
		e.event("warn", "出口探测失败："+eg.Error)
		e.alert("出口探测失败", describe(n)+"："+eg.Error)
	case eg.OK && prev.OK && prev.IP != eg.IP:
		e.event("warn", "出口 IP 变化："+prev.IP+" → "+eg.IP)
	case eg.OK && !prev.OK:
		e.event("info", "出口探测成功："+eg.IP+countrySuffix(eg.Country))
	}
	return true
}

// learnBaseline 在没有配置允许列表时，记住这个 Wi-Fi 第一次探测到的出口。
func (e *Evaluator) learnBaseline() {
	e.mu.Lock()
	cfg, n, good, box := e.cfg, e.net, e.good, e.box
	e.mu.Unlock()
	key := netKey(n, box)
	if len(cfg.ExpectedEgress) > 0 || !good.OK || key == "" || cfg.Baselines[key] != "" {
		return
	}
	next := cloneConfig(cfg)
	next.Baselines[key] = good.IP
	if err := e.store(next); err != nil {
		e.event("error", err.Error())
		return
	}
	e.event("info", fmt.Sprintf("已记住%s的出口 %s，之后出口变化会阻断", netLabel(n, box), good.IP))
}

// OnTrustedNetwork 报告当前是否确定在受信的 Wi-Fi（路由盒网络）上，不看出口探测：
// 路由盒的 DNS 防泄漏不依赖出口是否符合要求。路由刚变化、还没重新识别网络时返回 false（偏向拦截）。
func (e *Evaluator) OnTrustedNetwork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.netSeen && !e.unsettled && trustedNetwork(e.cfg, e.net, e.box)
}

// TrustedInterface 在确定位于受信网络上时返回物理网卡名，判断同 OnTrustedNetwork。
func (e *Evaluator) TrustedInterface() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.netSeen && !e.unsettled && trustedNetwork(e.cfg, e.net, e.box) && e.net.Interface != "" {
		return e.net.Interface, true
	}
	return "", false
}

// CurrentInterface 在网络已识别（路由没有刚变化）时返回物理网卡名，不看是否受信。
func (e *Evaluator) CurrentInterface() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.netSeen && !e.unsettled && e.net.Interface != "" {
		return e.net.Interface, true
	}
	return "", false
}

// boxVerdict 是路由盒网络上的判断：总是直连交给路由盒，说明里带上最近的出口探测结果。
func boxVerdict(label string, good, latest Egress, tun bool) killswitch.Verdict {
	reason := label
	switch {
	case latest.Error != "":
		reason += "，出口探测失败（" + latest.Error + "），以路由盒的判断为准"
	case good.OK:
		reason += "，出口 " + good.IP + countrySuffix(good.Country)
	}
	if tun {
		reason += "，全部流量交给路由盒"
	}
	return killswitch.Verdict{Path: killswitch.PathDirect, Reason: reason}
}

// BoxAddr 返回当前路由盒的地址（OnBox 时）。
func (e *Evaluator) BoxAddr() (netip.Addr, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.netSeen && !e.unsettled && e.box.ID != "" {
		return e.box.Addr.Addr(), true
	}
	return netip.Addr{}, false
}

// OnBox 报告当前是否在发现并验证了的路由盒网络上（路由刚变化、还没重新识别时为 false）。
func (e *Evaluator) OnBox() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.netSeen && !e.unsettled && e.box.ID != ""
}

// trustedNetwork 判断当前网络是否受信：发现并验证了路由盒，或在受信的 Wi-Fi 上（名单暂时保留，ADR-073）。
func trustedNetwork(cfg Config, n Network, box boxfind.Box) bool {
	return box.ID != "" || n.WiFi && n.SSID != "" && slices.Contains(cfg.TrustedSSIDs, n.SSID)
}

// Verdict 实现 killswitch.Policy。只读内存状态。
func (e *Evaluator) Verdict(tun bool) killswitch.Verdict {
	gate := e.gateActive(e.config())
	discovery := e.discoveryOn()
	e.mu.Lock()
	cfg, n, good, latest, failures, seen := e.cfg, e.net, e.good, e.egress, e.failures, e.netSeen
	unsettled, wasTrusted := e.unsettled, e.onTrusted
	bs := boxState{on: discovery, box: e.box, err: e.boxErr, checked: !e.boxAt.IsZero()}
	now := e.now()
	// 内核的 TUN 刚接管、Evaluator 还没来得及重新检查时，按 TUN 处理：经内核总是安全的。
	tunStale := e.tun != tun
	e.mu.Unlock()
	if !seen || tunStale {
		good = Egress{}
	}
	// 路由变了、还没读出新网络：原来在受信网络上就先阻断（出口结果已作废），否则照常经内核。
	if unsettled && (len(cfg.TrustedSSIDs) > 0 || discovery) && (!tun || gate) {
		if wasTrusted {
			return killswitch.Verdict{Path: killswitch.PathHold, Reason: "网络变化，正在确认新网络", Pending: true}
		}
		return killswitch.Verdict{Path: killswitch.PathProxy, Reason: "网络变化，正在确认新网络"}
	}
	return verdictFor(cfg, n, bs, good, latest, failures, now, tun, gate)
}

// boxState 是判断用的路由盒发现结果。
type boxState struct {
	on      bool // 打开了发现
	box     boxfind.Box
	err     string
	checked bool // 这个网络上已经找过一次
}

func verdictFor(cfg Config, n Network, bs boxState, good, latest Egress, failures int, now time.Time, tun, gate bool) killswitch.Verdict {
	proxy := func(reason string) killswitch.Verdict {
		return killswitch.Verdict{Path: killswitch.PathProxy, Reason: reason}
	}
	hold := func(format string, args ...any) killswitch.Verdict {
		return killswitch.Verdict{Path: killswitch.PathHold, Reason: fmt.Sprintf(format, args...)}
	}
	switch {
	case len(cfg.TrustedSSIDs) == 0 && !bs.on:
		return proxy("")
	case tun && !gate:
		return proxy("TUN 已接管流量")
	case n.Interface == "":
		return proxy("没有网络连接")
	case bs.box.ID != "":
		// 发现并验证了路由盒（ADR-073）：路由盒接管，全部流量交给它、按它的策略走，不看出口探测（ADR-082）。
		// 出口不对由路由盒自己的严格线路检查和阻断兜底；探测结果只用来显示。
		return boxVerdict(netLabel(n, bs.box), good, latest, tun)
	default:
		if reason := ssidReason(cfg, n); reason != "" {
			if !bs.on {
				return proxy(reason)
			}
			missing := "正在查找路由盒"
			if bs.checked {
				missing = "没有发现路由盒"
				if bs.err != "" {
					missing += "（" + bs.err + "）"
				}
			}
			if len(cfg.TrustedSSIDs) == 0 {
				return proxy(missing)
			}
			return proxy(missing + "；" + reason)
		}
	}
	label := netLabel(n, bs.box)
	if !good.OK || now.Sub(good.CheckedAt) > egressMaxAge || failures > 0 {
		if latest.Error != "" {
			v := hold("出口探测失败：%s", latest.Error)
			// 刚才还正常、只失败了一次：先阻断，但按「正在确认」处理（不立即弹通知），很快会重试。
			v.Pending = good.OK && failures < maxProbeFailures
			return v
		}
		v := hold("正在探测%s的出口", label)
		v.Pending = true
		return v
	}
	ip, _ := netip.ParseAddr(good.IP)
	if len(cfg.ExpectedEgress) > 0 {
		if !matchAny(cfg.ExpectedEgress, ip) {
			return hold("出口 %s 不在允许列表里", good.IP)
		}
	} else if base := cfg.Baselines[netKey(n, bs.box)]; base != "" && base != good.IP {
		return hold("出口从 %s 变为 %s；确认无误后接受新出口", base, good.IP)
	}
	if len(cfg.AllowedCountries) > 0 && !slices.Contains(cfg.AllowedCountries, good.Country) {
		c := good.Country
		if c == "" {
			c = "未知"
		}
		return hold("出口地区 %s 不在允许列表里", c)
	}
	via := ""
	if tun {
		via = "，除直连外的流量交给路由盒，受保护服务经出口闸门逐个确认"
	}
	return killswitch.Verdict{Path: killswitch.PathDirect,
		Reason: fmt.Sprintf("%s，出口 %s%s%s", label, good.IP, countrySuffix(good.Country), via)}
}

// ssidReason 返回当前网络不算受信 Wi-Fi 的原因；是受信 Wi-Fi 时为空。
func ssidReason(cfg Config, n Network) string {
	switch {
	case !n.WiFi:
		return fmt.Sprintf("出口网卡 %s 不是 Wi-Fi", n.Interface)
	case n.SSID == "":
		if n.Hint != "" {
			return n.Hint
		}
		return "读不到 Wi-Fi 名称"
	case !slices.Contains(cfg.TrustedSSIDs, n.SSID):
		return fmt.Sprintf("Wi-Fi「%s」不受信", n.SSID)
	}
	return ""
}

func matchAny(list []string, ip netip.Addr) bool {
	for _, raw := range list {
		if p, err := parsePrefix(raw); err == nil && p.Contains(ip) {
			return true
		}
	}
	return false
}

func countrySuffix(c string) string {
	if c == "" {
		return ""
	}
	return "（" + c + "）"
}

func describe(n Network) string {
	switch {
	case n.Interface == "":
		return "无网络"
	case n.SSID != "":
		return fmt.Sprintf("Wi-Fi「%s」", n.SSID)
	case n.WiFi:
		return "Wi-Fi（名称未知）"
	default:
		return n.Interface
	}
}

// ---- 供 API 调用 ----

func (e *Evaluator) Status() Status {
	gate := e.gateStatus()
	active := e.gateActive(e.config())
	discovery := e.discoveryOn()
	e.mu.Lock()
	defer e.mu.Unlock()
	trusted := trustedNetwork(e.cfg, e.net, e.box)
	return Status{
		Network:  e.net,
		Trusted:  trusted,
		Enabled:  len(e.cfg.TrustedSSIDs) > 0 || discovery,
		Probing:  (!e.tun || active) && trusted,
		Egress:   e.egress,
		Baseline: e.cfg.Baselines[netKey(e.net, e.box)],
		Gate:     gate,
		Box:      e.boxStatusLocked(),
	}
}

func (e *Evaluator) Config() Config { return e.config() }

// UpdateConfig 保存用户可改的字段，基准由程序维护（可以删除，不能新增）。
// blockedCIDRs 是 Kill Switch 拦截的网段：探测地址不能落在里面。
func (e *Evaluator) UpdateConfig(next Config, blockedCIDRs []string) error {
	cur := e.config()
	baselines := map[string]string{}
	for ssid := range next.Baselines {
		if ip, ok := cur.Baselines[ssid]; ok {
			baselines[ssid] = ip
		}
	}
	next.Baselines = baselines
	if err := next.normalize(); err != nil {
		return &ValidationError{err.Error()}
	}
	if err := next.checkProbesNotBlocked(blockedCIDRs); err != nil {
		return &ValidationError{err.Error()}
	}
	if err := e.store(next); err != nil {
		return err
	}
	e.event("info", "受信网络设置已保存")
	e.Kick()
	return nil
}

// TrustCurrent 把当前 Wi-Fi 加入受信列表。
func (e *Evaluator) TrustCurrent() error {
	e.mu.Lock()
	n := e.net
	e.mu.Unlock()
	if !n.WiFi || n.SSID == "" {
		return &ValidationError{"当前出口不是可识别的 Wi-Fi"}
	}
	cfg := e.config()
	if slices.Contains(cfg.TrustedSSIDs, n.SSID) {
		return nil
	}
	cfg.TrustedSSIDs = append(cfg.TrustedSSIDs, n.SSID)
	if err := e.store(cfg); err != nil {
		return err
	}
	e.event("info", fmt.Sprintf("已信任 Wi-Fi「%s」", n.SSID))
	e.Kick()
	return nil
}

// AcceptEgress 把当前出口记为这个 Wi-Fi 的基准。
func (e *Evaluator) AcceptEgress() error {
	e.mu.Lock()
	n, good, box := e.net, e.good, e.box
	e.mu.Unlock()
	cfg := e.config()
	if !trustedNetwork(cfg, n, box) {
		return &ValidationError{"当前不在受信的网络上（受信 Wi-Fi 或路由盒）"}
	}
	key := netKey(n, box)
	if len(cfg.ExpectedEgress) > 0 {
		return &ValidationError{"已设置出口允许列表，请直接修改列表"}
	}
	if !good.OK {
		return &ValidationError{"还没有成功的出口探测结果"}
	}
	cfg.Baselines[key] = good.IP
	// 各服务的出口跟着重新记录：用户确认了这个网络现在的出口。
	for k := range cfg.Baselines {
		if net, _, ok := splitServiceKey(k); ok && net == key {
			delete(cfg.Baselines, k)
		}
	}
	if err := e.store(cfg); err != nil {
		return err
	}
	e.resetGates()
	e.event("info", fmt.Sprintf("已接受%s的新出口 %s", netLabel(n, box), good.IP))
	e.Kick()
	return nil
}

// Recheck 立即重新探测出口。
func (e *Evaluator) Recheck() {
	e.mu.Lock()
	e.egress.CheckedAt = time.Time{}
	e.mu.Unlock()
	e.Kick()
}

func (e *Evaluator) config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneConfig(e.cfg)
}

func (e *Evaluator) store(c Config) error {
	if err := saveConfig(e.path, c); err != nil {
		return fmt.Errorf("保存受信网络设置失败：%w", err)
	}
	e.mu.Lock()
	e.cfg = c
	e.mu.Unlock()
	return nil
}

func (e *Evaluator) event(level, msg string) {
	if e.log != nil {
		e.log.Info(msg, "kind", level, "source", "trust")
	}
	e.notify(level, msg)
}

// ValidationError 是用户输入错误，API 返回 400。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
