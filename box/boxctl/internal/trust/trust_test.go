package trust

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"boxctl/internal/killswitch"
)

// ---- 测试替身 ----

type fakeDetector struct {
	mu sync.Mutex
	n  Network
}

func (f *fakeDetector) Detect(context.Context) Network {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeDetector) set(n Network) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n = n
}

type fakeProber struct {
	mu    sync.Mutex
	eg    Egress
	calls int
	now   func() time.Time
}

func (f *fakeProber) Probe(context.Context, []string) Egress {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	eg := f.eg
	eg.CheckedAt = f.now()
	return eg
}

func (f *fakeProber) set(eg Egress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eg = eg
}

type harness struct {
	e        *Evaluator
	detector *fakeDetector
	prober   *fakeProber
	clock    time.Time
	tun      bool
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "network.json")
	cfg := defaultConfig()
	cfg.TrustedSSIDs = []string{"Home"}
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	h := &harness{detector: &fakeDetector{}, clock: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	h.prober = &fakeProber{now: func() time.Time { return h.clock }}
	e, err := New(path, h.detector, h.prober, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return h.clock }
	e.boxProxy = func(context.Context, netip.Addr, string) error { return nil } // 测试用的路由盒都开着代理入口
	h.e = e
	return h
}

func (h *harness) tick(advance time.Duration) killswitch.Verdict {
	h.clock = h.clock.Add(advance)
	h.e.Tick(context.Background(), h.tun)
	h.e.probeWG.Wait()
	// 探测完成后 Run 会被 Kick 再检查一次，这里照做。
	h.e.Tick(context.Background(), h.tun)
	return h.e.Verdict(h.tun)
}

var home = Network{Interface: "en0", WiFi: true, SSID: "Home"}

func good(ip, country string) Egress { return Egress{OK: true, IP: ip, Country: country} }

func expect(t *testing.T, v killswitch.Verdict, path killswitch.Path, reason string) {
	t.Helper()
	if v.Path != path {
		t.Fatalf("路径 = %s（%s），应为 %s", v.Path, v.Reason, path)
	}
	if reason != "" && !strings.Contains(v.Reason, reason) {
		t.Fatalf("原因 = %q，应包含 %q", v.Reason, reason)
	}
}

func direct(t *testing.T, v killswitch.Verdict) { t.Helper(); expect(t, v, killswitch.PathDirect, "") }

// ---- 判断 ----

func TestNoTrustedWiFiMeansProxyOnly(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TrustedSSIDs = nil })
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "US"))
	expect(t, h.tick(0), killswitch.PathProxy, "")
	if h.prober.calls != 0 {
		t.Fatal("没有受信 Wi-Fi 时不应探测出口")
	}
}

func TestUntrustedNetworksGoThroughProxy(t *testing.T) {
	h := newHarness(t, nil)
	h.detector.set(Network{Interface: "en7"})
	expect(t, h.tick(0), killswitch.PathProxy, "不是 Wi-Fi")

	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	expect(t, h.tick(time.Second), killswitch.PathProxy, "不受信")
	if h.prober.calls != 0 {
		t.Fatal("不受信的网络不应探测出口")
	}

	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(home)
	direct(t, h.tick(time.Second))
	if got := h.e.Config().Baselines["Home"]; got != "203.0.113.7" {
		t.Fatalf("应记住首次出口作为基准，得到 %q", got)
	}
}

func TestTUNOverridesTrust(t *testing.T) {
	h := newHarness(t, nil)
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(0))

	// Evaluator 还没重新检查时，内核报告 TUN 已接管：立即按经内核处理。
	expect(t, h.e.Verdict(true), killswitch.PathProxy, "TUN")

	h.tun = true
	calls := h.prober.calls
	expect(t, h.tick(probeAllowed), killswitch.PathProxy, "TUN")
	if h.prober.calls != calls {
		t.Fatal("TUN 接管时探测会经过内核，不应探测")
	}

	// TUN 关闭：需要重新确认出口才直连。
	h.tun = false
	h.prober.set(Egress{Error: "timeout"})
	expect(t, h.tick(time.Second), killswitch.PathHold, "")
}

