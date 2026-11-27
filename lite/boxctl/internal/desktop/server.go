package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"boxctl/internal/dist"
)

// updateReporter 记下更新相关的失败：写日志、进事件（概览可见）、进诊断数据（上报给管理服务）。
type updateReporter func(level, msg string, fields map[string]string)

// updateRoutes：给 Tauri updater 用的更新接口，target 加上 Lite 版的前缀（见 updateTargetPrefix）。绑定管理服务的接口见 serverRoutes。
// 更新接口由本机服务转发到管理服务：外壳拿不到服务端令牌，也不用处理自签名证书。
func updateRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, c *dist.Client, report updateReporter, onAppVersion func(string)) {
	fail := func(w http.ResponseWriter, err error) {
		var in *dist.InputError
		if errors.As(err, &in) {
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", in.Msg})
			return
		}
		writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
	}
	// Tauri updater 的动态更新接口：没有更新时 204。
	mux.Handle("GET /api/v1/desktop/update/{target}/{arch}/{current}", authed(func(w http.ResponseWriter, r *http.Request) {
		if onAppVersion != nil {
			onAppVersion(r.PathValue("current"))
		}
		u, err := c.CheckUpdate(r.Context(), updateTargetPrefix+r.PathValue("target"), r.PathValue("arch"), r.PathValue("current"))
		if err != nil {
			report("warn", "检查更新失败："+err.Error(), map[string]string{"stage": "check", "current": r.PathValue("current")})
			fail(w, err)
			return
		}
		if u == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"version":   u.Version,
			"notes":     u.Notes,
			"pub_date":  u.PubDate,
			"signature": u.Signature,
			// Tauri updater 下载时带着同样的令牌头访问本机服务。
			"url": fmt.Sprintf("http://%s/api/v1/desktop/update/download/%d", r.Host, u.ReleaseID),
		})
	}))
	mux.Handle("GET /api/v1/desktop/update/download/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusNotFound, apiErr{"not_found", "更新包不存在"})
			return
		}
		if err := c.Download(r.Context(), id, w); err != nil {
			report("error", "下载更新包失败："+err.Error(), map[string]string{"stage": "download", "release": r.PathValue("id")})
			fail(w, err) // 已经开始传输时写不进状态码，外壳会看到包不完整、签名校验失败
		}
	}))
	// 外壳回报更新失败（检查、下载、校验、安装）：外壳从「应用程序」启动时没有终端，错误只能经这里看到。
	mux.Handle("POST /api/v1/desktop/update/report", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Stage   string `json:"stage"`
			Version string `json:"version"`
			Error   string `json:"error"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.Error == "" {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		stage := map[string]string{"check": "检查更新", "download": "下载更新", "install": "安装更新"}[in.Stage]
		if stage == "" {
			stage, in.Stage = "更新", "other"
		}
		msg := stage + "失败"
		if in.Version != "" {
			msg += "（" + clip(in.Version, 32) + "）"
		}
		report("error", msg+"："+clip(strings.TrimSpace(in.Error), 400), map[string]string{"stage": in.Stage, "version": clip(in.Version, 32)})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
