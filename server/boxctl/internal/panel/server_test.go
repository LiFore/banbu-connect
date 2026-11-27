package panel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"boxctl/internal/groups"
	"boxctl/internal/guard"
	"boxctl/internal/nodes"
	"boxctl/internal/secret"
	"boxctl/internal/sub"
	"boxctl/internal/subscriptions"
)

const newPassword = "correct horse battery"

type testEnv struct {
	t       *testing.T
	h       http.Handler
	initPwd string
	cookie  *http.Cookie
	csrf    string
}

func newEnv(t *testing.T, guardSock string, static fstest.MapFS) *testEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, authSvc, err := OpenAuth(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := authSvc.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	pwd, err := os.ReadFile(authSvc.InitialPasswordPath)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := secret.LoadOrCreateKey(filepath.Join(dir, "device.key"))
	box, _ := secret.New(key)
	subs := subscriptions.New(db, box, "")
	sample, err := os.ReadFile("../sub/testdata/sample_sub.json")
	if err != nil {
		t.Fatal(err)
	}
	subs.Fetch = func(context.Context, sub.FetchRequest) (sub.FetchResult, error) {
		return sub.FetchResult{Body: sample}, nil
	}
	groupSvc := groups.New(db)
	nodeSvc := nodes.New(db, box)
	nodeSvc.Groups = groupSvc
	return &testEnv{
		t: t,
		h: NewHandler(Deps{DB: db, Auth: authSvc, Subs: subs, Nodes: nodeSvc, Groups: groupSvc, GuardSocket: guardSock, Static: static,
			Logger: slog.New(slog.DiscardHandler)}),
		initPwd: strings.TrimSpace(string(pwd)),
	}
}

// do 发起请求；自动带上已登录的 cookie，csrf 为 true 时带 CSRF 头。
func (e *testEnv) do(method, path string, body any, csrf bool, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	if csrf {
		req.Header.Set(csrfHeader, e.csrf)
	}
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// absorb 从响应中更新 cookie 与 CSRF token。
func (e *testEnv) absorb(rec *httptest.ResponseRecorder) sessionResponse {
	e.t.Helper()
	var s sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		e.t.Fatalf("decode %q: %v", rec.Body, err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
				e.t.Fatalf("insecure cookie %+v", c)
			}
			e.cookie = c
		}
	}
	e.csrf = s.CSRFToken
	return s
}

func (e *testEnv) loginAndChange() {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/auth/login", map[string]string{"password": e.initPwd}, false)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login %d %s", rec.Code, rec.Body)
	}
	e.absorb(rec)
	rec = e.do("POST", "/api/v1/auth/password",
		map[string]string{"old_password": e.initPwd, "new_password": newPassword}, true)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("change password %d %s", rec.Code, rec.Body)
	}
	e.absorb(rec)
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e apiError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

func shortSock(t *testing.T) string {
	dir, err := os.MkdirTemp("", "bx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "g.sock")
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)

	if rec := e.do("GET", "/api/v1/health", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("health without login: %d", rec.Code)
	}
	if s := e.absorb(e.do("GET", "/api/v1/auth/session", nil, false)); s.Authenticated {
		t.Fatal("session before login")
	}

	rec := e.do("POST", "/api/v1/auth/login", map[string]string{"password": e.initPwd}, false)
	if s := e.absorb(rec); rec.Code != http.StatusOK || !s.MustChange {
		t.Fatalf("login: %d %+v", rec.Code, s)
	}

	// 未改密前其它接口被拦截
	if rec := e.do("GET", "/api/v1/health", nil, false); errCode(t, rec) != "password_change_required" {
		t.Fatalf("health before change: %d %s", rec.Code, rec.Body)
	}
	// 写操作缺 CSRF 头被拒
	rec = e.do("POST", "/api/v1/auth/password",
		map[string]string{"old_password": e.initPwd, "new_password": newPassword}, false)
	if errCode(t, rec) != "csrf_failed" {
		t.Fatalf("no csrf: %d %s", rec.Code, rec.Body)
	}
	// 弱密码
	rec = e.do("POST", "/api/v1/auth/password",
		map[string]string{"old_password": e.initPwd, "new_password": "short"}, true)
	if errCode(t, rec) != "weak_password" {
		t.Fatalf("weak: %d %s", rec.Code, rec.Body)
	}

	oldCookie := e.cookie
	rec = e.do("POST", "/api/v1/auth/password",
		map[string]string{"old_password": e.initPwd, "new_password": newPassword}, true)
	if s := e.absorb(rec); rec.Code != http.StatusOK || s.MustChange {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}

	rec = e.do("GET", "/api/v1/health", nil, false)
	var health healthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &health)
	if rec.Code != http.StatusOK || health.GuardError != "unreachable" || health.SchemaVersion < 2 {
		t.Fatalf("health: %d %s", rec.Code, rec.Body)
	}

	// 改密后旧会话失效
	cur := e.cookie
	e.cookie = oldCookie
	if rec := e.do("GET", "/api/v1/health", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session: %d", rec.Code)
	}
	e.cookie = cur

	if rec := e.do("POST", "/api/v1/auth/logout", nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/api/v1/health", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", rec.Code)
	}
}

