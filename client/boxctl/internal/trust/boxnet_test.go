package trust

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"boxctl/internal/boxfind"
	"boxctl/internal/killswitch"
)

// fakeFinder 是可控的路由盒发现结果。
type fakeFinder struct {
	mu    sync.Mutex
	box   boxfind.Box
	err   error
	calls int
	seen  []netip.Addr
}

func (f *fakeFinder) find(_ context.Context, servers []netip.Addr, secret string) (boxfind.Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = servers
	if secret != "s3cret" {
		return boxfind.Box{}, boxfind.ErrNotFound
	}
	return f.box, f.err
}

func (f *fakeFinder) set(b boxfind.Box, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.box, f.err = b, err
}

var homeBox = boxfind.Box{ID: "home-box", Addr: netip.MustParseAddrPort("192.168.100.1:19443")}

// boxHarness 是没有受信 Wi-Fi、只靠路由盒发现的判断器。
func boxHarness(t *testing.T) (*harness, *fakeFinder) {
	t.Helper()
	h := newHarness(t, func(c *Config) { c.TrustedSSIDs = nil })
	f := &fakeFinder{}
	h.e.findBox = f.find
	h.e.SetBoxDiscovery(func() string { return "s3cret" }, func(string) []netip.Addr {
		// 198.18.0.2 是 TUN 里的地址，要被过滤掉。
		return []netip.Addr{netip.MustParseAddr("198.18.0.2"), netip.MustParseAddr("192.168.100.1")}
	})
	return h, f
}

// settle 连续检查几次，让发现、出口探测都跑完。
func (h *harness) settle() killswitch.Verdict {
	h.tick(time.Second)
	return h.tick(time.Second)
}

func TestBoxDiscoveryTrustsWiredNetwork(t *testing.T) {
	h, f := boxHarness(t)
	f.set(homeBox, nil)
	h.prober.set(good("203.0.113.7", "US"))
	// 有线网络、没有受信 Wi-Fi：找到路由盒就算受信网络。
	h.detector.set(Network{Interface: "en7"})
	v := h.settle()
	expect(t, v, killswitch.PathDirect, "路由盒「home-box」")
	if len(f.seen) != 1 || f.seen[0].String() != "192.168.100.1" {
		t.Fatalf("DNS 服务器没过滤 TUN 地址：%v", f.seen)
	}
	// 出口基准按路由盒记。
	if got := h.e.Config().Baselines["路由盒:home-box"]; got != "203.0.113.7" {
		t.Fatalf("baselines = %v", h.e.Config().Baselines)
	}
	if st := h.e.Status(); !st.Trusted || !st.Enabled || st.Box.ID != "home-box" {
		t.Fatalf("status = %+v", st)
	}
	if !h.e.OnTrustedNetwork() {
		t.Fatal("路由盒网络上应委托路由盒 DNS")
	}
}

func TestBoxDiscoveryNotFound(t *testing.T) {
	h, f := boxHarness(t)
	f.set(boxfind.Box{}, boxfind.ErrNotFound)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(Network{Interface: "en7"})
	expect(t, h.settle(), killswitch.PathProxy, "没有发现路由盒")
	// 没找到时过一会儿再找。
	calls := f.calls
	h.tick(boxMissRetry)
	if f.calls == calls {
		t.Fatal("没找到时应该定时重找")
	}
}

