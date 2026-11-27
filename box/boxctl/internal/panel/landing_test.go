package panel

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"boxctl/internal/buildinfo"
)

func TestServerLandingAndAdmin(t *testing.T) {
	static := fstest.MapFS{"index.html": {Data: []byte("<html>SPA-INDEX</html>")}}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	server := NewHandler(Deps{Static: static, Server: true, Logger: slog.New(slog.DiscardHandler)})
	if rec := get(server, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), buildinfo.Version) || strings.Contains(rec.Body.String(), "SPA-INDEX") {
		t.Fatalf("landing: %d %s", rec.Code, rec.Body)
	}
	var info map[string]string
	if rec := get(server, "/?format=json"); json.Unmarshal(rec.Body.Bytes(), &info) != nil || info["version"] != buildinfo.Version {
		t.Fatalf("landing json: %s", rec.Body)
	}
	for _, p := range []string{"/admin", "/admin/", "/admin/devices"} {
		if rec := get(server, p); !strings.Contains(rec.Body.String(), "SPA-INDEX") {
			t.Fatalf("%s should serve the admin app: %s", p, rec.Body)
		}
	}
	// 路由盒、桌面客户端不受影响：根路径仍是前端。
	box := NewHandler(Deps{Static: static, Logger: slog.New(slog.DiscardHandler)})
	if rec := get(box, "/"); !strings.Contains(rec.Body.String(), "SPA-INDEX") {
		t.Fatalf("box root: %s", rec.Body)
	}
}
