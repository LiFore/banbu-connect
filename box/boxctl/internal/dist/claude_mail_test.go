package dist

import (
	"testing"
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

const testLink = "https://claude.ai/magic-link#3a046a5f643c7cbffc44cd3f0123456789abcdef"
