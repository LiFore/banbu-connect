package panel

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const desktopToken = "0123456789abcdef0123456789abcdef"

func desktopHandler(t *testing.T) http.Handler {
	t.Helper()
	svc, err := OpenServices(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.DB.Close() })
	return NewHandler(Deps{
		DB: svc.DB, Subs: svc.Subs, Nodes: svc.Nodes, Groups: svc.Groups, DesktopToken: desktopToken,
		Static: fstest.MapFS{"index.html": {Data: []byte("app")}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ExtraRoutes: func(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
			mux.Handle("POST /api/v1/extra", authed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		},
	})
}

func desktopCall(h http.Handler, method, path, token, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Host = "127.0.0.1:17930"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDesktopModeRequiresToken(t *testing.T) {
	h := desktopHandler(t)
	if w := desktopCall(h, "GET", "/api/v1/groups", "", ""); w.Code != 401 {
		t.Fatalf("无令牌：%d", w.Code)
	}
	if w := desktopCall(h, "GET", "/api/v1/groups", "wrong", ""); w.Code != 401 {
		t.Fatalf("错令牌：%d", w.Code)
	}
	if w := desktopCall(h, "GET", "/api/v1/groups", desktopToken, ""); w.Code != 200 {
		t.Fatalf("正确令牌：%d %s", w.Code, w.Body)
	}
	if w := desktopCall(h, "POST", "/api/v1/extra", "", ""); w.Code != 401 {
		t.Fatalf("附加路由也要认证：%d", w.Code)
	}
	if w := desktopCall(h, "POST", "/api/v1/extra", desktopToken, ""); w.Code != 204 {
		t.Fatalf("写操作不需要 CSRF：%d", w.Code)
	}
	if w := desktopCall(h, "POST", "/api/v1/extra", desktopToken, "https://evil.example.com"); w.Code != 403 {
		t.Fatalf("跨站请求应被拒绝：%d", w.Code)
	}
}

func TestDesktopModeHasNoLogin(t *testing.T) {
	h := desktopHandler(t)
	w := desktopCall(h, "GET", "/api/v1/auth/session", "", "")
	if !strings.Contains(w.Body.String(), `"authenticated":false`) || !strings.Contains(w.Body.String(), `"desktop":true`) {
		t.Fatal(w.Body.String())
	}
	w = desktopCall(h, "GET", "/api/v1/auth/session", desktopToken, "")
	if !strings.Contains(w.Body.String(), `"authenticated":true`) {
		t.Fatal(w.Body.String())
	}
	if w := desktopCall(h, "POST", "/api/v1/auth/login", desktopToken, ""); w.Code != 404 {
		t.Fatalf("桌面模式不应有登录接口：%d", w.Code)
	}
}
