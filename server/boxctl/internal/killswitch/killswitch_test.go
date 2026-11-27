package killswitch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
)

type fakeProber struct{ h Health }

func (p *fakeProber) Probe(context.Context, string, string, string, []string) Health { return p.h }

var healthy = Health{DNSOK: true, DNSDetail: "93.184.216.34", DomesticOK: true, TUNOK: true, Interface: "utun4"}

func newTestEngine(t *testing.T) (*Engine, *FakeBackend, *fakeProber) {
	t.Helper()
	b := &FakeBackend{Ifaces: []string{"en0"}}
	p := &fakeProber{h: healthy}
	e, err := NewEngine(filepath.Join(t.TempDir(), "config.json"), b, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Reconcile(context.Background())
	e.ProbeOnce(context.Background())
	return e, b, p
}

func TestNormalizeRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"注入字符":    func(c *Config) { c.Services[0].Suffixes = []string{"a.com'; rm -rf /"} },
		"顶级域":     func(c *Config) { c.Services[0].Suffixes = []string{"com"} },
		"路径穿越":    func(c *Config) { c.Services[0].Suffixes = []string{"../../etc/passwd"} },
		"网段无效":    func(c *Config) { c.Services[0].CIDRs = []string{"nope"} },
		"网段过宽 v4": func(c *Config) { c.Services[0].CIDRs = []string{"0.0.0.0/0"} },
		"网段过宽 v6": func(c *Config) { c.Services[0].CIDRs = []string{"::/0"} },
		"非回环 DNS": func(c *Config) { c.DNSListen = "8.8.8.8:53" },
		"非本机控制接口": func(c *Config) { c.Controller = "http://example.com:9097" },
		"服务名重复":   func(c *Config) { c.Services[1].Name = c.Services[0].Name },
		"服务名为空":   func(c *Config) { c.Services[0].Name = " " },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := defaultConfig()
			mutate(&c)
			if err := c.normalize(); err == nil {
				t.Fatal("应当拒绝")
			}
		})
	}
	c := defaultConfig()
	c.Services[0].Suffixes = []string{" .Example.COM ", "example.com", ""}
	c.Services[0].CIDRs = []string{"160.79.104.9/23"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Services[0].Suffixes, []string{"example.com"}) || c.Services[0].CIDRs[0] != "160.79.104.0/23" {
		t.Fatalf("规范化结果不对：%+v", c.Services[0])
	}
}

func TestEnableRefusesWithoutMihomoPath(t *testing.T) {
	e, b, p := newTestEngine(t)
	ctx := context.Background()
	for name, h := range map[string]Health{
		"DNS 不通":  {DNSOK: false, DNSDetail: "无响应", TUNOK: true},
		"TUN 未接管": {DNSOK: true, TUNOK: false, Interface: "en0"},
	} {
		p.h = h
		var notReady *NotReadyError
		if err := e.Enable(ctx, false); !errors.As(err, &notReady) {
			t.Fatalf("%s：应当拒绝，得到 %v", name, err)
		}
		if b.Current != nil || e.Status().Enabled {
			t.Fatalf("%s：拒绝后不应有任何改动", name)
		}
	}
	if err := e.Enable(ctx, true); err != nil {
		t.Fatal(err)
	}
	if b.Current == nil || !e.Status().Enabled {
		t.Fatal("force 应当启用")
	}
}

func TestStates(t *testing.T) {
	e, _, p := newTestEngine(t)
	ctx := context.Background()
	if got := e.Status().State; got != StateOff {
		t.Fatalf("got %s", got)
	}
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := e.Status().State; got != StateProtecting {
		t.Fatalf("got %s", got)
	}
	p.h = Health{DNSOK: true, TUNOK: false, Interface: "en0"}
	e.ProbeOnce(ctx)
	if got := e.Status().State; got != StateProxyOnly {
		t.Fatalf("got %s", got)
	}
	p.h = Health{DNSOK: false, DNSDetail: "无响应"}
	e.ProbeOnce(ctx)
	if got := e.Status().State; got != StateBlocking {
		t.Fatalf("got %s", got)
	}
}

