package trust

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"boxctl/internal/killswitch"
)

// fakeStrictProber 按探测入口的端口返回结果。
type fakeStrictProber struct {
	results map[string]Egress
	port    string
}

func (f fakeStrictProber) Probe(context.Context, []string) Egress { return f.results[f.port] }

func newStrictForTest(allowed []string, results map[string]Egress) *StrictChecker {
	c := NewStrictChecker(
		func() []StrictTarget { return []StrictTarget{{"Claude", 19200}, {"ChatGPT", 19201}} },
		func() []string { return allowed },
		func() []string { return []string{"https://example.com/trace"} },
	)
	c.prober = func(p *url.URL) EgressProber { return fakeStrictProber{results, p.Port()} }
	return c
}

func ok(ip string) Egress { return Egress{OK: true, IP: ip, CheckedAt: time.Now()} }

func TestStrictAllGroupsMustMatch(t *testing.T) {
	res := map[string]Egress{"19200": ok("198.51.100.5"), "19201": ok("203.0.113.9")}
	c := newStrictForTest([]string{"198.51.100.0/24"}, res)
	if !c.Check(context.Background()) {
		t.Fatal("首次探测出现不符合时应报告变化")
	}
	s := c.Status()
	if !strings.Contains(s.Reason, "ChatGPT") || !strings.Contains(s.Reason, "203.0.113.9") {
		t.Fatalf("一组不符合就阻断：%+v", s)
	}
	if !s.Groups[0].Allowed || s.Groups[1].Allowed {
		t.Fatalf("每组的结果：%+v", s.Groups)
	}

	inner := StrictPolicy{Inner: fixedPolicy{killswitch.PathProxy}, Strict: c}
	if v := inner.Verdict(true); v.Path != killswitch.PathHold {
		t.Fatalf("经内核时不符合应阻断：%+v", v)
	}
	direct := StrictPolicy{Inner: fixedPolicy{killswitch.PathDirect}, Strict: c}
	if v := direct.Verdict(false); v.Path != killswitch.PathDirect {
		t.Fatalf("直连时不看严格线路：%+v", v)
	}

	res["19201"] = ok("198.51.100.7")
	if !c.Check(context.Background()) || c.Status().Reason != "" {
		t.Fatalf("全部符合后应恢复：%+v", c.Status())
	}
	if v := inner.Verdict(true); v.Path != killswitch.PathProxy {
		t.Fatalf("全部符合时经内核：%+v", v)
	}
}

func TestStrictProbeFailureTolerance(t *testing.T) {
	res := map[string]Egress{"19200": ok("198.51.100.5"), "19201": ok("198.51.100.6")}
	c := newStrictForTest([]string{"198.51.100.0/24"}, res)
	c.Check(context.Background())
	res["19200"] = Egress{Error: "超时", CheckedAt: time.Now()}
	c.Check(context.Background())
	if r := c.Status().Reason; r != "" {
		t.Fatalf("偶尔一次失败不阻断：%s", r)
	}
	c.Check(context.Background())
	if r := c.Status().Reason; !strings.Contains(r, "Claude") || !strings.Contains(r, "超时") {
		t.Fatalf("连续失败应阻断：%s", r)
	}
}

func TestStrictNotRequiredOnlyShows(t *testing.T) {
	res := map[string]Egress{"19200": ok("203.0.113.1"), "19201": {Error: "超时"}}
	c := newStrictForTest(nil, res)
	c.Check(context.Background())
	s := c.Status()
	if s.Required || s.Reason != "" || len(s.Groups) != 2 || s.Groups[0].Egress.IP != "203.0.113.1" {
		t.Fatalf("没有下发列表时只展示：%+v", s)
	}
}

type fixedPolicy struct{ p killswitch.Path }

func (f fixedPolicy) Verdict(bool) killswitch.Verdict { return killswitch.Verdict{Path: f.p} }

