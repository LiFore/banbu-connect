package dist

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestSummarizeUsage(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	reset := "2026-11-23T15:00:00Z"
	rs := []quotaRow{
		{bucket: "pro", label: "Pro", remaining: 0.8, reset: reset, at: now.Add(-60 * time.Minute)},
		{bucket: "flash", label: "Flash", remaining: 1, reset: "", at: now.Add(-60 * time.Minute)},
		{bucket: "pro", label: "Pro", remaining: 0.5, reset: reset, at: now.Add(-30 * time.Minute)},
		{bucket: "pro", label: "Pro", remaining: 0.3, reset: reset, at: now.Add(-10 * time.Minute)},
	}
	u := summarizeUsage(1, rs, now)
	// 50 分钟用了 0.5：每小时 0.6，剩 0.3 约 30 分钟后耗尽，早于 15:00 恢复。
	if !u.Known || u.Min != 0.3 || u.MinLabel != "Pro" || u.BurnPerHour < 0.59 || u.BurnPerHour > 0.61 {
		t.Fatalf("usage = %+v", u)
	}
	if ex, _ := time.Parse(time.RFC3339, u.ExhaustAt); ex.Sub(now) < 19*time.Minute || ex.Sub(now) > 21*time.Minute {
		t.Fatalf("exhaust at %s", u.ExhaustAt)
	}
	if !u.soon(now) || u.Exhausted() {
		t.Fatal("should be soon but not exhausted")
	}
	// 恢复前用不完：不给耗尽时间。
	rs[0].reset, rs[2].reset, rs[3].reset = "2026-11-23T12:10:00Z", "2026-11-23T12:10:00Z", "2026-11-23T12:10:00Z"
	if u := summarizeUsage(1, rs, now); u.ExhaustAt != "" {
		t.Fatalf("exhaust after reset = %s", u.ExhaustAt)
	}
	// 一小时以前的采样不算。
	if u := summarizeUsage(1, rs[:2], now.Add(2*time.Hour)); u.Known {
		t.Fatalf("stale = %+v", u)
	}
}

func TestOrderAndSuggest(t *testing.T) {
	now := time.Now()
	usage := map[int64]GeminiUsage{
		1: {AccountID: 1, Known: true, Min: 0.02, ResetAt: "2026-11-23T13:00:00Z"},                          // 耗尽
		2: {AccountID: 2, Known: true, Min: 0.6, ResetAt: "2026-11-23T18:00:00Z"},                           // 可用，晚恢复
		3: {AccountID: 3, Known: true, Min: 0.4, ResetAt: "2026-11-23T14:00:00Z"},                           // 可用，早恢复：先用
		5: {AccountID: 5, Known: true, Min: 0.01, ResetAt: "2026-11-23T12:30:00Z"},                          // 耗尽，早恢复
		6: {AccountID: 6, Known: true, Min: 0.9, ResetAt: "2026-11-23T14:00:00Z"},                           // 同时间恢复，剩得多的在前
		7: {AccountID: 7, Known: true, Min: 0.5, ExhaustAt: now.Add(10 * time.Minute).Format(time.RFC3339)}, // 快用完
	}
	got := orderAccounts([]int64{1, 2, 3, 4, 5, 6}, usage)
	if want := []int64{6, 3, 2, 4, 5, 1}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	emails := map[int64]string{1: "a", 2: "b", 3: "c", 6: "f", 7: "g"}
	if s := suggest(1, got, usage, emails, now); s == nil || s.AccountID != 6 {
		t.Fatalf("suggest for exhausted = %+v", s)
	}
	if s := suggest(7, []int64{7, 2}, usage, emails, now); s == nil || s.AccountID != 2 {
		t.Fatalf("suggest for soon = %+v", s)
	}
	if s := suggest(2, got, usage, emails, now); s != nil {
		t.Fatalf("no suggestion expected, got %+v", s)
	}
}