func TestLeavingTrustedWiFiSwitchesToProxy(t *testing.T) {
	h := newHarness(t, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(home)
	direct(t, h.tick(0))

	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Other"})
	expect(t, h.tick(time.Second), killswitch.PathProxy, "不受信")

	h.detector.set(Network{})
	expect(t, h.tick(time.Second), killswitch.PathProxy, "没有网络")
}

func TestLeaveTrustedCallback(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AutoStartProxy = true })
	var got []string
	h.e.OnLeaveTrusted(func(from, to string) { got = append(got, from+" → "+to) })
	h.prober.set(good("203.0.113.7", "US"))

	// 启动时就在不受信的网络上：不是「离开」，不触发。
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	h.tick(0)
	h.detector.set(home)
	direct(t, h.tick(time.Second))
	if len(got) != 0 {
		t.Fatalf("进入受信网络不应触发：%v", got)
	}

	// 受信 → 断网 → 其他 Wi-Fi：断网期间不下结论，连上其他 Wi-Fi 时触发一次。
	h.detector.set(Network{})
	h.tick(time.Second)
	if len(got) != 0 {
		t.Fatalf("断网时不应触发：%v", got)
	}
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	h.tick(time.Second)
	h.tick(time.Second)
	if len(got) != 1 || got[0] != "Wi-Fi「Home」 → Wi-Fi「Cafe」" {
		t.Fatalf("应触发一次，得到 %v", got)
	}

	// 受信 → 有线：也算离开。
	h.detector.set(home)
	h.tick(time.Second)
	h.detector.set(Network{Interface: "en7"})
	h.tick(time.Second)
	if len(got) != 2 || got[1] != "Wi-Fi「Home」 → en7" {
		t.Fatalf("切到有线应触发，得到 %v", got)
	}

	// 改设置把当前 Wi-Fi 移出受信列表不算切换网络。
	h.detector.set(home)
	h.tick(time.Second)
	cfg := h.e.Config()
	cfg.TrustedSSIDs = []string{"Office"}
	if err := h.e.UpdateConfig(cfg, nil); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	if len(got) != 2 {
		t.Fatalf("修改设置不应触发：%v", got)
	}
}

func TestLeaveTrustedNeedsSwitch(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AutoStartProxy = false })
	called := false
	h.e.OnLeaveTrusted(func(string, string) { called = true })
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(home)
	h.tick(0)
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	h.tick(time.Second)
	if called {
		t.Fatal("开关关闭时不应触发")
	}
}

func TestDefaultsOnAndKeepSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	c, err := loadConfig(path)
	if err != nil || !c.AutoStartProxy || !c.GateDirect {
		t.Fatalf("新装默认开启：%+v %v", c, err)
	}
	// 老版本保存的配置没有这两个字段：按默认开启
	if err := os.WriteFile(path, []byte(`{"trusted_ssids":["Home"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := loadConfig(path); !c.AutoStartProxy || !c.GateDirect {
		t.Fatalf("没有字段时默认开启：%+v", c)
	}
	// 用户关掉并保存过的，保持关闭
	if err := os.WriteFile(path, []byte(`{"auto_start_proxy":false,"gate_direct":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := loadConfig(path); c.AutoStartProxy || c.GateDirect {
		t.Fatalf("保存过的关闭不应被改：%+v", c)
	}
}

func TestHoldsOnEgressChangeUntilAccepted(t *testing.T) {
	h := newHarness(t, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(home)
	direct(t, h.tick(0))

	h.prober.set(good("198.51.100.9", "US"))
	direct(t, h.tick(5*time.Second)) // 还没到探测间隔
	expect(t, h.tick(probeAllowed), killswitch.PathHold, "变为 198.51.100.9")

	if err := h.e.AcceptEgress(); err != nil {
		t.Fatal(err)
	}
	direct(t, h.tick(time.Second))
}

func TestEgressReturningToBaselineRecovers(t *testing.T) {
	h := newHarness(t, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(home)
	direct(t, h.tick(0))

	h.prober.set(good("198.51.100.9", "US"))
	expect(t, h.tick(probeAllowed), killswitch.PathHold, "变为")
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(probeBlocked))
}

func TestExpectedEgressList(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ExpectedEgress = []string{"203.0.113.0/24"} })
	h.detector.set(home)
	h.prober.set(good("198.51.100.9", "US"))
	expect(t, h.tick(0), killswitch.PathHold, "不在允许列表")
	h.prober.set(good("203.0.113.50", "US"))
	direct(t, h.tick(probeBlocked))
	if len(h.e.Config().Baselines) != 0 {
		t.Fatal("有允许列表时不应记基准")
	}
}

func TestCountryCheck(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowedCountries = []string{"us", "JP"} })
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "HK"))
	expect(t, h.tick(0), killswitch.PathHold, "地区 HK")
}

func TestProbeFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.detector.set(home)
	h.prober.set(Egress{Error: "timeout"})
	expect(t, h.tick(0), killswitch.PathHold, "探测失败")

	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(probeBlocked))

	// 直连时第一次失败就先阻断，但按「正在确认」处理（不立即通知），3 秒后重试。
	h.prober.set(Egress{Error: "timeout"})
	v := h.tick(probeAllowed)
	expect(t, v, killswitch.PathHold, "探测失败")
	if !v.Pending {
		t.Fatal("第一次失败应按正在确认处理")
	}
	// 重试成功就恢复直连。
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(probeRetry))

	// 连续失败：确定的阻断。
	h.prober.set(Egress{Error: "timeout"})
	h.tick(probeAllowed)
	v = h.tick(probeRetry)
	expect(t, v, killswitch.PathHold, "探测失败")
	if v.Pending {
		t.Fatal("连续失败应是确定的阻断")
	}
}

func TestNetworkChangeDropsOldEgress(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TrustedSSIDs = []string{"Home", "Home-5G"} })
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(0))

	// 换到另一个受信 Wi-Fi：先阻断，探测通过才放行；探测失败就保持阻断。
	h.prober.set(Egress{Error: "timeout"})
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Home-5G"})
	expect(t, h.tick(time.Second), killswitch.PathHold, "探测失败")
}

func TestTrustCurrent(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TrustedSSIDs = nil })
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "New"})
	h.prober.set(good("203.0.113.7", "US"))
	expect(t, h.tick(0), killswitch.PathProxy, "")
	if err := h.e.TrustCurrent(); err != nil {
		t.Fatal(err)
	}
	direct(t, h.tick(time.Second))
}

func TestUpdateConfigKeepsOnlyExistingBaselines(t *testing.T) {
	h := newHarness(t, nil)
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(0))
	next := h.e.Config()
	next.Baselines = map[string]string{"Home": "1.2.3.4", "Fake": "5.6.7.8"}
	if err := h.e.UpdateConfig(next, nil); err != nil {
		t.Fatal(err)
	}
	if b := h.e.Config().Baselines; b["Home"] != "203.0.113.7" || b["Fake"] != "" {
		t.Fatalf("基准只能删除，不能通过接口改写：%v", b)
	}
	next.Baselines = map[string]string{}
	if err := h.e.UpdateConfig(next, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.e.Config().Baselines) != 0 {
		t.Fatal("应能删除基准")
	}
	next.ProbeURLs = []string{"https://160.79.104.10/cdn-cgi/trace"}
	if err := h.e.UpdateConfig(next, []string{"160.79.104.0/23"}); err == nil {
		t.Fatal("探测地址在拦截的网段里应被拒绝")
	}
}

// ---- 与 Kill Switch 联动 ----

type healthyProber struct {
	mu  sync.Mutex
	tun bool
}

func (p *healthyProber) Probe(context.Context, string, string, string, []string) killswitch.Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	return killswitch.Health{DNSOK: true, TUNOK: p.tun, Interface: "en0"}
}

