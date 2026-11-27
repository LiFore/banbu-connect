package desktop

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boxctl/internal/dist"
)

func TestAppUpdateFlow(t *testing.T) {
	c, err := dist.NewClient(t.TempDir(), "0.2.2", nil)
	if err != nil {
		t.Fatal(err)
	}
	var reported []string
	u := &appUpdater{client: c, report: func(_, msg string, _ map[string]string) { reported = append(reported, msg) }}
	mux := http.NewServeMux()
	u.routes(mux, func(h http.HandlerFunc) http.Handler { return h })
	do := func(method, path, body string) (int, AppUpdateStatus) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		var st AppUpdateStatus
		_ = json.Unmarshal(w.Body.Bytes(), &st)
		return w.Code, st
	}

	// 没绑定管理服务：查不到更新，也不能安装。
	if code, st := do("GET", "/api/v1/desktop/app-update", ""); code != 200 || st.Bound || st.Available != nil {
		t.Fatalf("status: %d %+v", code, st)
	}
	if code, _ := do("POST", "/api/v1/desktop/app-update/install", ""); code != http.StatusConflict {
		t.Fatalf("install without update: %d", code)
	}

	// 有更新时：请求出现在状态里，外壳回报进度，装完才能请求重启。
	u.available = &dist.UpdateInfo{Version: "0.2.3"}
	if code, _ := do("POST", "/api/v1/desktop/app-update/install", ""); code != 200 {
		t.Fatalf("install: %d", code)
	}
	req := u.Pending()
	if req == nil || req.Action != "install" {
		t.Fatalf("pending: %+v", req)
	}
	if code, _ := do("POST", "/api/v1/desktop/app-update/restart", ""); code != http.StatusConflict {
		t.Fatalf("restart before installed: %d", code)
	}
	// 旧请求的进度不覆盖当前请求。
	do("POST", "/api/v1/desktop/app-update/progress", `{"id":"stale","phase":"failed","error":"x"}`)
	if u.status().Progress.Phase != "requested" {
		t.Fatalf("stale progress applied: %+v", u.status().Progress)
	}
	do("POST", "/api/v1/desktop/app-update/progress", `{"id":"`+req.ID+`","phase":"downloading","downloaded":50,"total":100}`)
	if p := u.status().Progress; p.Phase != "downloading" || p.Downloaded != 50 {
		t.Fatalf("progress: %+v", p)
	}
	do("POST", "/api/v1/desktop/app-update/progress", `{"id":"`+req.ID+`","phase":"installed","version":"0.2.3"}`)
	if code, _ := do("POST", "/api/v1/desktop/app-update/restart", ""); code != 200 || u.Pending().Action != "restart" {
		t.Fatalf("restart: %d %+v", code, u.Pending())
	}

	// 失败进事件和诊断。
	u.requestAction("install")
	do("POST", "/api/v1/desktop/app-update/progress", `{"id":"`+u.Pending().ID+`","phase":"failed","version":"0.2.3","error":"signature"}`)
	if len(reported) != 1 || !strings.Contains(reported[0], "signature") {
		t.Fatalf("reported: %v", reported)
	}
}

// 重启请求要过期：后台服务没更新（不重启）时，留着的请求会让重启后的外壳再次重启，无限循环。
func TestRestartRequestExpires(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	u := &appUpdater{now: func() time.Time { return now }}
	u.requestAction("restart")
	if p := u.Pending(); p == nil || p.Action != "restart" {
		t.Fatalf("刚发出的重启请求应在状态里：%+v", p)
	}
	now = now.Add(restartTTL + time.Second)
	if p := u.Pending(); p != nil {
		t.Fatalf("重启请求过期后不应再出现：%+v", p)
	}
	// 安装请求不过期：外壳不在时用户点了更新，外壳启动后接着做。
	u.requestAction("install")
	now = now.Add(time.Hour)
	if p := u.Pending(); p == nil || p.Action != "install" {
		t.Fatalf("安装请求不应过期：%+v", p)
	}
}
