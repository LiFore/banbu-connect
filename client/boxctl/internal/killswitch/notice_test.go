package killswitch

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type switchPolicy struct{ v Verdict }

func (p *switchPolicy) Verdict(bool) Verdict { return p.v }

func noticeKinds(e *Engine) []string {
	var out []string
	for _, n := range e.Status().Notices {
		out = append(out, n.Kind)
	}
	return out
}

func TestNotices(t *testing.T) {
	ctx := context.Background()
	e, _, p := newTestEngine(t)
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	e.notices.now = func() time.Time { return clock }
	pol := &switchPolicy{v: Verdict{Path: PathProxy}}
	e.SetPolicy(pol, "127.0.0.1:10053")
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	expect := func(want ...string) {
		t.Helper()
		if got := noticeKinds(e); !slices.Equal(got, want) {
			t.Fatalf("通知 = %v，应为 %v", got, want)
		}
	}
	set := func(v Verdict, advance time.Duration) {
		pol.v = v
		clock = clock.Add(advance)
		e.Reconcile(ctx)
		e.ProbeOnce(ctx)
	}
	pending := Verdict{Path: PathHold, Reason: "正在探测", Pending: true}

	// 正在确认出口的短暂阻断不通知，恢复也不通知。
	set(pending, 0)
	set(pending, 5*time.Second)
	set(Verdict{Path: PathProxy}, time.Second)
	expect()

	// 一直确认不下来：超过宽限期才通知，恢复时通知。
	set(pending, time.Second)
	set(pending, noticeGrace)
	expect(NoticeBlocked)
	set(Verdict{Path: PathDirect, Reason: "Wi-Fi「Home」"}, time.Second)
	expect(NoticeBlocked, NoticeRestored)

	// 确定的阻断立即通知。
	set(Verdict{Path: PathHold, Reason: "出口不在允许列表里"}, time.Second)
	expect(NoticeBlocked, NoticeRestored, NoticeBlocked)
	set(Verdict{Path: PathProxy}, time.Second)

	// 内核不可用：同样有宽限期（内核重启只要几秒）。
	p.h = Health{DNSOK: false, DNSDetail: "Mihomo DNS 无响应"}
	set(Verdict{Path: PathProxy}, time.Second)
	expect(NoticeBlocked, NoticeRestored, NoticeBlocked, NoticeRestored)
	set(Verdict{Path: PathProxy}, noticeGrace)
	expect(NoticeBlocked, NoticeRestored, NoticeBlocked, NoticeRestored, NoticeBlocked)

	// 阻断中关闭保护：不发「恢复」。
	if err := e.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	e.ProbeOnce(ctx)
	expect(NoticeBlocked, NoticeRestored, NoticeBlocked, NoticeRestored, NoticeBlocked)

	// 保护关闭时探测失败不通知。
	e.Notify(NoticeProbeFailed, "出口探测失败", "超时")
	if n := len(e.Status().Notices); n != 5 {
		t.Fatalf("保护关闭时不应通知，实际 %d 条", n)
	}

	ids := e.Status().Notices
	for i := 1; i < len(ids); i++ {
		if ids[i].ID <= ids[i-1].ID {
			t.Fatal("通知 ID 应单调递增")
		}
	}
}

func TestNoticeRulesFailed(t *testing.T) {
	ctx := context.Background()
	e, b, _ := newTestEngine(t)
	pol := &switchPolicy{v: Verdict{Path: PathDirect, Reason: "Wi-Fi「Home」"}}
	e.SetPolicy(pol, "127.0.0.1:10053")
	if err := e.Enable(ctx, false); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.FailApply = errors.New("pfctl 失败")
	b.mu.Unlock()

	// 离开受信网络后规则写不上：发一次通知，重试仍失败不重复发。
	pol.v = Verdict{Path: PathProxy}
	e.Reconcile(ctx)
	e.Reconcile(ctx)
	if got := noticeKinds(e); !slices.Equal(got, []string{NoticeRulesFailed}) {
		t.Fatalf("通知 = %v", got)
	}

	// 恢复后再失败，重新通知。
	b.mu.Lock()
	b.FailApply = nil
	b.mu.Unlock()
	e.Reconcile(ctx)
	if e.Status().State == StateError {
		t.Fatal("规则写上后不应是错误状态")
	}
	b.mu.Lock()
	b.FailApply = errors.New("pfctl 失败")
	b.Current = nil
	b.mu.Unlock()
	e.Reconcile(ctx)
	if got := noticeKinds(e); !slices.Equal(got, []string{NoticeRulesFailed, NoticeRulesFailed}) {
		t.Fatalf("通知 = %v", got)
	}
}
