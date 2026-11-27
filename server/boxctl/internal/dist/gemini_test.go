package dist

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testIDToken 生成一个只有 email 声明的 JWT（不签名，服务端只读不校验）。
func testIDToken(email string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"email":%q}`, email))) + ".sig"
}

func testCredential(email, refresh string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"token":       map[string]string{"access_token": "ya29.x", "token_type": "Bearer", "refresh_token": refresh, "expiry": "2026-01-01T10:00:00.000000Z"},
		"auth_method": "consumer",
		"id_token":    testIDToken(email),
	})
	return b
}

func newDevice(t *testing.T, s *Server) Device {
	t.Helper()
	d, _, err := s.CreateDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGeminiAccountCRUD(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2, d3 := newDevice(t, s), newDevice(t, s), newDevice(t, s)

	a, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: " A@Gmail.com ", Devices: []int64{d1.ID, d1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "a@gmail.com" || a.MaxDevices != 2 || len(a.Devices) != 1 || a.HasCredential {
		t.Fatalf("account = %+v", a)
	}
	if _, err := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com"}); err == nil {
		t.Fatal("duplicate email should fail")
	}
	// 超过设备上限。
	if _, err := s.UpdateGeminiAccount(ctx, a.ID, GeminiAccountInput{Email: a.Email, Devices: []int64{d1.ID, d2.ID, d3.ID}}); err == nil {
		t.Fatal("over max_devices should fail")
	}
	// 不存在的客户端。
	if _, err := s.UpdateGeminiAccount(ctx, a.ID, GeminiAccountInput{Email: a.Email, Devices: []int64{999}}); err == nil {
		t.Fatal("unknown device should fail")
	}
	a, err = s.UpdateGeminiAccount(ctx, a.ID, GeminiAccountInput{Email: a.Email, MaxDevices: 3, Devices: []int64{d1.ID, d2.ID, d3.ID}})
	if err != nil || len(a.Devices) != 3 {
		t.Fatalf("update = %+v, %v", a, err)
	}
	// 删除客户端时分配一起删除。
	if err := s.DeleteDevice(ctx, d3.ID); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.geminiAccount(ctx, a.ID); len(a.Devices) != 2 {
		t.Fatalf("devices after delete = %v", a.Devices)
	}
	if err := s.DeleteGeminiAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.GeminiAccounts(ctx); len(list) != 0 {
		t.Fatalf("list = %v", list)
	}
}

func TestGeminiCredentialUpload(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2, other := newDevice(t, s), newDevice(t, s), newDevice(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d1.ID, d2.ID}})

	// 没分配到的客户端不能上传。
	if _, err := s.UploadGeminiCredential(ctx, other, a.ID, CredentialUpload{Replace: true, Credential: testCredential("a@gmail.com", "r1")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unassigned upload err = %v", err)
	}
	// 别的账号的凭据。
	if _, err := s.UploadGeminiCredential(ctx, d1, a.ID, CredentialUpload{Replace: true, Credential: testCredential("b@gmail.com", "r1")}); err == nil {
		t.Fatal("wrong email should fail")
	}
	// 没有 refresh token。
	if _, err := s.UploadGeminiCredential(ctx, d1, a.ID, CredentialUpload{Replace: true, Credential: json.RawMessage(`{"token":{}}`)}); err == nil {
		t.Fatal("missing refresh token should fail")
	}
	v, err := s.UploadGeminiCredential(ctx, d1, a.ID, CredentialUpload{Replace: true, Source: "login", Credential: testCredential("a@gmail.com", "r1")})
	if err != nil || v != 1 {
		t.Fatalf("login upload = %d, %v", v, err)
	}
	// d2 拿着版本 1 刷新成功；d1 还拿着版本 0（过期）上传被拒绝。
	if v, err = s.UploadGeminiCredential(ctx, d2, a.ID, CredentialUpload{BaseVersion: 1, Credential: testCredential("a@gmail.com", "r2")}); err != nil || v != 2 {
		t.Fatalf("refresh upload = %d, %v", v, err)
	}
	if _, err = s.UploadGeminiCredential(ctx, d1, a.ID, CredentialUpload{BaseVersion: 1, Credential: testCredential("a@gmail.com", "r3")}); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("stale upload err = %v", err)
	}
	raw, ver, err := s.geminiCredential(ctx, a.ID)
	if err != nil || ver != 2 {
		t.Fatal(ver, err)
	}
	if info, _ := ParseGeminiCredential(raw); info.RefreshToken != "r2" || info.Email != "a@gmail.com" {
		t.Fatalf("stored = %+v", info)
	}
	// 数据库里是密文。
	var sealed []byte
	_ = s.DB.QueryRow(`SELECT credential FROM gemini_accounts WHERE id = ?`, a.ID).Scan(&sealed)
	if string(sealed) == string(raw) {
		t.Fatal("credential stored in plaintext")
	}
	// 清除凭据：版本号继续递增。
	acc, err := s.ClearGeminiCredential(ctx, a.ID)
	if err != nil || acc.HasCredential || acc.CredVersion != 3 {
		t.Fatalf("clear = %+v, %v", acc, err)
	}
	// 换邮箱也清除凭据。
	_, _ = s.UploadGeminiCredential(ctx, d1, a.ID, CredentialUpload{Replace: true, Credential: testCredential("a@gmail.com", "r4")})
	acc, _ = s.UpdateGeminiAccount(ctx, a.ID, GeminiAccountInput{Email: "c@gmail.com", Devices: []int64{d1.ID}})
	if acc.HasCredential {
		t.Fatal("changing email should clear the credential")
	}
}

// boundClient 建一台客户端并绑定到测试管理服务。
func boundClient(t *testing.T, s *Server) (*Client, Device) {
	t.Helper()
	ctx := context.Background()
	d, token, err := s.CreateDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, fp := tlsServer(t, s)
	c, err := NewClient(t.TempDir(), "0.2.6", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.binding = &Binding{URL: u, Token: token, Fingerprint: fp}
	c.mu.Unlock()
	return c, d
}

func TestGeminiSyncWait(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d := newDevice(t, s)
	first, err := s.GeminiSyncWait(ctx, d.ID, "", time.Second)
	if err != nil || len(first.Accounts) != 0 || first.Hash == "" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	// 没有变化时等满。
	start := time.Now()
	if out, _ := s.GeminiSyncWait(ctx, d.ID, first.Hash, 200*time.Millisecond); out.Hash != first.Hash || time.Since(start) < 150*time.Millisecond {
		t.Fatal("should wait until timeout")
	}
	// 分配了账号就立即返回。
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	}()
	start = time.Now()
	out, err := s.GeminiSyncWait(ctx, d.ID, first.Hash, 5*time.Second)
	if err != nil || len(out.Accounts) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("out = %+v, %v", out, err)
	}
}

func TestGeminiClientSync(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	other := newDevice(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	b, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "b@gmail.com", Devices: []int64{d.ID, other.ID}})
	_, _ = s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "c@gmail.com", Devices: []int64{other.ID}})

	// 还没同步时外壳什么都不做。
	if rev := c.GeminiApplyRev(); rev != "" {
		t.Fatalf("rev before sync = %q", rev)
	}
	sync := func() {
		t.Helper()
		if err := c.syncGemini(ctx, 0); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	if st := c.GeminiStatus(); len(st.Accounts) != 2 || st.ActiveID != 0 {
		t.Fatalf("status = %+v", st)
	}
	if rev := c.GeminiApplyRev(); rev != "0:0" {
		t.Fatalf("rev without credential = %q", rev)
	}

	// a 有了凭据：自动成为当前账号。
	if _, err := s.UploadGeminiCredential(ctx, d, a.ID, CredentialUpload{Replace: true, Source: "login", Credential: testCredential("a@gmail.com", "ra")}); err != nil {
		t.Fatal(err)
	}
	sync()
	if rev := c.GeminiApplyRev(); rev != fmt.Sprintf("%d:1", a.ID) {
		t.Fatalf("rev = %q", rev)
	}
	ap, _ := c.GeminiApplyCredential()
	if ap.Email != "a@gmail.com" || len(ap.Credential) == 0 {
		t.Fatalf("apply = %+v", ap)
	}
	var active int64
	_ = s.DB.QueryRow(`SELECT active_account_id FROM gemini_device_state WHERE device_id = ?`, d.ID).Scan(&active)
	if active != a.ID {
		t.Fatalf("server sees active %d", active)
	}

	// 本机刷新过（同一 refresh token、更晚过期）：上传，版本号加 1。
	refreshed, _ := json.Marshal(map[string]any{
		"token":    map[string]string{"access_token": "ya29.new", "refresh_token": "ra", "expiry": "2026-11-23T11:00:00Z"},
		"id_token": testIDToken("a@gmail.com"),
	})
	if ok, err := c.UploadLocalGemini(ctx, a.ID, refreshed); err != nil || !ok {
		t.Fatalf("upload = %v, %v", ok, err)
	}
	if rev := c.GeminiApplyRev(); rev != fmt.Sprintf("%d:2", a.ID) {
		t.Fatalf("rev after upload = %q", rev)
	}
	// 一样的再传一次：不上传。
	if ok, _ := c.UploadLocalGemini(ctx, a.ID, refreshed); ok {
		t.Fatal("same credential should not upload")
	}
	// 本机登录成了别的账号：不上传。
	if _, err := c.UploadLocalGemini(ctx, a.ID, testCredential("x@gmail.com", "rx")); err == nil {
		t.Fatal("other account should be rejected")
	}

	// 管理页面下发切换到 b。
	if _, err := s.RequestGeminiSwitch(ctx, d.ID, b.ID); err == nil {
		t.Fatal("switch to account without credential should fail")
	}
	_, _ = s.UploadGeminiCredential(ctx, other, b.ID, CredentialUpload{Replace: true, Credential: testCredential("b@gmail.com", "rb")})
	if _, err := s.RequestGeminiSwitch(ctx, d.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	sync()
	if st := c.GeminiStatus(); st.ActiveID != b.ID {
		t.Fatalf("active after switch = %d", st.ActiveID)
	}
	var pending string
	_ = s.DB.QueryRow(`SELECT active_account_id, switch_id FROM gemini_device_state WHERE device_id = ?`, d.ID).Scan(&active, &pending)
	if active != b.ID || pending != "" {
		t.Fatalf("server state = %d %q", active, pending)
	}

	// 手动切回 a；取消分配 a 后自动换到 b。
	if err := c.SetGeminiActive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = s.UpdateGeminiAccount(ctx, a.ID, GeminiAccountInput{Email: "a@gmail.com"})
	sync()
	if st := c.GeminiStatus(); st.ActiveID != b.ID || len(st.Accounts) != 1 {
		t.Fatalf("after unassign = %+v", st)
	}
	// 本机选择跨重启保留。
	c2, _ := NewClient(c.dir, "0.2.6", slog.New(slog.DiscardHandler))
	if st := c2.GeminiStatus(); st.ActiveID != b.ID {
		t.Fatalf("restored active = %d", st.ActiveID)
	}
	// 解绑后清空：外壳删除本机凭据。
	c.mu.Lock()
	c.binding = nil
	c.mu.Unlock()
	c.resetGemini()
	if rev := c.GeminiApplyRev(); rev != "0:0" {
		t.Fatalf("rev after unbind = %q", rev)
	}
}

func TestGeminiBrowserProfile(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	if _, err := c.OpenGeminiProfile(a.ID, ""); err == nil {
		t.Fatal("unsynced account should be rejected")
	}
	if err := c.syncGemini(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.OpenGeminiProfile(a.ID, "https://evil.example/"); err == nil {
		t.Fatal("non-Google url should be rejected")
	}
	c.GeminiRoute = func() (string, string) { return "Google", "http://127.0.0.1:19190" }
	if _, err := c.OpenGeminiProfile(a.ID, ""); err != nil {
		t.Fatal(err)
	}
	p := c.PendingBrowser()
	if p == nil {
		t.Fatal("no pending browser")
	}
	l, err := c.ClaimBrowser(p.ID)
	if err != nil || l.Profile != "a@gmail.com" || l.URL != geminiAccountURL || l.Proxy != "http://127.0.0.1:19190" {
		t.Fatalf("launch = %+v, %v", l, err)
	}
	// 只能认领一次。
	if _, err := c.ClaimBrowser(p.ID); err == nil || c.PendingBrowser() != nil {
		t.Fatal("claim twice should fail")
	}
}

// fakeGeminiGoogle 替换 Google 的接口：token 接口按 grant_type 返回，记下收到的表单。
func fakeGeminiGoogle(t *testing.T, email string) *[]url.Values {
	t.Helper()
	var forms []url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms = append(forms, r.PostForm)
		if r.PostForm.Get("code") == "bad" || r.PostForm.Get("refresh_token") == "revoked" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
			return
		}
		resp := map[string]any{"access_token": "ya29.fresh", "expires_in": 3599, "token_type": "Bearer", "id_token": testIDToken(email)}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			resp["refresh_token"] = "1//refresh"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	old := geminiEndpoints
	geminiEndpoints.Token, geminiEndpoints.UserInfo, geminiEndpoints.CloudCode = ts.URL+"/token", ts.URL+"/userinfo", ts.URL
	t.Cleanup(func() { geminiEndpoints = old })
	return &forms
}

func TestGeminiLogin(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	c, d := boundClient(t, s)
	a, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "a@gmail.com", Devices: []int64{d.ID}})
	forms := fakeGeminiGoogle(t, "a@gmail.com")
	_ = c.syncGemini(ctx, 0)

	// 没设置 OAuth 客户端时不能登录。
	if _, err := c.StartGeminiLogin(a.ID); err == nil {
		t.Fatal("login without oauth client should fail")
	}
	if _, err := s.SetGeminiOAuth(ctx, DefaultGeminiClientID, "secret-x"); err != nil {
		t.Fatal(err)
	}
	_ = c.syncGemini(ctx, 0)
	l, err := c.StartGeminiLogin(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 外壳认领到的是 Google 授权页，带上 login_hint 和 PKCE。
	launch, err := c.ClaimBrowser(c.PendingBrowser().ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(launch.URL)
	q := u.Query()
	if !strings.HasPrefix(launch.URL, "https://accounts.google.com/") || q.Get("login_hint") != "a@gmail.com" ||
		q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" {
		t.Fatalf("auth url = %s", launch.URL)
	}
	if st := c.GeminiStatus(); st.Login == nil || st.Login.Phase != GeminiPhaseAuthorize {
		t.Fatalf("login = %+v", st.Login)
	}
	// 别的 state 不接受。
	if err := c.FinishGeminiLoginURL(ctx, l.ID, "http://127.0.0.1:1/oauth-callback?state=x&code=c"); err == nil {
		t.Fatal("wrong state should fail")
	}
	// 浏览器回跳到本机服务。
	resp, err := http.Get(q.Get("redirect_uri") + "?state=" + q.Get("state") + "&code=good")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	st := c.GeminiStatus()
	if st.Login == nil || st.Login.Phase != GeminiPhaseDone || st.ActiveID != a.ID {
		t.Fatalf("after callback = %+v %+v", st, st.Login)
	}
	f := (*forms)[0]
	if f.Get("client_secret") != "secret-x" || f.Get("code_verifier") == "" || f.Get("redirect_uri") != q.Get("redirect_uri") {
		t.Fatalf("token form = %v", f)
	}
	raw, ver, _ := s.geminiCredential(ctx, a.ID)
	info, _ := ParseGeminiCredential(raw)
	if ver != 1 || info.RefreshToken != "1//refresh" || info.Email != "a@gmail.com" {
		t.Fatalf("stored = %d %+v", ver, info)
	}
	if rev := c.GeminiApplyRev(); rev != fmt.Sprintf("%d:1", a.ID) {
		t.Fatalf("rev = %q", rev)
	}
	// 登录成了别的账号：拒绝。
	_ = c.syncGemini(ctx, 0)
	b, _ := s.CreateGeminiAccount(ctx, GeminiAccountInput{Email: "b@gmail.com", Devices: []int64{d.ID}})
	_ = c.syncGemini(ctx, 0)
	l, _ = c.StartGeminiLogin(b.ID)
	u, _ = url.Parse(launchURL(t, c))
	if err := c.FinishGeminiLoginURL(ctx, l.ID, u.Query().Get("redirect_uri")+"?state="+u.Query().Get("state")+"&code=good"); err == nil ||
		!strings.Contains(err.Error(), "a@gmail.com") {
		t.Fatalf("wrong account err = %v", err)
	}
}

func launchURL(t *testing.T, c *Client) string {
	t.Helper()
	l, err := c.ClaimBrowser(c.PendingBrowser().ID)
	if err != nil {
		t.Fatal(err)
	}
	return l.URL
}