func TestGeminiAutoRotateAndSwitch(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	b, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "b@gmail.com", Devices: []int64{d.ID}})
	for _, x := range []GeminiAccount{a, b} {
		_, _ = s.UploadGeminiCredential(ctx, d, x.ID, CredentialUpload{Replace: true, Credential: testCredential(x.Email, "r"+x.Email)})
	}
	// a 快用完、b 很充足：建议顺序 b 在前。
	_ = s.RecordGeminiQuota(ctx, d.ID, []GeminiQuotaSample{
		{AccountID: a.ID, Buckets: []GeminiQuotaBucket{{Key: "pro", Label: "Pro", Remaining: 0.03, ResetAt: "2099-01-01T00:00:00Z"}}},
		{AccountID: b.ID, Buckets: []GeminiQuotaBucket{{Key: "pro", Label: "Pro", Remaining: 0.9, ResetAt: "2099-01-01T00:00:00Z"}}},
	})
	_ = c.syncGemini(ctx, 0)
	if st := c.GeminiStatus(); !slices.Equal(st.Order, []int64{b.ID, a.ID}) || st.ActiveID != b.ID {
		t.Fatalf("order = %v active = %d", st.Order, st.ActiveID)
	}

	// 用户手动切到 a；采到 a 已耗尽 → 自动切回 b。
	_ = c.SetGeminiActive(ctx, a.ID)
	c.mu.Lock()
	c.gem.quota = map[int64]GeminiQuota{
		a.ID: {Buckets: []GeminiQuotaBucket{{Key: "pro", Remaining: 0.03}}},
		b.ID: {Buckets: []GeminiQuotaBucket{{Key: "pro", Remaining: 0.9}}},
	}
	c.mu.Unlock()
	if c.GeminiBrief().Defer != nil {
		t.Fatal("user switch should not be deferred")
	}
	c.autoRotateGemini(ctx)
	if st := c.GeminiStatus(); st.ActiveID != b.ID || st.Rotated == "" || st.Pending == nil || st.Pending.AccountID != b.ID {
		t.Fatalf("after rotate = %+v", st)
	}
	// 自动轮换给 Antigravity 宽限期；「立即切换」结束宽限期。
	if br := c.GeminiBrief(); br.Defer == nil || br.Defer.Until <= time.Now().Unix() || br.Defer.Reason == "" {
		t.Fatalf("rotate should be deferred: %+v", br.Defer)
	}
	c.ApplyGeminiNow()
	if c.GeminiBrief().Defer != nil || c.GeminiStatus().Pending != nil {
		t.Fatal("apply-now should clear the deferral")
	}
	// b 也不够时不切。
	c.mu.Lock()
	c.gem.quota[b.ID] = GeminiQuota{Buckets: []GeminiQuotaBucket{{Key: "pro", Remaining: 0.01}}}
	c.mu.Unlock()
	_ = c.SetGeminiActive(ctx, a.ID)
	c.autoRotateGemini(ctx)
	if c.GeminiStatus().ActiveID != a.ID {
		t.Fatal("should stay when nothing is better")
	}

	// 管理页面：a 已耗尽，建议换 b；下发切换后客户端执行，建议消失。
	o, err := s.GeminiOverview(ctx)
	if err != nil || len(o.Devices) != 1 || len(o.Usage) != 2 {
		t.Fatalf("overview = %+v, %v", o, err)
	}
	v := o.Devices[0]
	if v.ActiveID != a.ID || v.Suggestion == nil || v.Suggestion.AccountID != b.ID {
		t.Fatalf("device view = %+v", v)
	}
	if _, err := s.RequestGeminiSwitch(ctx, d.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if o, _ = s.GeminiOverview(ctx); o.Devices[0].PendingSwitch != b.ID || o.Devices[0].Suggestion != nil {
		t.Fatalf("pending = %+v", o.Devices[0])
	}
	_ = c.syncGemini(ctx, 0)
	if o, _ = s.GeminiOverview(ctx); o.Devices[0].ActiveID != b.ID || o.Devices[0].PendingSwitch != 0 {
		t.Fatalf("after switch = %+v", o.Devices[0])
	}
	// 管理员下发的切换同样延后。
	if br := c.GeminiBrief(); br.Defer == nil || br.Defer.Reason != "管理员切换了本机的 Gemini 账号" {
		t.Fatalf("admin switch should be deferred: %+v", br.Defer)
	}
}

// 新旧客户端混用（或刚升级）：同一账号既有逐模型、又有时间窗口的采样时只看窗口，按客户端的顺序排；
// 消耗速率按 key 找同一项（窗口的名称和 key 不一一对应）。
func TestSummarizeUsageWindows(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	reset := "2026-11-25T15:00:00Z"
	week := "2026-11-30T00:00:00Z"
	at := func(m int) time.Time { return now.Add(time.Duration(-m) * time.Minute) }
	rs := []quotaRow{
		// 旧版客户端：逐模型，Gemini 3 Pro 已经很紧。
		{bucket: "gemini-3-pro-high", label: "Gemini 3 Pro (High)", remaining: 0.02, reset: reset, at: at(50)},
		// 新版客户端：窗口。
		{bucket: "gemini:weekly", label: "Gemini（每周）", remaining: 0.7, reset: week, at: at(60)},
		{bucket: "claude:5h", label: "Claude（5 小时）", remaining: 0.6, reset: reset, at: at(60)},
		{bucket: "gemini:weekly", label: "Gemini（每周）", remaining: 0.65, reset: week, at: at(0)},
		{bucket: "claude:5h", label: "Claude（5 小时）", remaining: 0.4, reset: reset, at: at(0)},
	}
	u := summarizeUsage(1, rs, now)
	if len(u.Buckets) != 2 || u.Buckets[0].Key != "claude:5h" || u.Buckets[1].Key != "gemini:weekly" {
		t.Fatalf("buckets = %+v", u.Buckets)
	}
	if u.Min != 0.4 || u.MinLabel != "Claude（5 小时）" || u.ResetAt != reset {
		t.Fatalf("min = %v %q %q", u.Min, u.MinLabel, u.ResetAt)
	}
	// 一小时用了 0.2：每小时 20%，2 小时后耗尽，早于 3 小时后的重置。
	if u.BurnPerHour < 0.19 || u.BurnPerHour > 0.21 || u.ExhaustAt != "2026-11-25T14:00:00Z" {
		t.Fatalf("burn = %v exhaust = %q", u.BurnPerHour, u.ExhaustAt)
	}

	// 只有逐模型（全是旧版客户端）：照旧。
	u = summarizeUsage(1, rs[:1], now)
	if len(u.Buckets) != 1 || u.Min != 0.02 || !u.Exhausted() {
		t.Fatalf("models only = %+v", u)
	}
}
