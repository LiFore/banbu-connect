package desktop

import (
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"

	"boxctl/internal/dist"
)

// serverRoutes 挂绑定管理服务和接入申请的接口，路径为 /api/v1/desktop/server...。
func serverRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, c *dist.Client) {
	const base = "/api/v1/desktop/server"
	fail := func(w http.ResponseWriter, err error) {
		var in *dist.InputError
		if errors.As(err, &in) {
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", in.Msg})
			return
		}
		writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
	}
	decodeBody := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return false
		}
		return true
	}
	// 绑定或解绑后马上同步账号，不等下一轮。
	kick := func() { c.Kick() }

	mux.Handle("GET "+base, authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, c.Status())
	}))
	mux.Handle("POST "+base+"/bind", authed(func(w http.ResponseWriter, r *http.Request) {
		var in dist.Binding
		if !decodeBody(w, r, &in) {
			return
		}
		st, err := c.Bind(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		_ = c.CancelEnrollment() // 手动绑定后，之前的申请没用了
		kick()
		writeJSON(w, http.StatusOK, st)
	}))
	// 接入申请：提交设备信息，管理员授权后点「刷新授权」自动领取令牌并绑定。
	enrollView := func(e *dist.EnrollmentView) map[string]any {
		return map[string]any{"default_url": dist.DefaultServerURL, "default_name": deviceName(), "enrollment": e}
	}
	mux.Handle("GET "+base+"/enroll", authed(func(w http.ResponseWriter, _ *http.Request) {
		e, err := c.Enrollment()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrollView(e))
	}))
	mux.Handle("POST "+base+"/enroll", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL         string `json:"url"`
			Fingerprint string `json:"fingerprint"`
			Name        string `json:"name"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.URL) == "" {
			in.URL = dist.DefaultServerURL
		}
		e, err := c.Enroll(r.Context(), in.URL, in.Fingerprint, dist.EnrollRequest{
			Name: in.Name, Platform: runtime.GOOS + "/" + runtime.GOARCH, OSVersion: osVersion(),
		})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrollView(e))
	}))
	mux.Handle("POST "+base+"/enroll/refresh", authed(func(w http.ResponseWriter, r *http.Request) {
		e, bound, err := c.RefreshEnrollment(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if bound {
			kick()
		}
		out := enrollView(e)
		out["bound"] = bound
		out["server"] = c.Status()
		writeJSON(w, http.StatusOK, out)
	}))
	mux.Handle("DELETE "+base+"/enroll", authed(func(w http.ResponseWriter, _ *http.Request) {
		if err := c.CancelEnrollment(); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrollView(nil))
	}))
	mux.Handle("POST "+base+"/unbind", authed(func(w http.ResponseWriter, _ *http.Request) {
		if err := c.Unbind(); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.Status())
	}))
	mux.Handle("POST "+base+"/sync", authed(func(w http.ResponseWriter, r *http.Request) {
		if _, err := c.Sync(r.Context()); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.Status())
	}))
}
