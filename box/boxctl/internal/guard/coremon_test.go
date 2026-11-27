package guard

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestCoreMonitorBlocksImmediatelyAndRecoversSlowly(t *testing.T) {
	sys := &FakeSystem{}
	e := NewEngine(sys, t.TempDir(), "br-lan", true, slog.New(slog.DiscardHandler))
	var fail error
	e.coreProbe = func(context.Context, Policy) (string, error) { return "v1.19.33", fail }
	ctx := context.Background()
	e.Start(ctx)
	p, _ := policy().Normalize()
	p.CoreExpected, p.CoreController = true, "127.0.0.1:19290"
	if err := e.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	base := e.Status().PolicyVersion

	e.coreTick(ctx)
	if st := e.Status(); st.Core.Blocking || st.Hold || !st.Core.Healthy {
		t.Fatalf("内核正常时不阻断：%+v", st.Core)
	}

	// 第一次失败就阻断：core_down 链丢弃局域网去外网的流量，hold 也打开。
	fail = errors.New("内核离线：控制接口没有响应")
	e.coreTick(ctx)
	st := e.Status()
	script, _, _ := sys.Snapshot()
	if !st.Core.Blocking || !st.Hold || st.State != StateBlocked || !strings.Contains(st.Reason, "内核离线") {
		t.Fatalf("第一次失败应阻断：%+v", st)
	}
	if !strings.Contains(script, `add rule inet proxy_guard core_down iifname "br-lan" counter drop comment "core down"`) ||
		!strings.Contains(script, `core_down iifname "br-lan" fib daddr type local accept`) {
		t.Fatalf("应整体阻断局域网、放行访问路由盒：\n%s", script)
	}
	if st.PolicyVersion != base {
		t.Fatal("状态里的策略版本应是面板下发的那份，不随 guard 自己的阻断变化")
	}
	// 自检按实际策略比对，不会把阻断规则当成「缺失」反复重装。
	_, _, applies := sys.Snapshot()
	e.Check(ctx)
	if _, _, n := sys.Snapshot(); n != applies {
		t.Fatal("阻断时自检不应重装")
	}

	// 恢复：一次成功不解除，连续两次才解除。
	fail = nil
	e.coreTick(ctx)
	if !e.Status().Core.Blocking {
		t.Fatal("一次成功不应解除")
	}
	e.coreTick(ctx)
	st = e.Status()
	script, _, _ = sys.Snapshot()
	if st.Core.Blocking || st.Hold || strings.Contains(script, "add rule inet proxy_guard core_down") {
		t.Fatalf("连续成功后应解除：%+v", st.Core)
	}
	if st.Core.Blocks != 1 || st.Core.FailTotal != 1 || st.Core.Checks != 4 {
		t.Fatalf("计数：%+v", st.Core)
	}
}

func TestCoreMonitorRespectsUserStopAndNoProtection(t *testing.T) {
	sys := &FakeSystem{}
	e := NewEngine(sys, t.TempDir(), "br-lan", true, slog.New(slog.DiscardHandler))
	e.coreProbe = func(context.Context, Policy) (string, error) { return "", errors.New("内核离线") }
	ctx := context.Background()
	e.Start(ctx)

	p, _ := policy().Normalize() // 用户停掉了内核：CoreExpected 为 false
	_ = e.Apply(ctx, p)
	e.coreTick(ctx)
	if st := e.Status(); st.Core.Blocking || st.Hold {
		t.Fatalf("用户停掉内核时不阻断：%+v", st.Core)
	}
	// 没有受保护的服务（Kill Switch 关闭）：内核离线也不阻断局域网。
	q, _ := Policy{CoreExpected: true}.Normalize()
	_ = e.Apply(ctx, q)
	e.coreTick(ctx)
	if st := e.Status(); st.Core.Blocking {
		t.Fatal("没有受保护服务时不阻断")
	}
}

func TestPanelCannotSetCoreDown(t *testing.T) {
	p, err := Policy{CoreDown: true, CoreController: "127.0.0.1:19290"}.Normalize()
	if err != nil || p.CoreDown {
		t.Fatalf("CoreDown 只能由 guard 设置：%+v %v", p, err)
	}
	if _, err := (Policy{CoreController: "10.0.0.1:19290"}).Normalize(); err == nil {
		t.Fatal("内核地址必须是本机")
	}
}

func TestParseCounters(t *testing.T) {
	out := `{"nftables":[{"rule":{"chain":"forward","expr":[{"match":{}},{"counter":{"packets":3,"bytes":100}},{"drop":null}]}},
		{"rule":{"chain":"forward","expr":[{"counter":{"packets":2,"bytes":10}}]}},
		{"rule":{"chain":"dev_0","expr":[{"counter":{"packets":5,"bytes":10}}]}},
		{"rule":{"chain":"output","expr":[{"accept":null}]}}]}`
	m := parseCounters([]byte(out))
	if m["forward"] != 5 || m["device_block"] != 5 || m["output"] != 0 {
		t.Fatalf("%v", m)
	}
}
