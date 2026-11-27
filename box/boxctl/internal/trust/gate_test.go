package trust

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"boxctl/internal/killswitch"
)

// fakeUpstream 是一个 HTTP CONNECT 入口：建立隧道后先写自己的名字，再原样回显。
func fakeUpstream(t *testing.T, name string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"+name+"|"+req.Host+"|")
				_, _ = io.Copy(c, br)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

type gateHarness struct {
	*harness
	gate   *Gate
	addr   string
	direct int
	proxy  int
	svc    *fakeProber // 各服务自己的出口探测（经 DIRECT 入口）
}

func newGateHarness(t *testing.T) *gateHarness {
	h := newHarness(t, func(c *Config) { c.GateDirect = true })
	g := &gateHarness{harness: h, direct: fakeUpstream(t, "direct"), proxy: fakeUpstream(t, "proxy")}
	g.svc = &fakeProber{now: func() time.Time { return h.clock }}
	directURL := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(g.direct)}
	h.e.SetGate(func() *url.URL { return directURL },
		func() map[string]string { return map[string]string{"Claude": "https://claude.ai/cdn-cgi/trace"} })
	// 网络级的探测（经 DIRECT 入口）和各服务的探测分开：前者用 h.prober，后者用 g.svc。
	h.e.viaProxy = func(*url.URL) EgressProber { return gateProber{network: h.prober, svc: g.svc} }
	g.gate = NewGate(h.e, "pw", func() (int, map[string]int) { return g.direct, map[string]int{"Claude": g.proxy} }, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.addr = ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := g.gate.Serve(ctx, g.addr); err != nil {
		t.Fatal(err)
	}
	h.tun = true
	return g
}

// gateProber 按地址分流：服务自己的 trace 走 svc，其他走 network。
type gateProber struct {
	network, svc EgressProber
	svcURL       string // 为空时是 https://claude.ai/cdn-cgi/trace
}

func (p gateProber) Probe(ctx context.Context, urls []string) Egress {
	u := p.svcURL
	if u == "" {
		u = "https://claude.ai/cdn-cgi/trace"
	}
	if len(urls) == 1 && urls[0] == u {
		return p.svc.Probe(ctx, urls)
	}
	return p.network.Probe(ctx, urls)
}

// dial 经闸门连 claude.ai:443，返回上游写回的名字（direct 或 proxy）。
func (g *gateHarness) dial(t *testing.T, user, pass string) string {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", g.addr, &proxy.Auth{User: user, Password: pass}, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", "claude.ai:443")
	if err != nil {
		return "error"
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	var got string
	buf := make([]byte, 64)
	for !strings.HasSuffix(got, "|ping") {
		n, err := c.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("读隧道失败：%v（已读 %q）", err, got)
		}
	}
	name, rest, _ := cut(got, "|")
	if host, echo, _ := cut(rest, "|"); host != "claude.ai:443" || echo != "ping" {
		t.Fatalf("隧道内容不对：%q", got)
	}
	return name
}