func TestKillSwitchFollowsVerdict(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	backend := &killswitch.FakeBackend{Ifaces: []string{"en0"}}
	kp := &healthyProber{}
	ks, err := killswitch.NewEngine(filepath.Join(t.TempDir(), "ks.json"), backend, kp, nil)
	if err != nil {
		t.Fatal(err)
	}
	ks.SetPolicy(h.e, "127.0.0.1:10053")
	step := func(advance time.Duration) killswitch.Status {
		h.tick(advance)
		ks.ProbeOnce(ctx)
		ks.Reconcile(ctx)
		return ks.Status()
	}

	// 不在受信网络：经内核，DNS 交给 Mihomo。
	h.detector.set(Network{Interface: "en7"})
	step(0)
	if err := ks.Enable(ctx, true); err != nil {
		t.Fatal(err)
	}
	s := step(time.Second)
	if s.Verdict.Path != killswitch.PathProxy || backend.Current == nil || backend.Current.ListenPort == 10053 {
		t.Fatalf("应经内核：%+v %+v", s.Verdict, backend.Current)
	}

	// 回到受信 Wi-Fi，出口还没确认：阻断，DNS 交给阻断入口。
	h.prober.set(Egress{Error: "timeout"})
	h.detector.set(home)
	s = step(time.Second)
	if s.State != killswitch.StateHeld || backend.Current == nil || backend.Current.ListenPort != 10053 {
		t.Fatalf("应阻断：%s %+v", s.State, backend.Current)
	}

	// 出口确认：撤销规则，直连放行。
	h.prober.set(good("203.0.113.7", "US"))
	s = step(probeBlocked)
	if s.State != killswitch.StateDirect || backend.Current != nil {
		t.Fatalf("应直连：%s %+v", s.State, backend.Current)
	}
	flushed := backend.Flushed

	// 规则被别人写回来也会撤销。
	_ = backend.Apply(ctx, killswitch.Desired{Suffixes: []string{"claude.ai"}})
	if s = step(time.Second); backend.Current != nil || s.State != killswitch.StateDirect {
		t.Fatalf("受信网络上残留的规则应被撤销：%s", s.State)
	}

	// TUN 接管：经内核。
	kp.mu.Lock()
	kp.tun = true
	kp.mu.Unlock()
	h.tun = true
	s = step(time.Second)
	if s.Verdict.Path != killswitch.PathProxy || backend.Current == nil {
		t.Fatalf("TUN 接管后应经内核：%+v", s.Verdict)
	}
	if backend.Flushed == flushed {
		t.Fatal("换路时应清理 DNS 缓存")
	}

	// 关闭 Kill Switch：不管走哪条路都撤销规则。
	if err := ks.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	if backend.Current != nil || ks.Status().State != killswitch.StateOff {
		t.Fatal("关闭后应撤销规则")
	}
}

func TestEnableOnHoldNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.detector.set(home)
	h.prober.set(Egress{Error: "timeout"})
	h.tick(0)
	ks, err := killswitch.NewEngine(filepath.Join(t.TempDir(), "ks.json"), &killswitch.FakeBackend{Ifaces: []string{"en0"}}, &healthyProber{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ks.SetPolicy(h.e, "127.0.0.1:10053")
	var notReady *killswitch.NotReadyError
	if err := ks.Enable(ctx, false); !errors.As(err, &notReady) {
		t.Fatalf("出口未确认时启用应要求确认，得到 %v", err)
	}
	h.prober.set(good("203.0.113.7", "US"))
	h.tick(probeBlocked)
	if err := ks.Enable(ctx, false); err != nil {
		t.Fatalf("受信网络直连时可以直接启用：%v", err)
	}
}

// ---- 配置校验 ----

func TestConfigValidation(t *testing.T) {
	cases := map[string]func(*Config){
		"出口 IP": func(c *Config) { c.ExpectedEgress = []string{"not-ip"} },
		"地区":    func(c *Config) { c.AllowedCountries = []string{"USA"} },
		"https": func(c *Config) { c.ProbeURLs = []string{"http://example.com"} },
	}
	for want, mutate := range cases {
		c := defaultConfig()
		mutate(&c)
		err := c.normalize()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：得到 %v", want, err)
		}
	}
	c := defaultConfig()
	c.TrustedSSIDs = []string{" Home ", "Home", ""}
	c.ExpectedEgress = []string{"203.0.113.7", "198.51.100.0/24"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedSSIDs) != 1 || c.ExpectedEgress[0] != "203.0.113.7/32" {
		t.Fatalf("规范化结果不对：%+v %+v", c.TrustedSSIDs, c.ExpectedEgress)
	}
}

// ---- 解析 ----