func TestStrictInvalidateHoldsUntilRechecked(t *testing.T) {
	res := map[string]Egress{"19200": ok("198.51.100.5"), "19201": ok("198.51.100.6")}
	c := newStrictForTest([]string{"198.51.100.0/24"}, res)
	changes := 0
	c.onChange = func() { changes++ }
	var alerts []string
	c.SetAlert(func(_, body string) { alerts = append(alerts, body) })
	c.Check(context.Background())
	if c.Status().Reason != "" {
		t.Fatal("全部符合时不阻断")
	}

	// 切换了线路：旧结果作废，重测前按「正在探测」阻断，并立即通知 Kill Switch。
	c.Invalidate()
	s := c.Status()
	if !strings.Contains(s.Reason, "正在探测") || !s.Pending || changes != 1 {
		t.Fatalf("作废后应等待重测：%+v，变化 %d 次", s, changes)
	}
	if v := (StrictPolicy{Inner: fixedPolicy{killswitch.PathProxy}, Strict: c}).Verdict(true); v.Path != killswitch.PathHold || !v.Pending {
		t.Fatalf("重测前应阻断并标记 Pending：%+v", v)
	}
	if !c.Check(context.Background()) || c.Status().Reason != "" {
		t.Fatalf("重测符合后恢复：%+v", c.Status())
	}

	// 从成功变成失败时发一次通知，持续失败不重复。
	res["19201"] = Egress{Error: "超时", CheckedAt: time.Now()}
	c.Check(context.Background())
	c.Check(context.Background())
	if len(alerts) != 1 || !strings.Contains(alerts[0], "ChatGPT") {
		t.Fatalf("通知：%v", alerts)
	}
}

func TestStrictHealthThresholds(t *testing.T) {
	res := map[string]Egress{"19200": ok("198.51.100.5"), "19201": ok("198.51.100.6")}
	c := newStrictForTest([]string{"198.51.100.0/24"}, res)
	c.SetHealth(func() StrictHealth { return StrictHealth{Failures: 3, Recover: 2} })
	ctx := context.Background()
	c.Check(ctx)
	res["19200"] = Egress{Error: "超时", CheckedAt: time.Now()}
	c.Check(ctx)
	c.Check(ctx)
	if r := c.Status().Reason; r != "" {
		t.Fatalf("失败阈值为 3 时两次失败不阻断：%s", r)
	}
	c.Check(ctx)
	if r := c.Status().Reason; !strings.Contains(r, "超时") {
		t.Fatalf("第三次失败应阻断：%s", r)
	}
	// 来回翻转期间保持阻断：一次成功不够。
	res["19200"] = ok("198.51.100.5")
	c.Check(ctx)
	if r := c.Status().Reason; !strings.Contains(r, "连续 2 次") {
		t.Fatalf("恢复阈值为 2 时一次成功仍阻断：%s", r)
	}
	res["19200"] = Egress{Error: "超时", CheckedAt: time.Now()}
	c.Check(ctx)
	res["19200"] = ok("198.51.100.5")
	c.Check(ctx)
	if c.Status().Reason == "" {
		t.Fatal("中间又失败一次，连续成功次数应重新计算")
	}
	c.Check(ctx)
	if r := c.Status().Reason; r != "" {
		t.Fatalf("连续两次成功后恢复：%s", r)
	}
}

func TestStrictHealthNormalize(t *testing.T) {
	if h := (StrictHealth{}).Normalize(); h != DefaultStrictHealth() {
		t.Fatalf("零值取默认：%+v", h)
	}
	h := StrictHealth{Interval: 1, BlockedInterval: 1000, Failures: 99, Recover: -1}.Normalize()
	if h.Interval != 10 || h.BlockedInterval != 120 || h.Failures != 10 || h.Recover != 1 {
		t.Fatalf("范围限制：%+v", h)
	}
	if DefaultStrictHealth().maxAge() != 75*time.Second {
		t.Fatal("默认有效期应保持 75 秒")
	}
}

// 网络变化后先阻断再放通：作废后哪怕探测失败次数还没到阈值，也不能因为「偶尔一次失败不阻断」而放行，
// 只有探测成功、出口符合才解除。
func TestStrictInvalidateStaysBlockedOnFailure(t *testing.T) {
	res := map[string]Egress{"19200": ok("198.51.100.5"), "19201": ok("198.51.100.6")}
	c := newStrictForTest([]string{"198.51.100.0/24"}, res)
	ctx := context.Background()
	c.Check(ctx)
	if c.Status().Reason != "" {
		t.Fatal("全部符合时不阻断")
	}
	c.Invalidate()
	res["19200"] = Egress{Error: "网络不可达", CheckedAt: time.Now()}
	c.Check(ctx)
	if r := c.Status().Reason; r == "" {
		t.Fatal("作废后第一次探测失败也应继续阻断")
	}
	res["19200"] = ok("203.0.113.9") // 网络恢复，但出口变了、不在列表里
	c.Check(ctx)
	if r := c.Status().Reason; !strings.Contains(r, "不在管理服务允许的列表里") {
		t.Fatalf("出口不符应继续阻断：%q", r)
	}
	res["19200"] = ok("198.51.100.7")
	c.Check(ctx)
	if r := c.Status().Reason; r != "" {
		t.Fatalf("出口符合后才放通：%q", r)
	}
}
