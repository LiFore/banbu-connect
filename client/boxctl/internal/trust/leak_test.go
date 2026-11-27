package trust

import (
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"boxctl/internal/killswitch"
)

// 睡眠唤醒：墙上时间比单调时间多走了一截（macOS 睡眠时单调时间不走），睡前的出口确认作废，先阻断再重新确认。
func TestWakeInvalidatesEgress(t *testing.T) {
	h := newHarness(t, nil)
	h.detector.set(home)
	h.prober.set(good("203.0.113.7", "US"))
	wall, mono := time.Unix(1_800_000_000, 0), time.Unix(0, 0)
	h.e.clock = func() (time.Time, time.Time) { return wall, mono }
	direct(t, h.tick(0))

	// 正常的两次检查之间：不算唤醒。
	wall, mono = wall.Add(3*time.Second), mono.Add(3*time.Second)
	if h.e.checkWake() {
		t.Fatal("3 秒的间隔不应算唤醒")
	}

	// 墙上时间走了 8 小时，单调时间只走了 1 秒：睡过了。
	wall, mono = wall.Add(8*time.Hour), mono.Add(time.Second)
	kicked := 0
	h.e.changed = func() { kicked++ }
	if !h.e.checkWake() {
		t.Fatal("应判断为唤醒")
	}
	expect(t, h.e.Verdict(false), killswitch.PathHold, "网络变化")
	if kicked != 1 {
		t.Fatalf("唤醒应通知 Kill Switch，通知了 %d 次", kicked)
	}
	// 重新识别网络、重新探测后恢复直连。
	calls := h.prober.calls
	direct(t, h.tick(time.Second))
	if h.prober.calls == calls {
		t.Fatal("唤醒后应重新探测出口")
	}
}

// 单调时间也含睡眠的系统（Windows）：两次检查隔了很久也按唤醒处理。
func TestWakeByLongGap(t *testing.T) {
	if woke(3*time.Second, 3*time.Second) || woke(12*time.Second, 12*time.Second) {
		t.Fatal("正常间隔、检查偶尔慢一点不应算唤醒")
	}
	if !woke(wakeGap, wakeGap) || !woke(time.Hour, time.Second) {
		t.Fatal("长间隔、墙上时间跳变应算唤醒")
	}
}

// 同一个 Wi-Fi 名称、同一张网卡，但网关的 MAC 变了（同网段的另一台路由器）：按换网络处理。
func TestRouterChangeIsNetworkChange(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		moved      bool
	}{
		{"same", "192.168.1.1/aa:aa:aa:aa:aa:aa", "192.168.1.1/aa:aa:aa:aa:aa:aa", false},
		{"mac gone", "192.168.1.1/aa:aa:aa:aa:aa:aa", "192.168.1.1", false},
		{"mac appears", "192.168.1.1", "192.168.1.1/aa:aa:aa:aa:aa:aa", false},
		{"other mac", "192.168.1.1/aa:aa:aa:aa:aa:aa", "192.168.1.1/bb:bb:bb:bb:bb:bb", true},
		{"other ip", "192.168.1.1", "192.168.2.1", true},
		{"unknown", "", "192.168.2.1", false},
	}
	for _, c := range cases {
		if got := routerChanged(c.prev, c.next); got != c.moved {
			t.Errorf("%s: routerChanged(%q, %q) = %v", c.name, c.prev, c.next, got)
		}
	}
	if got := keepRouterMAC("192.168.1.1/aa:aa:aa:aa:aa:aa", "192.168.1.1"); got != "192.168.1.1/aa:aa:aa:aa:aa:aa" {
		t.Fatalf("读不到 MAC 时应沿用上次的：%q", got)
	}

	h := newHarness(t, nil)
	n := home
	n.Router = "192.168.1.1/aa:aa:aa:aa:aa:aa"
	h.detector.set(n)
	h.prober.set(good("203.0.113.7", "US"))
	direct(t, h.tick(0))
	calls := h.prober.calls

	// 邻居条目过期，这次读不到 MAC：不算换网络，出口结果保留。
	n.Router = "192.168.1.1"
	h.detector.set(n)
	direct(t, h.tick(time.Second))
	if h.prober.calls != calls {
		t.Fatal("读不到 MAC 不应作废出口结果")
	}
	if h.e.Status().Network.Router != "192.168.1.1/aa:aa:aa:aa:aa:aa" {
		t.Fatalf("应沿用上次的 MAC：%q", h.e.Status().Network.Router)
	}

	// 换了一台路由器：出口结果作废，重新探测，出口变了就阻断（基准是原来那台的出口）。
	n.Router = "192.168.1.1/bb:bb:bb:bb:bb:bb"
	h.detector.set(n)
	h.prober.set(good("192.0.2.50", "CN"))
	expect(t, h.tick(time.Second), killswitch.PathHold, "出口从")
}

// 出口闸门放行的长连接：服务的出口不再符合、或者判断不再是直连时，已经建立的直连连接被断开。
func TestGateRevokesLiveDirectConns(t *testing.T) {
	g := newGateHarness(t)
	g.detector.set(home)
	g.prober.set(good("203.0.113.7", "US"))
	g.svc.set(good("198.51.100.9", "US"))
	direct(t, g.tick(0))

	c := g.open(t)
	defer c.Close()
	waitLive(t, g.gate, 1)

	// 过期后重新确认，出口变了：直连连接被断开。
	g.clock = g.clock.Add(gateTTL)
	g.svc.set(good("192.0.2.1", "US"))
	if got := g.dial(t, "Claude", "pw"); got != "proxy" {
		t.Fatalf("出口变化应经代理，得到 %s", got)
	}
	expectClosed(t, c)
	waitLive(t, g.gate, 0)

	// Revoke("") 断开全部（判断从直连变为其他时由 boxRouter 调用）。
	g.clock = g.clock.Add(gateTTL)
	g.svc.set(good("198.51.100.9", "US"))
	c2 := g.open(t)
	defer c2.Close()
	waitLive(t, g.gate, 1)
	if n := g.gate.Revoke(""); n != 1 {
		t.Fatalf("应断开 1 条，断开了 %d 条", n)
	}
	expectClosed(t, c2)
}

// open 经闸门建立一条到 claude.ai 的隧道并读完上游的开头，返回还开着的连接。
func (g *gateHarness) open(t *testing.T) interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
} {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", g.addr, &proxy.Auth{User: "Claude", Password: "pw"}, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", "claude.ai:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "direct|") {
		t.Fatalf("应经直连：%q %v", buf[:n], err)
	}
	_ = c.SetDeadline(time.Time{})
	return c
}

func waitLive(t *testing.T, g *Gate, want int) {
	t.Helper()
	for range 100 {
		g.mu.Lock()
		n := 0
		for _, set := range g.live {
			n += len(set)
		}
		g.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("经闸门直连的连接数应为 %d", want)
}

func expectClosed(t *testing.T, c interface {
	io.Reader
	SetDeadline(time.Time) error
}) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 8)); err == nil {
		t.Fatal("连接应已断开")
	} else if strings.Contains(err.Error(), "timeout") {
		t.Fatal("连接没有被断开（读超时）")
	}
}