// 内核挂掉时绝不能因此撤销规则，这是 Kill Switch 的核心。
func TestMihomoDownKeepsRules(t *testing.T) {
	e, b, p := newTestEngine(t)
	ctx := context.Background()
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	p.h = Health{DNSOK: false, DNSDetail: "无响应"}
	e.ProbeOnce(ctx)
	e.Reconcile(ctx)
	if b.Current == nil || b.Removed != 0 {
		t.Fatal("Mihomo 不可用时规则不能被撤销")
	}
	if b.Flushed != 1 {
		t.Fatalf("故障时应清一次 DNS 缓存，否则缓存的真实 IP 能绕过 DNS 层，实际 %d 次", b.Flushed)
	}
	p.h = healthy
	e.ProbeOnce(ctx)
	if b.Flushed != 2 {
		t.Fatalf("恢复后应再清一次 DNS 缓存，实际 %d 次", b.Flushed)
	}
}

func TestReconcileRestoresRules(t *testing.T) {
	e, b, _ := newTestEngine(t)
	ctx := context.Background()
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	b.Current = nil // 规则被删
	e.Reconcile(ctx)
	if b.Current == nil {
		t.Fatal("规则被删后应当恢复")
	}
	b.Ifaces = []string{"en0", "en5"} // 新接入网卡
	e.Reconcile(ctx)
	if !slices.Contains(b.Current.Interfaces, "en5") {
		t.Fatal("新网卡应当被覆盖")
	}
	if s := e.Status(); !s.Rules.InSync || s.LastError != "" {
		t.Fatalf("恢复后应当一致：%+v", s)
	}
}

func TestReconcileReportsFailureAndRetries(t *testing.T) {
	e, b, _ := newTestEngine(t)
	ctx := context.Background()
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	b.Current = nil
	b.FailApply = errors.New("boom")
	e.Reconcile(ctx)
	if got := e.Status().State; got != StateError {
		t.Fatalf("got %s", got)
	}
	b.FailApply = nil
	e.Reconcile(ctx)
	if s := e.Status(); s.State != StateProtecting || s.LastError != "" {
		t.Fatalf("应当恢复：%+v", s)
	}
}

func TestDisabledCleansLeftovers(t *testing.T) {
	e, b, _ := newTestEngine(t)
	b.Current = &Desired{Suffixes: []string{"claude.ai"}}
	e.Reconcile(context.Background())
	if b.Current != nil {
		t.Fatal("未启用时应当清理残留规则")
	}
}

func TestDisableWorksWithMihomoDown(t *testing.T) {
	e, b, p := newTestEngine(t)
	ctx := context.Background()
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	p.h = Health{}
	if err := e.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Current != nil || e.Status().State != StateOff {
		t.Fatal("应当已撤销")
	}
}

func TestUpdateConfigAppliesAndRollsBack(t *testing.T) {
	e, b, _ := newTestEngine(t)
	ctx := context.Background()
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	next := e.Config()
	next.Services[1].Suffixes = append(next.Services[1].Suffixes, "sora.com")
	if err := e.UpdateConfig(ctx, next); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.Current.Suffixes, "sora.com") {
		t.Fatal("启用状态下应当立即生效")
	}

	bad := e.Config()
	bad.Services[0].Suffixes = []string{"bad domain"}
	var invalid *ValidationError
	if err := e.UpdateConfig(ctx, bad); !errors.As(err, &invalid) {
		t.Fatalf("got %v", err)
	}

	// 应用失败：配置不变。
	before := e.Config()
	next = e.Config()
	next.Services[1].Suffixes = []string{"other.example.com"}
	b.FailApply = errors.New("boom")
	if err := e.UpdateConfig(ctx, next); err == nil {
		t.Fatal("应当失败")
	}
	b.FailApply = nil
	if !slices.Equal(e.Config().Services[1].Suffixes, before.Services[1].Suffixes) {
		t.Fatal("失败后配置不应改变")
	}
}

func TestConfigPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	b := &FakeBackend{Ifaces: []string{"en0"}}
	e, err := NewEngine(path, b, &fakeProber{h: healthy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	// 重启：规则还在，启用状态也在。
	e2, err := NewEngine(path, b, &fakeProber{h: healthy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e2.Reconcile(ctx)
	if !e2.Status().Enabled || b.Applied != 1 || b.Removed != 0 {
		t.Fatalf("重启不应改动规则：applied=%d removed=%d", b.Applied, b.Removed)
	}
}

func TestNoInterfacesRefused(t *testing.T) {
	e, b, _ := newTestEngine(t)
	b.Ifaces = nil
	err := e.Enable(context.Background(), true)
	if err == nil || b.Current != nil {
		t.Fatalf("没有物理网卡时不能写防火墙规则：%v", err)
	}
}

// ---- 规则生成 ----

func TestPFRules(t *testing.T) {
	got := pfRules(Desired{CIDRs: []string{"153.61.224.0/22", "160.79.104.0/23", "2607:6bc0::/48"}, Interfaces: []string{"en0", "en5"}})
	want := "block drop out quick on { en0 en5 } inet from any to { 153.61.224.0/22 160.79.104.0/23 }\n" +
		"block drop out quick on { en0 en5 } inet6 from any to { 2607:6bc0::/48 }\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
	if got := pfRules(Desired{Interfaces: []string{"en0"}}); got != "" {
		t.Fatalf("没有网段时应为空：%q", got)
	}
}

func TestParseHardwarePorts(t *testing.T) {
	out := "Hardware Port: Wi-Fi\nDevice: en0\nEthernet Address: x\n\nHardware Port: Bridge\nDevice: bridge0\n\nHardware Port: USB\nDevice: en5\n"
	if got := parseHardwarePorts(out); !slices.Equal(got, []string{"en0", "en5"}) {
		t.Fatalf("got %v", got)
	}
}

func TestNrptNamespaces(t *testing.T) {
	if got := nrptNamespaces([]string{"claude.ai"}); !slices.Equal(got, []string{".claude.ai", "claude.ai"}) {
		t.Fatalf("got %v", got)
	}
}

// ---- 规则校验 ----

func TestJudgeSuffix(t *testing.T) {
	proxies := map[string]mihomoProxy{
		"Claude": {Type: "Selector", All: []string{"自动选择"}},
		"自动选择":   {Type: "Selector", All: []string{"HK"}},
		"HK":     {Type: "Shadowsocks"},
		"代理模式":   {Type: "Selector", All: []string{"自动选择", "DIRECT"}},
		"环A":     {Type: "Selector", All: []string{"环B"}},
		"环B":     {Type: "Selector", All: []string{"环A"}},
		"DIRECT": {Type: "Direct"},
	}
	rule := func(typ, payload, proxy string) mihomoRule {
		return mihomoRule{Type: typ, Payload: payload, Proxy: proxy}
	}
	cases := []struct {
		name, suffix string
		rules        []mihomoRule
		want         string
	}{
		{"严格规则", "claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "Claude")}, "ok"},
		{"父域名命中", "api.claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "Claude")}, "ok"},
		{"不是子域名", "notclaude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "Claude"), rule("Match", "", "DIRECT")}, "missing"},
		{"关键字命中", "claude.ai", []mihomoRule{rule("DomainKeyword", "claude", "Claude")}, "ok"},
		{"目标含 DIRECT", "claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "代理模式")}, "direct"},
		{"目标就是 DIRECT", "claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "DIRECT")}, "direct"},
		{"先命中的规则生效", "claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "DIRECT"), rule("DomainSuffix", "claude.ai", "Claude")}, "direct"},
		{"REJECT", "claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "REJECT")}, "ok"},
		{"只落到兜底", "claude.ai", []mihomoRule{rule("Match", "", "Claude")}, "missing"},
		{"没有规则", "claude.ai", nil, "missing"},
		{"组之间有环也能结束", "claude.ai", []mihomoRule{rule("DomainSuffix", "claude.ai", "环A")}, "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, detail := judgeSuffix(c.suffix, c.rules, proxies); got != c.want {
				t.Fatalf("got %s（%s）, want %s", got, detail, c.want)
			}
		})
	}
}

func TestCheckRulesUsesControllerAndSecret(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/rules":
			_, _ = w.Write([]byte(`{"rules":[{"type":"DomainSuffix","payload":"claude.ai","proxy":"Claude"}]}`))
		case "/proxies":
			_, _ = w.Write([]byte(`{"proxies":{"Claude":{"type":"Selector","all":["HK"]},"HK":{"type":"Shadowsocks"}}}`))
		}
	}))
	defer srv.Close()
	cfg := Config{Controller: srv.URL, ControllerSecret: "s3", Services: []Service{
		{Name: "Claude", Enabled: true, Group: "Claude", Suffixes: []string{"claude.ai", "new.example.com"}},
		{Name: "Off", Enabled: false, Suffixes: []string{"off.example.com"}},
	}}
	got := checkRules(context.Background(), cfg)
	if auth != "Bearer s3" {
		t.Fatalf("auth = %q", auth)
	}
	if len(got) != 2 || got[0].Status != "ok" || got[1].Status != "missing" {
		t.Fatalf("got %+v", got)
	}
	if got[1].Snippet != `"DOMAIN-SUFFIX,new.example.com,Claude",` {
		t.Fatalf("snippet = %q", got[1].Snippet)
	}
	srv.Close()
	if got := checkRules(context.Background(), cfg); got[0].Status != "unknown" {
		t.Fatalf("连不上时应为 unknown：%+v", got[0])
	}
}

func TestRoutesKeepInternalController(t *testing.T) {
	e, _, _ := newTestEngine(t)
	e.SetController("http://127.0.0.1:19190", "secret")
	mux := http.NewServeMux()
	e.Routes(mux, func(fn http.HandlerFunc) http.Handler { return fn })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/killswitch/config", nil))
	if w.Code != 200 || bytesContains(w.Body.Bytes(), "secret") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(viewOf(e.Config()))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/killswitch/config", bytesReader(body)))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := e.Config(); c.Controller == "" || c.ControllerSecret != "secret" {
		t.Fatalf("保存配置不应清掉内部控制接口：%+v", c)
	}
	var notReady struct {
		Code string `json:"code"`
	}
	bad := viewOf(e.Config())
	bad.Services[0].CIDRs = []string{"0.0.0.0/0"}
	body, _ = json.Marshal(bad)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/killswitch/config", bytesReader(body)))
	_ = json.Unmarshal(w.Body.Bytes(), &notReady)
	if w.Code != 400 || notReady.Code != "invalid" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// fakeDNS 对任何查询都返回 rcode；rcode 为 0 时附带一条真实 IP 的 A 记录。
func fakeDNS(t *testing.T, rcode byte) string {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			// 只保留问题段：查询可能带 EDNS 附加记录，答案必须紧跟在问题后面。
			end := 12
			for buf[end] != 0 {
				end += int(buf[end]) + 1
			}
			end += 5
			if end > n {
				continue
			}
			resp := append([]byte(nil), buf[:end]...)
			resp[10], resp[11] = 0, 0
			resp[2], resp[3] = 0x81, 0x80|rcode
			if rcode == 0 {
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 93, 184, 216, 34)
			}
			_, _ = c.WriteTo(resp, addr)
		}
	}()
	return c.LocalAddr().String()
}

func TestProbeDNSAcceptsRealIP(t *testing.T) {
	ok, detail := probeDNS(context.Background(), "example.com", fakeDNS(t, 0))
	if !ok || detail != "93.184.216.34" {
		t.Fatalf("真实 IP 应算正常：%v %s", ok, detail)
	}
	if ok, _ := probeDNS(context.Background(), "example.com", fakeDNS(t, 3)); !ok {
		t.Fatal("后缀没有记录时 Mihomo 仍然作答，应算正常")
	}
}

func TestTUNNotListeningIsNotTakenOver(t *testing.T) {
	enable := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tun": map[string]any{"enable": enable}})
	}))
	defer srv.Close()
	e, _, _ := newTestEngine(t)
	e.SetController(srv.URL, "")
	ctx := context.Background()
	if h := e.probe(ctx); h.TUNOK {
		t.Fatal("内核里 TUN 没运行时，出口不是物理网卡也不算接管（可能是其他 VPN）")
	}
	enable = true
	if h := e.probe(ctx); !h.TUNOK {
		t.Fatal("内核里 TUN 在运行时应算接管")
	}
}