func TestBoxDiscoveryKeepsBoxOnSingleFailure(t *testing.T) {
	h, f := boxHarness(t)
	f.set(homeBox, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(Network{Interface: "en7"})
	direct(t, h.settle())

	// 复查失败一次：先不切走（闸门的逐连接确认兜底）；连续两次才认为路由盒不在了。
	f.set(boxfind.Box{}, boxfind.ErrNotFound)
	direct(t, h.tick(boxRecheck))
	expect(t, h.tick(boxRecheck), killswitch.PathProxy, "没有发现路由盒")
}

func TestBoxDiscoveryNetworkChangeForgetsBox(t *testing.T) {
	h, f := boxHarness(t)
	f.set(homeBox, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(Network{Interface: "en7"})
	direct(t, h.settle())

	// 换到别的网络：发现结果作废，那里没有路由盒就经代理。
	f.set(boxfind.Box{}, boxfind.ErrNotFound)
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	expect(t, h.settle(), killswitch.PathProxy, "没有发现路由盒")
}

// 路由盒接管（ADR-082）：出口变化、探测失败都不阻断，按路由盒的策略走；说明里带上探测结果。
func TestBoxDiscoveryIgnoresEgress(t *testing.T) {
	h, f := boxHarness(t)
	f.set(homeBox, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(Network{Interface: "en7"})
	direct(t, h.settle())
	h.prober.set(good("198.51.100.9", "CN"))
	expect(t, h.tick(probeAllowed), killswitch.PathDirect, "出口 198.51.100.9")
	h.prober.set(Egress{Error: "chatgpt.com：EOF"})
	expect(t, h.tick(probeAllowed), killswitch.PathDirect, "出口探测失败（chatgpt.com：EOF）")
	if !h.e.OnBox() {
		t.Fatal("应在路由盒网络上")
	}
}

func TestBoxDiscoveryOffWithoutSecret(t *testing.T) {
	h, f := boxHarness(t)
	h.e.SetBoxDiscovery(func() string { return "" }, func(string) []netip.Addr { return nil })
	f.set(homeBox, nil)
	h.detector.set(Network{Interface: "en7"})
	h.settle()
	if f.calls != 0 {
		t.Fatal("没有口令时不应查找路由盒")
	}
	if st := h.e.Status(); st.Enabled || st.Box.Enabled {
		t.Fatalf("status = %+v", st)
	}
}

// 受信 Wi-Fi 名单保留：在受信 Wi-Fi 上没有路由盒也照样直连。
func TestBoxDiscoveryKeepsTrustedWiFi(t *testing.T) {
	h := newHarness(t, nil)
	f := &fakeFinder{}
	h.e.findBox = f.find
	h.e.SetBoxDiscovery(func() string { return "s3cret" }, func(string) []netip.Addr { return nil })
	f.set(boxfind.Box{}, boxfind.ErrNotFound)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(home)
	direct(t, h.settle())
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	expect(t, h.settle(), killswitch.PathProxy, "没有发现路由盒；Wi-Fi「Cafe」不受信")
}

// 路由盒的代理入口用不了（旧版路由盒、口令不一致）：不算路由盒网络，走本机代理（ADR-084）。
func TestBoxDiscoveryRequiresProxy(t *testing.T) {
	h, f := boxHarness(t)
	h.e.boxProxy = func(context.Context, netip.Addr, string) error {
		return errors.New("连不上路由盒的代理入口 192.168.100.1:19444")
	}
	f.set(homeBox, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(Network{Interface: "en0"})
	expect(t, h.settle(), killswitch.PathProxy, "代理入口")
	if h.e.OnBox() {
		t.Fatal("不应认为在路由盒网络上")
	}
}

// 每次验证通过路由盒都报一次客户端信息（发到发现端口）；验证不过时不报。
func TestBoxHello(t *testing.T) {
	h, f := boxHarness(t)
	var mu sync.Mutex
	var sent []netip.AddrPort
	h.e.SetBoxHello(func() boxfind.Hello { return boxfind.Hello{Name: "mac", OS: "macOS 15", Version: "0.2.22"} })
	h.e.sendHello = func(_ context.Context, addr netip.AddrPort, secret string, hello boxfind.Hello) error {
		mu.Lock()
		defer mu.Unlock()
		if secret != "s3cret" || hello.Name != "mac" {
			t.Errorf("hello = %+v secret = %q", hello, secret)
		}
		sent = append(sent, addr)
		return errors.New("旧版路由盒") // 失败不影响发现结果
	}
	count := func() int {
		h.e.probeWG.Wait()
		mu.Lock()
		defer mu.Unlock()
		return len(sent)
	}
	f.set(homeBox, nil)
	h.prober.set(good("203.0.113.7", "US"))
	h.detector.set(Network{Interface: "en7"})
	expect(t, h.settle(), killswitch.PathDirect, "路由盒「home-box」")
	if n := count(); n != 1 || sent[0] != homeBox.Addr {
		t.Fatalf("sent = %v", sent)
	}
	// 30 秒后复查：再报一次。
	h.tick(boxRecheck)
	if n := count(); n != 2 {
		t.Fatalf("after recheck sent = %d", n)
	}
	// 验证不过：不报。
	f.set(boxfind.Box{}, errors.New("验证不通过"))
	h.tick(boxRecheck)
	if n := count(); n != 2 {
		t.Fatalf("failed verify sent = %d", n)
	}
}
