package killswitch

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDNSGuardFollowsTUN(t *testing.T) {
	ctx := context.Background()
	e, _, p := newTestEngine(t)
	g := &FakeGuard{}
	var on atomic.Bool
	on.Store(true)
	e.SetDNSGuard(g, on.Load)

	e.ProbeOnce(ctx)
	if !g.On || e.Status().DNSGuard.Active != true || g.Ifaces[0] != "en0" {
		t.Fatalf("TUN 接管时应拦截物理网卡上的 DNS：%+v", e.Status().DNSGuard)
	}

	// TUN 掉了：必须撤销，否则整机无法解析域名。
	p.h.TUNOK = false
	e.ProbeOnce(ctx)
	if g.On || e.Status().DNSGuard.Active {
		t.Fatal("TUN 未接管时应撤销拦截")
	}

	// 设置里关掉：TUN 接管也不拦截。
	p.h.TUNOK = true
	on.Store(false)
	e.ProbeOnce(ctx)
	if g.On || e.Status().DNSGuard.Enabled {
		t.Fatal("关闭设置后不应拦截")
	}

	// 写入失败：报告错误，不留半套规则。
	on.Store(true)
	g.Fail = errors.New("pf 不可用")
	e.ProbeOnce(ctx)
	st := e.Status().DNSGuard
	if g.On || st.Active || !strings.Contains(st.Error, "pf 不可用") {
		t.Fatalf("写入失败应报告：%+v", st)
	}
	g.Fail = nil
	e.ProbeOnce(ctx)
	if st = e.Status().DNSGuard; !st.Active || st.Error != "" {
		t.Fatalf("恢复后应生效并清除错误：%+v", st)
	}
}

// 受信网络（路由盒）上 DNS 交给路由盒：路由盒拦公共 DoH，拦了它的 DNS 整机就无法解析（BXC-D0-002）。
func TestDNSGuardDelegatedOnTrustedNetwork(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEngine(t)
	g := &FakeGuard{}
	var trusted atomic.Bool
	e.SetDNSGuard(g, func() bool { return true })
	e.SetDNSDelegate(trusted.Load)

	e.ProbeOnce(ctx)
	if !g.On {
		t.Fatal("不在受信网络上应拦截")
	}

	// 连上路由盒：换网络触发的核对（Reconcile）就要撤销，不等下一轮健康检查。
	trusted.Store(true)
	e.Reconcile(ctx)
	st := e.Status().DNSGuard
	if g.On || st.Active || !st.Delegated || !st.Enabled {
		t.Fatalf("受信网络上应交给路由盒：%+v", st)
	}

	// 离开受信网络：立即恢复拦截。
	trusted.Store(false)
	e.Reconcile(ctx)
	if st = e.Status().DNSGuard; !g.On || !st.Active || st.Delegated {
		t.Fatalf("离开受信网络应恢复拦截：%+v", st)
	}
}

func TestDomesticDNSEvents(t *testing.T) {
	ctx := context.Background()
	e, _, p := newTestEngine(t)
	p.h.DomesticOK = false
	p.h.DomesticDetail = "国内域名解析失败"
	e.ProbeOnce(ctx) // 第一次检查就失败也要提示
	p.h.DomesticOK = true
	e.ProbeOnce(ctx)
	var warned, recovered bool
	for _, ev := range e.Status().Events {
		warned = warned || ev.Message == "国内域名解析失败"
		recovered = recovered || ev.Message == "国内域名解析已恢复"
	}
	if !warned || !recovered {
		t.Fatalf("应记录失败和恢复：%+v", e.Status().Events)
	}
}

func TestDNSGuardClearedOnStartAndExit(t *testing.T) {
	e, _, _ := newTestEngine(t)
	g := &FakeGuard{On: true} // 上次崩溃留下的
	e.SetDNSGuard(g, func() bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.Run(ctx)
	if g.On {
		t.Fatal("启动时应撤销残留的拦截")
	}
}

func TestGuardPFRules(t *testing.T) {
	got := guardPFRules([]string{"en0", "en7"})
	want := "pass out quick on { en0 en7 } proto udp from any port 19184:19191 to any port 53\n" +
		"block drop out quick on { en0 en7 } proto { tcp, udp } from any to any port { 53, 853 }\n"
	if got != want {
		t.Fatalf("pf 规则：%q", got)
	}
}
