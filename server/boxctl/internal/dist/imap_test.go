package dist

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeIMAP 是只认几条命令的 IMAP 服务器（明文，只用于测试）。mails 是 INBOX 里的邮件。
type fakeIMAP struct {
	user, pass string
	mails      []string
	received   time.Time
	logins     int
}

func (f *fakeIMAP) serve(t *testing.T) MailServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return MailServer{Host: "127.0.0.1", Port: port, Security: "none"}
}

func (f *fakeIMAP) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	fmt.Fprint(c, "* OK fake ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, cmd, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch {
		case strings.HasPrefix(cmd, "LOGIN "):
			if cmd != "LOGIN "+imapQuote(f.user)+" "+imapQuote(f.pass) {
				fmt.Fprintf(c, "%s NO [AUTHENTICATIONFAILED] bad\r\n", tag)
				continue
			}
			f.logins++
			fmt.Fprintf(c, "%s OK logged in\r\n", tag)
		case cmd == `EXAMINE "INBOX"` || cmd == "EXAMINE INBOX":
			fmt.Fprintf(c, "* %d EXISTS\r\n%s OK [READ-ONLY] done\r\n", len(f.mails), tag)
		case strings.HasPrefix(cmd, "EXAMINE "):
			fmt.Fprintf(c, "%s NO no such mailbox\r\n", tag)
		case strings.HasPrefix(cmd, "UID SEARCH "):
			ids := []string{}
			for i := range f.mails {
				ids = append(ids, strconv.Itoa(i+1))
			}
			fmt.Fprintf(c, "* SEARCH %s\r\n%s OK done\r\n", strings.Join(ids, " "), tag)
		case strings.HasPrefix(cmd, "UID FETCH "):
			for i, m := range f.mails {
				fmt.Fprintf(c, "* %d FETCH (UID %d INTERNALDATE \"%s\" BODY[] {%d}\r\n%s)\r\n",
					i+1, i+1, f.received.Format("02-Jan-2006 15:04:05 -0700"), len(m), m)
			}
			fmt.Fprintf(c, "%s OK done\r\n", tag)
		case cmd == "LOGOUT":
			fmt.Fprintf(c, "* BYE\r\n%s OK bye\r\n", tag)
			return
		default:
			fmt.Fprintf(c, "%s BAD unknown\r\n", tag)
		}
	}
}

func TestImapMailbox(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	f := &fakeIMAP{user: "ai1@mail.example.com", pass: "p@ssw0rd", mails: []string{testMail}, received: time.Now().Add(-time.Minute)}
	srv := f.serve(t)
	if _, err := s.SetMailServer(ctx, srv); err != nil {
		t.Fatal(err)
	}

	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "ai1@mail.example.com", Devices: []int64{2}, MailPassword: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.HasMailbox || a.MailSource != "imap" {
		t.Fatalf("account = %+v", a)
	}
	// 自建邮箱的账号也下发给客户端。
	if l, _ := s.ClientClaudeAccounts(ctx, 2); len(l) != 1 {
		t.Fatalf("client accounts = %v", l)
	}
	if err := s.TestClaudeAccount(ctx, a.ID); err == nil || !strings.Contains(err.Error(), "登录失败") {
		t.Fatalf("wrong password: %v", err)
	}
	// 修改时密码留空不变，只改套餐；再填对密码。
	in := ClaudeAccountInput{Email: a.Email, Devices: []int64{2}, Plan: "Max 5x"}
	if a, err = s.UpdateClaudeAccount(ctx, a.ID, in); err != nil || !a.HasMailbox || a.Plan != "Max 5x" {
		t.Fatalf("update = %+v, %v", a, err)
	}
	in.MailPassword = f.pass
	if _, err = s.UpdateClaudeAccount(ctx, a.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := s.TestClaudeAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}

	link, err := s.fetchLoginLink(ctx, a.ID, a.Email, time.Now().Add(-5*time.Minute))
	if err != nil || link != testLink {
		t.Fatalf("link = %q, %v", link, err)
	}
	// 早于 since 到达的邮件不算。
	if link, _ := s.fetchLoginLink(ctx, a.ID, a.Email, time.Now()); link != "" {
		t.Fatalf("old mail used: %q", link)
	}
	mails, err := s.ClaudeMails(ctx, 2, a.ID)
	if err != nil || len(mails) != 1 || len(mails[0].Links) != 1 {
		t.Fatalf("mails = %+v, %v", mails, err)
	}

	// 换邮箱（没单独填登录名）时密码清除。
	if a, err = s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: "ai2@mail.example.com", Devices: []int64{2}}); err != nil || a.HasMailbox {
		t.Fatalf("email change kept mailbox: %+v, %v", a, err)
	}
	if _, err := s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: a.Email, MailPassword: "a\"b"}); err == nil {
		t.Fatal("quote in password should be rejected")
	}
}

func TestNormalizeProfile(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	if _, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "a@x.com", AccountType: "team"}); err == nil {
		t.Fatal("team without role should fail")
	}
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "a@x.com", Plan: " Team ", AccountType: "team", TeamRole: "owner", TeamName: "Acme", TeamDomain: "@Acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan != "Team" || a.AccountType != "team" || a.TeamRole != "owner" || a.TeamName != "Acme" || a.TeamDomain != "acme.com" {
		t.Fatalf("account = %+v", a)
	}
	// Codex 不区分 Team：标记被忽略。
	c, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Kind: KindCodex, Email: "a@x.com", Plan: "Plus", AccountType: "team", TeamRole: "owner", TeamName: "Acme"})
	if err != nil || c.AccountType != "personal" || c.TeamName != "" || c.Plan != "Plus" {
		t.Fatalf("codex = %+v, %v", c, err)
	}
	g, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "g@x.com", Plan: "AI Pro"})
	if err != nil || g.Plan != "AI Pro" {
		t.Fatalf("gemini = %+v, %v", g, err)
	}
}