func TestParseEgress(t *testing.T) {
	trace := "fl=123\nh=chatgpt.com\nip=203.0.113.7\nts=1\nloc=us\nwarp=off\n"
	if ip, loc, ok := parseEgress(trace); !ok || ip != "203.0.113.7" || loc != "US" {
		t.Fatalf("trace：%s %s %v", ip, loc, ok)
	}
	if ip, _, ok := parseEgress("2001:db8::1\n"); !ok || ip != "2001:db8::1" {
		t.Fatalf("纯文本：%s %v", ip, ok)
	}
	if _, _, ok := parseEgress("<html>"); ok {
		t.Fatal("非 IP 内容应失败")
	}
}

func TestParseMacOutputs(t *testing.T) {
	ports := "Hardware Port: Ethernet Adapter (en4)\nDevice: en4\n\nHardware Port: Wi-Fi\nDevice: en0\nEthernet Address: x\n"
	if got := parseWiFiDevices(ports); len(got) != 1 || got[0] != "en0" {
		t.Fatalf("Wi-Fi 网卡：%v", got)
	}
	summary := "<dictionary> {\n  BSSID : aa:bb\n  InterfaceType : WiFi\n  SSID : My Home : 5G\n}\n"
	if ssid, hidden := parseIPConfigSSID(summary); ssid != "My Home : 5G" || hidden {
		t.Fatalf("SSID：%q %v", ssid, hidden)
	}
	if ssid, hidden := parseIPConfigSSID("  SSID : <redacted>\n"); ssid != "" || !hidden {
		t.Fatalf("隐藏的 SSID：%q %v", ssid, hidden)
	}
	if got := parseAirportNetwork("Current Wi-Fi Network: Home\n"); got != "Home" {
		t.Fatalf("airport：%q", got)
	}
	if got := parseAirportNetwork("You are not associated with an AirPort network.\n"); got != "" {
		t.Fatalf("未连接：%q", got)
	}
}

func TestParseNetsh(t *testing.T) {
	out := "\r\n系统上有 1 个接口:\r\n\r\n    名称                   : WLAN\r\n    描述                   : Intel\r\n    状态                   : 已连接\r\n    SSID                   : 家里\r\n    BSSID                  : aa:bb:cc:dd:ee:ff\r\n"
	got := parseNetshInterfaces(out)
	if len(got) != 1 || got[0].Name != "WLAN" || got[0].SSID != "家里" {
		t.Fatalf("netsh：%+v", got)
	}
}

// ---- 阻断用的 DNS ----

func TestBlackholeRefuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.LocalAddr().String()
	l.Close()
	if err := ServeHold(ctx, addr); err != nil {
		t.Skip("端口不可用：", err)
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", addr)
	}}
	qctx, qcancel := context.WithTimeout(ctx, 2*time.Second)
	defer qcancel()
	start := time.Now()
	if _, err := r.LookupHost(qctx, "claude.ai"); err == nil {
		t.Fatal("应解析失败")
	}
	if time.Since(start) > time.Second {
		t.Fatal("应立即失败，而不是等超时")
	}

	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'c', 'l', 'a', 'u', 'd', 'e', 2, 'a', 'i', 0, 0, 1, 0, 1}
	resp := refuse(q)
	if binary.BigEndian.Uint16(resp) != 0x1234 || resp[2]&0x80 == 0 || resp[3]&0x0f != rcodeRefused || len(resp) != len(q) {
		t.Fatalf("应答不对：%x", resp)
	}
}

// gatedProber 等 release 才返回，用来模拟探测进行中网络变了。
type gatedProber struct {
	release chan struct{}
	eg      Egress
}

func (g *gatedProber) Probe(ctx context.Context, _ []string) Egress {
	select {
	case <-g.release:
		return g.eg
	case <-ctx.Done():
		return Egress{Error: "已取消", CheckedAt: time.Now()}
	}
}