func cut(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func TestGateDirectOnlyAfterPerServiceCheck(t *testing.T) {
	g := newGateHarness(t)
	g.detector.set(home)
	g.prober.set(good("203.0.113.7", "US"))
	g.svc.set(good("198.51.100.9", "US"))
	direct(t, g.tick(0))

	// 第一次：记住这个 Wi-Fi 上 Claude 的出口，直连。
	if got := g.dial(t, "Claude", "pw"); got != "direct" {
		t.Fatalf("应直连，得到 %s", got)
	}
	if g.e.Config().Baselines["Home#Claude"] != "198.51.100.9" {
		t.Fatalf("应记住 Claude 的出口：%v", g.e.Config().Baselines)
	}
	calls := g.svc.calls
	// TTL 内复用结果，不再探测。
	g.dial(t, "Claude", "pw")
	if g.svc.calls != calls {
		t.Fatal("TTL 内不应重新探测")
	}

	// 过期后同步重测：出口变了 → 改经代理。
	g.clock = g.clock.Add(gateTTL)
	g.svc.set(good("192.0.2.1", "US"))
	if got := g.dial(t, "Claude", "pw"); got != "proxy" {
		t.Fatalf("出口变化应经代理，得到 %s", got)
	}
	// 探测失败也经代理。
	g.clock = g.clock.Add(gateTTL)
	g.svc.set(Egress{Error: "timeout"})
	if got := g.dial(t, "Claude", "pw"); got != "proxy" {
		t.Fatalf("探测失败应经代理，得到 %s", got)
	}
	st := g.e.Status().Gate
	if !st.Enabled || !st.Active || len(st.Groups) != 1 || st.Groups[0].Direct != 2 || st.Groups[0].Proxied != 2 {
		t.Fatalf("闸门状态：%+v", st)
	}
}

func TestGateFailsClosedOnNetworkChange(t *testing.T) {
	g := newGateHarness(t)
	g.detector.set(home)
	g.prober.set(good("203.0.113.7", "US"))
	g.svc.set(good("198.51.100.9", "US"))
	direct(t, g.tick(0))
	if got := g.dial(t, "Claude", "pw"); got != "direct" {
		t.Fatalf("应直连，得到 %s", got)
	}

	// 路由变了（还没读出新 SSID）：立即不再直连，即使服务的确认还在 TTL 内。
	g.e.mu.Lock()
	g.e.quick = "stale"
	g.e.mu.Unlock()
	g.e.RouteChanged()
	if v := g.e.Verdict(true); v.Path != killswitch.PathHold || !v.Pending {
		t.Fatalf("网络变化后应先阻断：%+v", v)
	}
	if got := g.dial(t, "Claude", "pw"); got != "proxy" {
		t.Fatalf("网络变化后应经代理，得到 %s", got)
	}

	// 换到不受信的网络：经代理。
	g.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	expect(t, g.tick(time.Second), killswitch.PathProxy, "不受信")
	if got := g.dial(t, "Claude", "pw"); got != "proxy" {
		t.Fatalf("不受信网络应经代理，得到 %s", got)
	}
}

func TestGateRejectsBadAuthAndOffMode(t *testing.T) {
	g := newGateHarness(t)
	g.detector.set(home)
	g.prober.set(good("203.0.113.7", "US"))
	g.svc.set(good("198.51.100.9", "US"))
	direct(t, g.tick(0))
	if got := g.dial(t, "Claude", "wrong"); got != "error" {
		t.Fatalf("口令错误应拒绝，得到 %s", got)
	}
	// 关掉闸门直连：TUN 接管时按经内核处理，闸门只经代理。
	cfg := g.e.Config()
	cfg.GateDirect = false
	if err := g.e.UpdateConfig(cfg, nil); err != nil {
		t.Fatal(err)
	}
	expect(t, g.tick(time.Second), killswitch.PathProxy, "TUN")
	if got := g.dial(t, "Claude", "pw"); got != "proxy" {
		t.Fatalf("关闭后应经代理，得到 %s", got)
	}
}

// 路由盒接管（ADR-082）：受保护服务直接经 DIRECT 交给路由盒，不逐服务探测；服务自己的探测失败也放行。
func TestGateOnBoxSkipsServiceProbe(t *testing.T) {
	g := newGateHarness(t)
	g.e.cfg.TrustedSSIDs = nil
	f := &fakeFinder{}
	f.set(homeBox, nil)
	g.e.findBox = f.find
	g.e.SetBoxDiscovery(func() string { return "s3cret" }, func(string) []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("192.168.100.1")}
	})
	g.prober.set(Egress{Error: "chatgpt.com：EOF"})
	g.svc.set(Egress{Error: "claude.ai：EOF"})
	g.detector.set(Network{Interface: "en7"})
	direct(t, g.settle())
	if got := g.dial(t, "Claude", "pw"); got != "direct" {
		t.Fatalf("路由盒上受保护服务应直连交给路由盒：%s", got)
	}
}
