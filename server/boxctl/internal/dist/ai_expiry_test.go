package dist

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNormalizeExpiry(t *testing.T) {
	for in, want := range map[string]string{"": "", " 2027-01-31 ": "2027-01-31", "2027-01-31T08:00:00Z": "2027-01-31"} {
		if got, err := normalizeExpiry(in); err != nil || got != want {
			t.Fatalf("normalizeExpiry(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"2027/01/31", "2027-13-01", "明天"} {
		if _, err := normalizeExpiry(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 11, 23, 23, 30, 0, 0, loc)
	for date, want := range map[string]int{"2026-11-23": 0, "2026-11-24": 1, "2026-11-20": -3, "2026-12-03": 10} {
		if got, ok := expiryDaysLeft(date, now, loc); !ok || got != want {
			t.Fatalf("expiryDaysLeft(%s) = %d, want %d", date, got, want)
		}
	}
}

func TestCreateAccountWithCredential(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d := newDevice(t, s)

	// Codex：粘贴 auth.json，邮箱从 id_token 读出，凭据和账号一起保存。
	id := codexJWT(`{"email":"C@Example.com"}`)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Devices: []int64{d.ID}, SubExpires: "2027-01-31",
		AccessToken: `{"tokens":{"refresh_token":"rt-c","id_token":"` + id + `"}}`})
	if err != nil || a.Email != "c@example.com" || !a.HasCredential || a.CredVersion != 1 || a.SubExpires != "2027-01-31" {
		t.Fatalf("a = %+v, err = %v", a, err)
	}
	// 填的邮箱和凭据不一致：不建账号。
	if _, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Email: "x@example.com",
		AccessToken: `{"tokens":{"refresh_token":"r","id_token":"` + id + `"}}`}); err == nil {
		t.Fatal("wrong email should fail")
	}
	// Claude 的凭据里没有邮箱：必须填。
	if _, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindClaude, RefreshToken: "sk-ant-ort01-x"}); err == nil {
		t.Fatal("claude without email should fail")
	}
	b, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindClaude, Email: "b@example.com", RefreshToken: "sk-ant-ort01-x"})
	if err != nil || !b.HasCredential || b.CredSource != "管理员填写" {
		t.Fatalf("b = %+v, err = %v", b, err)
	}
	if n := len(must(s.ClaudeAccounts(ctx))); n != 2 {
		t.Fatalf("应有 2 个账号，实际 %d", n)
	}

	// 类型不能改；不传类型时沿用原来的。
	if _, err := s.UpdateClaudeAccount(ctx, b.ID, ClaudeAccountInput{Kind: KindCodex, Email: b.Email}); err == nil {
		t.Fatal("kind change should fail")
	}
	b, err = s.UpdateClaudeAccount(ctx, b.ID, ClaudeAccountInput{Email: b.Email, SubExpires: "2027-02-01"})
	if err != nil || b.Kind != KindClaude || b.SubExpires != "2027-02-01" || !b.HasCredential {
		t.Fatalf("b = %+v, err = %v", b, err)
	}

	// Gemini：refresh token 直接建，邮箱必填（只有 refresh token 读不出邮箱）。
	if _, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{RefreshToken: "1//0x"}); err == nil {
		t.Fatal("gemini without email should fail")
	}
	g, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "g@gmail.com", RefreshToken: "1//0x", SubExpires: "2027-03-01"})
	if err != nil || !g.HasCredential || g.CredVersion != 1 || g.SubExpires != "2027-03-01" {
		t.Fatalf("g = %+v, err = %v", g, err)
	}
	if raw, _, err := s.geminiCredential(ctx, g.ID); err != nil || !strings.Contains(string(raw), "1//0x") {
		t.Fatalf("gemini credential = %s, err = %v", raw, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestAIExpiryAlerts(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	sent := barkSink(t, s)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	day := func(n int) string { return time.Now().In(loc).AddDate(0, 0, n).Format(expiryLayout) }
	a := must(s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "a@example.com", SubExpires: day(2)}))
	must(s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Email: "far@example.com", SubExpires: day(30)}))
	must(s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "g@gmail.com", SubExpires: day(-1)}))

	if _, err := s.CheckAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sent(), "|")
	for _, want := range []string{"Claude 订阅 2 天后到期：a@example.com", "Gemini 订阅已到期：g@gmail.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少提醒 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "far@example.com") {
		t.Fatal("离到期还远的不应提醒")
	}
	// 同一件事只提醒一次；续费改了到期日后按新日期重新判断。
	_, _ = s.CheckAlerts(ctx)
	if got := sent(); len(got) != 0 {
		t.Fatalf("不应重复提醒：%v", got)
	}
	must(s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: a.Email, SubExpires: day(1)}))
	_, _ = s.CheckAlerts(ctx)
	if got := sent(); len(got) != 1 || !strings.Contains(got[0], "1 天后到期") {
		t.Fatalf("改了到期日应重新提醒：%v", got)
	}
}