func TestProbeRunsInBackgroundAndDropsStaleResult(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TrustedSSIDs = []string{"Home", "Home-5G"} })
	g := &gatedProber{release: make(chan struct{}), eg: good("203.0.113.7", "US")}
	g.eg.CheckedAt = h.clock
	h.e.prober = g
	changes := 0
	h.e.OnNetworkChange(func() { changes++ })
	ctx := context.Background()

	h.detector.set(home)
	h.e.Tick(ctx, false) // 探测在后台，不挡住网络检查
	expect(t, h.e.Verdict(false), killswitch.PathHold, "正在探测")
	if !h.e.Verdict(false).Pending {
		t.Fatal("等探测结果时应标记为 Pending")
	}

	// 探测中换了 Wi-Fi：旧探测被取消，结果丢弃，新网络重新探测。
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Home-5G"})
	h.e.Tick(ctx, false)
	if changes != 1 {
		t.Fatalf("网络变化回调 %d 次，应为 1", changes)
	}
	close(g.release)
	h.e.probeWG.Wait()
	h.e.Tick(ctx, false)
	if st := h.e.Status(); st.Egress.Error != "" || !st.Egress.OK {
		t.Fatalf("旧网络上被取消的探测不应记入：%+v", st.Egress)
	}
	direct(t, h.e.Verdict(false))
}

func TestRouteChangeHoldsImmediately(t *testing.T) {
	h := newHarness(t, nil)
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(0))
	kicked := 0
	h.e.changed = func() { kicked++ }

	// 地址没变（比如内核增删路由）：不作废。
	h.e.RouteChanged()
	direct(t, h.e.Verdict(false))
	if kicked != 0 {
		t.Fatal("网卡和地址没变时不应通知")
	}

	// 网卡或地址变了：不等读 SSID，立即按「正在确认」阻断并通知 Kill Switch。
	h.e.mu.Lock()
	h.e.quick = "stale"
	h.e.mu.Unlock()
	h.e.RouteChanged()
	v := h.e.Verdict(false)
	expect(t, v, killswitch.PathHold, "网络变化")
	if !v.Pending || kicked != 1 {
		t.Fatalf("应按正在确认阻断并通知：%+v，通知 %d 次", v, kicked)
	}

	// 读出新网络是不受信的 Wi-Fi：经内核。
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	expect(t, h.tick(time.Second), killswitch.PathProxy, "不受信")
}

func TestParseTraceColo(t *testing.T) {
	if got := traceField("fl=1\nip=203.0.113.7\ncolo=HKG\nloc=HK\n", "colo"); got != "HKG" {
		t.Fatalf("colo = %q", got)
	}
}

func TestScanCoversEverySiteAndPath(t *testing.T) {
	sites := []ScanSite{{Name: "A", URL: "https://a/cdn-cgi/trace"}, {Name: "B", URL: "https://b/cdn-cgi/trace"}}
	paths := []ScanPath{NewScanPath("v4", "IPv4", "", nil, "tcp4"), NewScanPath("v6", "IPv6", "", nil, "tcp6")}
	r := Scan(context.Background(), sites, paths, func(p ScanPath) EgressProber {
		return &fakeProber{eg: good("203.0.113.7", p.Key), now: time.Now}
	})
	if len(r.Results) != 4 {
		t.Fatalf("应有 4 个结果：%v", r.Results)
	}
	for _, x := range r.Results {
		if x.Egress.Country != x.Path {
			t.Fatalf("结果对应的路径不对：%+v", x)
		}
	}
}

// DNS 防泄漏据此让路给路由盒（BXC-D0-002）：只看 Wi-Fi 名称，不等出口探测；网络没确认时偏向拦截。
func TestOnTrustedNetwork(t *testing.T) {
	h := newHarness(t, nil)
	if h.e.OnTrustedNetwork() {
		t.Fatal("还没读到网络时不算受信")
	}
	h.detector.set(home)
	h.tun = true // TUN 接管时判断总是经内核，但仍要认出路由盒网络
	h.e.Tick(context.Background(), h.tun)
	if !h.e.OnTrustedNetwork() {
		t.Fatal("在受信 Wi-Fi 上应为 true，不等出口探测")
	}
	h.e.mu.Lock()
	h.e.unsettled = true // 路由刚变化，还没重新识别网络
	h.e.mu.Unlock()
	if h.e.OnTrustedNetwork() {
		t.Fatal("网络未确认时应为 false")
	}
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	h.e.Tick(context.Background(), h.tun)
	if h.e.OnTrustedNetwork() {
		t.Fatal("不受信的 Wi-Fi 上应为 false")
	}
}
