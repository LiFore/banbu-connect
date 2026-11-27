package dist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const htmlLoginMail = "From: Anthropic <no-reply-abc@mail.anthropic.com>\r\n" +
	"Subject: =?UTF-8?B?55m75b2VIENsYXVkZQ==?=\r\n" +
	"Date: Fri, 13 Nov 2026 08:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=b1\r\n\r\n" +
	"--b1\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"<html><head><style>p{color:red}</style></head><body><p>Hi,</p><p><a href=3D\"https://claude.ai/magic-link#abc=\r\n123\">Sign in</a></p>" +
	"<p>Code: <b>482913</b></p><script>alert(1)</script></body></html>\r\n" +
	"--b1--\r\n"

const plainNotice = "From: Anthropic <billing@anthropic.com>\r\nSubject: Receipt\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n\r\nThanks for your payment.\r\n"

const otherSender = "From: Someone <x@notanthropic.com>\r\nSubject: Private\r\n" +
	"Content-Type: text/plain\r\n\r\nsecret\r\n"

func TestClaudeMails(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	at := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	s.ListMail = func(_ context.Context, _ int64, sender string) ([]RawMail, error) {
		if sender != "anthropic.com" {
			return nil, nil
		}
		return []RawMail{
			{ID: "1", Raw: []byte(plainNotice), ReceivedAt: at.Add(-time.Hour)},
			{ID: "2", Raw: []byte(htmlLoginMail), ReceivedAt: at},
			{ID: "3", Raw: []byte(otherSender), ReceivedAt: at},
		}, nil
	}
	d, _, _ := s.CreateDevice(ctx)
	a, _ := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com", Devices: []int64{d.ID}})
	if _, err := s.ClaudeMails(ctx, d.ID, a.ID); err == nil {
		t.Fatal("没授权 Gmail 时应报错")
	}
	authorize(t, s, a.ID)

	list, err := s.ClaudeMails(ctx, d.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "2" || list[1].ID != "1" {
		t.Fatalf("只返回 Anthropic 的邮件，新的在前：%+v", list)
	}
	login := list[0]
	if login.Subject != "登录 Claude" || len(login.Links) != 1 || login.Links[0] != "https://claude.ai/magic-link#abc123" {
		t.Fatalf("登录邮件：%+v", login)
	}
	if !strings.Contains(login.Text, "Sign in（https://claude.ai/magic-link#abc123）") {
		t.Fatalf("正文里保留链接：%q", login.Text)
	}
	if !strings.Contains(login.Text, "482913") || strings.Contains(login.Text, "alert") || strings.Contains(login.Text, "color") || strings.Contains(login.Text, "<") {
		t.Fatalf("HTML 应转成纯文本：%q", login.Text)
	}
	if len(list[1].Links) != 0 || list[1].Text != "Thanks for your payment." {
		t.Fatalf("普通邮件：%+v", list[1])
	}

	// 不允许使用这个账号的客户端看不到。
	other, _, _ := s.CreateDevice(ctx)
	if _, err := s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: "user@gmail.com", Devices: []int64{d.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaudeMails(ctx, other.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未授权的客户端：%v", err)
	}
}

func TestOpenLinkRequest(t *testing.T) {
	c, _ := NewClient(t.TempDir(), "dev", nil)
	if err := c.RequestOpenLink("https://evil.example/claude.ai/magic-link#x"); err == nil {
		t.Fatal("只能打开 Claude 的登录链接")
	}
	if err := c.RequestOpenLink(testLink + " https://evil.example"); err == nil {
		t.Fatal("要整串匹配")
	}
	if err := c.RequestOpenLink(testLink); err != nil {
		t.Fatal(err)
	}
	p := c.PendingOpen()
	if p == nil {
		t.Fatal("应有待打开的链接")
	}
	if link, err := c.ClaimOpenLink(p.ID); err != nil || link != testLink {
		t.Fatalf("认领：%q %v", link, err)
	}
	if _, err := c.ClaimOpenLink(p.ID); err == nil || c.PendingOpen() != nil {
		t.Fatal("只能认领一次")
	}
}

const codexMail = "From: OpenAI <noreply@tm.openai.com>\r\nSubject: Your ChatGPT code is 482913\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n\r\nEnter this temporary verification code to continue: 482913\r\nCall 1234567 for help.\r\n"

func TestCodexMails(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	var asked string
	s.ListMail = func(_ context.Context, _ int64, sender string) ([]RawMail, error) {
		asked = sender
		return []RawMail{
			{ID: "1", Raw: []byte(codexMail), ReceivedAt: time.Now()},
			{ID: "2", Raw: []byte(plainNotice), ReceivedAt: time.Now()}, // Anthropic 的不给 Codex 账号
		}, nil
	}
	d, _, _ := s.CreateDevice(ctx)
	// 同一个 Gmail 可以同时登记 Claude 和 Codex 账号。
	_, _ = s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com"})
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Email: "user@gmail.com", Devices: []int64{d.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: "gpt", Email: "x@gmail.com"}); err == nil {
		t.Fatal("unknown kind should fail")
	}
	authorize(t, s, a.ID)
	list, err := s.ClaudeMails(ctx, d.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if asked != "openai.com" || len(list) != 1 || len(list[0].Codes) != 1 || list[0].Codes[0] != "482913" || len(list[0].Links) != 0 {
		t.Fatalf("codex mails (%s) = %+v", asked, list)
	}
	// 旧版客户端只看到 Claude 账号；带 kinds 时才有 Codex。
	if l, _ := s.ClientClaudeAccounts(ctx, d.ID); len(l) != 0 {
		t.Fatalf("default kinds = %+v", l)
	}
	if l, _ := s.ClientClaudeAccounts(ctx, d.ID, KindClaude, KindCodex); len(l) != 1 || l[0].Kind != KindCodex {
		t.Fatalf("with kinds = %+v", l)
	}
	if _, err := s.StartClaudeLogin(ctx, d.ID, a.ID); err == nil {
		t.Fatal("one-click login is for Claude only")
	}
}
