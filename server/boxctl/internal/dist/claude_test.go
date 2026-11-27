package dist

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testLink = "https://claude.ai/magic-link#3a046a5f643c7cbffc44cd3f0123456789abcdef"

// testMail 是一封登录邮件：quoted-printable 把长链接折行、把 = 编码成 =3D。
const testMail = "From: Anthropic <no-reply-P1ZZ0-dmkeUvQP2TouzL1w@mail.anthropic.com>\r\n" +
	"To: user@gmail.com\r\nSubject: Secure link to log in to Claude.ai\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n" +
	"--b1\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Click to log in: https://claude.ai/magic-link#3a046a5f643c7cbffc44cd3f0123=\r\n456789abcdef\r\n\r\n" +
	"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<a href=3D\"x\">x</a>\r\n--b1--\r\n"

// authorize 直接写入一个 Gmail 授权，供不关心授权流程的测试使用。
func authorize(t *testing.T, s *Server, id int64) {
	t.Helper()
	if _, err := s.DB.Exec(`UPDATE claude_accounts SET gmail_token = x'01' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAccountCRUD(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	a, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: " User@Gmail.com ", Devices: []int64{2}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "user@gmail.com" || a.GmailAuthorized {
		t.Fatalf("account = %+v", a)
	}
	if _, err := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com"}); err == nil {
		t.Fatal("duplicate email should fail")
	}
	// 没授权 Gmail 的账号不下发。
	if l, _ := s.ClientClaudeAccounts(ctx, 2); len(l) != 0 {
		t.Fatalf("unauthorized account listed: %v", l)
	}
	authorize(t, s, a.ID)
	if _, err := s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: "user@gmail.com", Note: "主力", Devices: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	// 只下发给分配到的客户端；名单为空时谁都不下发（ADR-060）。
	if _, err := s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: "user@gmail.com"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ClientClaudeAccounts(ctx, 2); len(l) != 0 {
		t.Fatalf("empty list should allow nobody: %v", l)
	}
	_, _ = s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: "user@gmail.com", Note: "主力", Devices: []int64{2}})
	if l, _ := s.ClientClaudeAccounts(ctx, 1); len(l) != 0 {
		t.Fatalf("device 1 sees %v", l)
	}
	if l, _ := s.ClientClaudeAccounts(ctx, 2); len(l) != 1 || l[0].Note != "主力" {
		t.Fatalf("device 2 sees %v", l)
	}
	// 换邮箱清掉授权：授权属于原来的邮箱。
	b, _ := s.UpdateClaudeAccount(ctx, a.ID, ClaudeAccountInput{Email: "other@gmail.com"})
	if b.GmailAuthorized {
		t.Fatal("changing email should clear the Gmail authorization")
	}
}

func TestClaudeLoginFlow(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	var calls atomic.Int32
	s.FetchMail = func(_ context.Context, _ int64, email string, since time.Time) (string, error) {
		if email != "user@gmail.com" || time.Since(since) < time.Minute {
			return "", errors.New("unexpected args")
		}
		if calls.Add(1) < 2 {
			return "", nil // 第一次还没收到
		}
		return testLink, nil
	}
	a, _ := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com", Devices: []int64{7}})
	dev := Device{ID: 7, Name: "MacBook"}
	if _, err := s.StartClaudeLogin(ctx, dev.ID, a.ID); err == nil {
		t.Fatal("没授权 Gmail 时不能发起")
	}
	authorize(t, s, a.ID)
	l, err := s.StartClaudeLogin(ctx, dev.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 别的客户端看不到这次登录。
	if _, err := s.WaitClaudeLink(ctx, 8, l.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other device: %v", err)
	}
	got, err := s.WaitClaudeLink(ctx, dev.ID, l.ID, 15*time.Second)
	if err != nil || got.Link != testLink || got.Status != LoginDelivered {
		t.Fatalf("wait = %+v, %v", got, err)
	}
	// 链接只交付一次。
	if again, _ := s.WaitClaudeLink(ctx, dev.ID, l.ID, 0); again.Link != "" {
		t.Fatal("link delivered twice")
	}
	// 登录的邮箱不一致算失败。
	done, err := s.FinishClaudeLogin(ctx, dev, l.ID, true, "other@gmail.com", "")
	if err != nil || done.Status != LoginFailed || !strings.Contains(done.Error, "other@gmail.com") {
		t.Fatalf("finish = %+v, %v", done, err)
	}
	acc, _ := s.claudeAccount(ctx, a.ID)
	if acc.LastError == "" || acc.LastLoginAt != "" {
		t.Fatalf("account = %+v", acc)
	}

	l2, _ := s.StartClaudeLogin(ctx, dev.ID, a.ID)
	if _, err := s.WaitClaudeLink(ctx, dev.ID, l2.ID, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.FinishClaudeLogin(ctx, dev, l2.ID, true, "USER@gmail.com", ""); done.Status != LoginDone {
		t.Fatalf("finish = %+v", done)
	}
	acc, _ = s.claudeAccount(ctx, a.ID)
	if acc.LastLoginDevice != "MacBook" || acc.LastError != "" {
		t.Fatalf("account = %+v", acc)
	}
}

func TestClaudeLoginAuthErrorStops(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.FetchMail = func(context.Context, int64, string, time.Time) (string, error) {
		return "", &MailAuthError{"Gmail 授权已失效，请重新授权"}
	}
	a, _ := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com", Devices: []int64{1}})
	authorize(t, s, a.ID)
	l, _ := s.StartClaudeLogin(ctx, 1, a.ID)
	got, _ := s.WaitClaudeLink(ctx, 1, l.ID, 5*time.Second)
	if got.Status != LoginFailed || !strings.Contains(got.Error, "重新授权") {
		t.Fatalf("got %+v", got)
	}
}

func TestLinkFromMessage(t *testing.T) {
	if got := linkFromMessage(strings.NewReader(testMail)); got != testLink {
		t.Fatalf("got %q", got)
	}
	fake := strings.Replace(testMail, "mail.anthropic.com", "mail.anthropic.com.evil.test", 1)
	if got := linkFromMessage(strings.NewReader(fake)); got != "" {
		t.Fatalf("spoofed sender accepted: %q", got)
	}
}

// fakeGoogle 模拟 Google 的令牌接口和 Gmail 接口。
type fakeGoogle struct {
	email     string
	revoked   atomic.Bool
	refreshes atomic.Int32
	mailAt    time.Time // 登录邮件到达时间；零值表示还没有
	verifier  string    // 换取授权时收到的 code_verifier
}

func (f *fakeGoogle) serve(t *testing.T) GoogleAPI {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("client_secret") != "sec" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			f.verifier = r.Form.Get("code_verifier")
			_, _ = w.Write([]byte(`{"access_token":"at1","expires_in":3600,"refresh_token":"rt1"}`))
		case "refresh_token":
			f.refreshes.Add(1)
			if f.revoked.Load() || r.Form.Get("refresh_token") != "rt1" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"at2","expires_in":3600}`))
		}
	})
	authed := func(r *http.Request) bool { return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at") }
	mux.HandleFunc("GET /gmail/v1/users/me/profile", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"emailAddress": f.email})
	})
	mux.HandleFunc("GET /gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) || !strings.HasPrefix(r.URL.Query().Get("q"), "from:mail.anthropic.com after:") {
			w.WriteHeader(400)
			return
		}
		if f.mailAt.IsZero() {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"m1"}]}`))
	})
	mux.HandleFunc("GET /gmail/v1/users/me/messages/m1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"raw":          base64.RawURLEncoding.EncodeToString([]byte(testMail)),
			"internalDate": strconv.FormatInt(f.mailAt.UnixMilli(), 10),
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return GoogleAPI{AuthURL: ts.URL + "/auth", TokenURL: ts.URL + "/token", GmailURL: ts.URL + "/gmail/v1", HTTP: ts.Client()}
}

// Gmail 授权：粘贴回跳地址 → 换 refresh token（PKCE）→ 核对邮箱 → 读邮件 → 授权被撤销时报错。
func TestGmailAuthAndFetch(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	g := &fakeGoogle{email: "user@gmail.com"}
	s.Google = g.serve(t)
	a, _ := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com"})

	if _, err := s.StartGmailAuth(ctx, a.ID); err == nil {
		t.Fatal("没填 OAuth 客户端时不能授权")
	}
	if _, err := s.SetGmailClient(ctx, "123.apps.googleusercontent.com", "sec"); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.GmailClient(ctx); !info.HasSecret || info.ClientID != "123.apps.googleusercontent.com" {
		t.Fatalf("client = %+v", info)
	}
	authURL, err := s.StartGmailAuth(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("login_hint") != "user@gmail.com" || q.Get("scope") != gmailScope || q.Get("code_challenge_method") != "S256" ||
		q.Get("access_type") != "offline" || q.Get("redirect_uri") != gmailRedirect {
		t.Fatalf("auth url = %s", authURL)
	}
	state := q.Get("state")

	if _, err := s.FinishGmailAuth(ctx, a.ID, gmailRedirect+"?state=wrong&code=good-code"); err == nil {
		t.Fatal("state 不对应失败")
	}
	acc, err := s.FinishGmailAuth(ctx, a.ID, gmailRedirect+"?state="+state+"&code=good-code&scope="+url.QueryEscape(gmailScope))
	if err != nil || !acc.GmailAuthorized {
		t.Fatalf("finish = %+v, %v", acc, err)
	}
	if g.verifier == "" {
		t.Fatal("应带 PKCE code_verifier")
	}
	// refresh token 加密保存。
	var raw []byte
	_ = s.DB.QueryRow(`SELECT gmail_token FROM claude_accounts WHERE id = ?`, a.ID).Scan(&raw)
	if strings.Contains(string(raw), "rt1") {
		t.Fatal("refresh token 应加密保存")
	}
	if err := s.TestClaudeAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}

	since := time.Now().Add(-time.Minute)
	if link, err := s.fetchGmailLink(ctx, a.ID, "", since); err != nil || link != "" {
		t.Fatalf("还没有邮件：%q %v", link, err)
	}
	g.mailAt = time.Now().Add(-2 * time.Hour) // 发起之前的旧邮件不算
	if link, _ := s.fetchGmailLink(ctx, a.ID, "", since); link != "" {
		t.Fatalf("旧邮件不应算：%q", link)
	}
	g.mailAt = time.Now()
	if link, err := s.fetchGmailLink(ctx, a.ID, "", since); err != nil || link != testLink {
		t.Fatalf("link = %q, %v", link, err)
	}

	// 访问令牌过期后用 refresh token 刷新；授权被撤销时返回 MailAuthError。
	s.forgetGmailToken(a.ID)
	if _, err := s.gmailAccessToken(ctx, a.ID); err != nil || g.refreshes.Load() != 1 {
		t.Fatalf("refresh: %v, %d", err, g.refreshes.Load())
	}
	g.revoked.Store(true)
	s.forgetGmailToken(a.ID)
	var auth *MailAuthError
	if _, err := s.fetchGmailLink(ctx, a.ID, "", since); !errors.As(err, &auth) {
		t.Fatalf("revoked: %v", err)
	}

	// 授权了别的 Gmail：拒绝。
	g.email = "someone@gmail.com"
	authURL, _ = s.StartGmailAuth(ctx, a.ID)
	u, _ = url.Parse(authURL)
	if _, err := s.FinishGmailAuth(ctx, a.ID, "?state="+u.Query().Get("state")+"&code=good-code"); err == nil ||
		!strings.Contains(err.Error(), "someone@gmail.com") {
		t.Fatalf("wrong account: %v", err)
	}
}

// 客户端经本机服务的流程：发起 → 外壳认领 → 长轮询取链接 → 回报。
func TestClaudeLoginThroughClient(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.FetchMail = func(context.Context, int64, string, time.Time) (string, error) { return testLink, nil }
	seed(t, s)
	if _, err := s.Publish(ctx, ""); err != nil {
		t.Fatal(err)
	}
	d, token, _ := s.CreateDevice(ctx)
	a, _ := s.CreateClaudeAccount(ctx, ClaudeAccountInput{Email: "user@gmail.com", Devices: []int64{d.ID}})
	authorize(t, s, a.ID)
	srvURL, fp := tlsServer(t, s)
	c, _ := NewClient(t.TempDir(), "0.2.0", nil)
	if _, err := c.Bind(ctx, Binding{URL: srvURL, Token: token, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}

	list, err := c.ClaudeAccounts(ctx)
	if err != nil || len(list) != 1 || list[0].Email != "user@gmail.com" {
		t.Fatalf("accounts = %v, %v", list, err)
	}
	l, err := c.StartClaudeLogin(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.PendingClaude(); p == nil || p.ID != l.ID {
		t.Fatalf("pending = %+v", p)
	}
	if _, err := c.ClaimClaudeLogin(l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClaimClaudeLogin(l.ID); err == nil {
		t.Fatal("一次登录只能认领一次")
	}
	if c.PendingClaude() != nil {
		t.Fatal("认领后不应再待启动")
	}
	if _, err := c.OpenClaudeLink(l.ID); err == nil {
		t.Fatal("邮件还没到时不能打开")
	}
	// 邮件到了：链接先留在本机，外壳拿不到，等用户点「打开登录链接」。
	got, err := c.WaitClaudeLink(ctx, l.ID, time.Second)
	if err != nil || got.Link != "" || got.Status != LoginDelivered {
		t.Fatalf("用户点击前不应交出链接：%+v, %v", got, err)
	}
	if cur := c.ClaudeLogin(); cur.Phase != PhaseMailReady {
		t.Fatalf("phase = %s", cur.Phase)
	}
	// 外壳在长轮询时用户点击：立即返回链接。
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = c.OpenClaudeLink(l.ID)
	}()
	got, err = c.WaitClaudeLink(ctx, l.ID, 10*time.Second)
	if err != nil || got.Link != testLink {
		t.Fatalf("link = %+v, %v", got, err)
	}
	if cur := c.ClaudeLogin(); cur.Phase != PhaseOpened {
		t.Fatalf("phase = %s", cur.Phase)
	}
	done, err := c.FinishClaudeLogin(ctx, l.ID, true, "user@gmail.com", "")
	if err != nil || done.Phase != PhaseDone {
		t.Fatalf("finish = %+v, %v", done, err)
	}
	acc, _ := s.claudeAccount(ctx, a.ID)
	if name, _ := deviceInfo(); acc.LastLoginDevice != name { // 客户端上报的设备名
		t.Fatalf("account = %+v", acc)
	}
}
