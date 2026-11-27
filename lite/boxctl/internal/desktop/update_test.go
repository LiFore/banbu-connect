package desktop

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdateReport(t *testing.T) {
	type rec struct {
		level, msg string
		fields     map[string]string
	}
	var got []rec
	mux := http.NewServeMux()
	updateRoutes(mux, func(h http.HandlerFunc) http.Handler { return h }, nil, func(level, msg string, fields map[string]string) {
		got = append(got, rec{level, msg, fields})
	}, nil)
	post := func(body string) int {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/desktop/update/report", strings.NewReader(body)))
		return w.Code
	}
	if code := post(`{"stage":"install","version":"0.2.0","error":"signature verification failed"}`); code != 200 {
		t.Fatalf("code %d", code)
	}
	if len(got) != 1 || got[0].level != "error" || !strings.Contains(got[0].msg, "安装更新失败（0.2.0）：signature") || got[0].fields["stage"] != "install" {
		t.Fatalf("recorded: %+v", got)
	}
	if code := post(`{"stage":"evil","error":"x"}`); code != 200 || got[1].fields["stage"] != "other" {
		t.Fatalf("unknown stage: %d %+v", code, got)
	}
	if code := post(`{"stage":"install"}`); code != 400 {
		t.Fatalf("empty error should be rejected: %d", code)
	}
}