func TestLoginRejectsCrossSiteAndForms(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)
	body := map[string]string{"password": e.initPwd}

	rec := e.do("POST", "/api/v1/auth/login", body, false, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	if errCode(t, rec) != "bad_origin" {
		t.Fatalf("cross origin: %d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", "/api/v1/auth/login", body, false, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if errCode(t, rec) != "bad_origin" {
		t.Fatalf("sec-fetch-site: %d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", "/api/v1/auth/login", body, false, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form: %d", rec.Code)
	}
	// 同源请求放行
	rec = e.do("POST", "/api/v1/auth/login", body, false, func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("same origin: %d %s", rec.Code, rec.Body)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t, shortSock(t), nil)
	for i := 0; i < 5; i++ {
		rec := e.do("POST", "/api/v1/auth/login", map[string]string{"password": "wrong"}, false)
		if errCode(t, rec) != "bad_password" {
			t.Fatalf("attempt %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := e.do("POST", "/api/v1/auth/login", map[string]string{"password": e.initPwd}, false)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("locked: %d %v", rec.Code, rec.Header())
	}
}

func TestHealthGuardReachable(t *testing.T) {
	sock := shortSock(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- guard.Run(ctx, guard.Options{Socket: sock, DataDir: t.TempDir(), Fake: true, Logger: slog.New(slog.DiscardHandler)})
	}()
	t.Cleanup(func() { cancel(); <-done })

	e := newEnv(t, sock, nil)
	e.loginAndChange()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var resp healthResponse
		_ = json.Unmarshal(e.do("GET", "/api/v1/health", nil, false).Body.Bytes(), &resp)
		if resp.Guard != nil {
			// 启动时先装上一份（没有时为空的）策略，规则就位即为 healthy。
			if resp.Guard.State != guard.StateHealthy || !resp.Guard.Installed {
				t.Fatalf("state %q", resp.Guard.State)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("guard never reachable: %+v", resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSPAFallbackAndHeaders(t *testing.T) {
	static := fstest.MapFS{
		"index.html":    {Data: []byte("<html>app</html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	e := newEnv(t, shortSock(t), static)

	cases := map[string]string{
		"/":                  "<html>app</html>",
		"/subscriptions/abc": "<html>app</html>", // 前端路由刷新不 404
		"/assets/app.js":     "console.log(1)",
	}
	for p, want := range cases {
		rec := e.do("GET", p, nil, false)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "connect-src 'self';") {
			t.Errorf("%s: missing security headers: %q", p, rec.Header().Get("Content-Security-Policy"))
		}
	}
	if rec := e.do("GET", "/api/v1/nope", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("unknown api: %d", rec.Code)
	}
}

func TestRedirectToHTTPS(t *testing.T) {
	rec := httptest.NewRecorder()
	redirectToHTTPS("8443").ServeHTTP(rec, httptest.NewRequest("GET", "http://192.168.1.1:8080/nodes?x=1", nil))
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://192.168.1.1:8443/nodes?x=1" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSecurityHeadersConnectSrc(t *testing.T) {
	h := securityHeaders(func() []string { return []string{"https://*.aliyuncs.com", "http://files.example.com"} },
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' https://*.aliyuncs.com http://files.example.com;") {
		t.Fatalf("connect-src 应追加来源：%q", csp)
	}
}
