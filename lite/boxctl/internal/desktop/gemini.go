package desktop

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"boxctl/internal/dist"
)

// geminiRoutes：Gemini（Antigravity）账号（S1.7）。页面用 status、active；
// 托盘外壳用 apply、applied、local（写入与读回本机凭据，见 client/app 的 gemini.rs）。
func geminiRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, c *dist.Client) {
	fail := func(w http.ResponseWriter, err error) {
		var in *dist.InputError
		switch {
		case errors.As(err, &in):
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", in.Msg})
		case errors.Is(err, dist.ErrNotFound):
			writeJSON(w, http.StatusNotFound, apiErr{"not_found", "这次登录已结束或不存在"})
		default:
			writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
		}
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return false
		}
		return true
	}

	mux.Handle("GET /api/v1/desktop/gemini", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, c.GeminiStatus())
	}))
	mux.Handle("POST /api/v1/desktop/gemini/active", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccountID int64 `json:"account_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := c.SetGeminiActive(r.Context(), in.AccountID); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.GeminiStatus())
	}))

	// 一键启动：切到这个账号，外壳写好凭据后启动 Antigravity 应用（参考 cockpit-tools 的「切换并启动」）。
	mux.Handle("POST /api/v1/desktop/gemini/accounts/{id}/launch", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "账号无效"})
			return
		}
		if _, err := c.LaunchGemini(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.GeminiStatus())
	}))

	// 等待中的切换（自动轮换、管理员下发）不等宽限期，马上关闭 Antigravity 切过去。
	mux.Handle("POST /api/v1/desktop/gemini/apply-now", authed(func(w http.ResponseWriter, _ *http.Request) {
		c.ApplyGeminiNow()
		writeJSON(w, http.StatusOK, c.GeminiStatus())
	}))

	// 立即重新查一次额度（BXC-F0-022），结果几秒后出现在状态里。
	mux.Handle("POST /api/v1/desktop/gemini/quota/refresh", authed(func(w http.ResponseWriter, _ *http.Request) {
		c.KickGeminiQuota()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	// 用这个账号的固定浏览器档案打开 Google 账号页（BXC-F0-019）。
	mux.Handle("POST /api/v1/desktop/gemini/accounts/{id}/browser", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "账号无效"})
			return
		}
		l, err := c.OpenGeminiProfile(id, "")
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"group": l.Group})
	}))

	// 一键登录（BXC-F0-020）：外壳用账号的浏览器档案打开 Google 授权页，回跳到本机服务。
	mux.Handle("POST /api/v1/desktop/gemini/login", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccountID int64 `json:"account_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		l, err := c.StartGeminiLogin(in.AccountID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": l})
	}))
	// 回跳打不开时，粘贴浏览器地址栏里的回跳地址完成登录。
	mux.Handle("POST /api/v1/desktop/gemini/login/{id}/callback", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := c.FinishGeminiLoginURL(r.Context(), r.PathValue("id"), in.URL); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.GeminiStatus())
	}))

	// 以下由托盘外壳调用。
	mux.Handle("POST /api/v1/desktop/gemini/browser/{id}/claim", authed(func(w http.ResponseWriter, r *http.Request) {
		l, err := c.ClaimBrowser(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, l)
	}))
	mux.Handle("POST /api/v1/desktop/gemini/launch/{id}/result", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Error string `json:"error"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := c.FinishGeminiLaunch(r.PathValue("id"), in.Error); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("GET /api/v1/desktop/gemini/apply", authed(func(w http.ResponseWriter, r *http.Request) {
		a, err := c.GeminiApplyFresh(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, a)
	}))
	mux.Handle("POST /api/v1/desktop/gemini/applied", authed(func(w http.ResponseWriter, r *http.Request) {
		var in dist.GeminiApplied
		if !decode(w, r, &in) {
			return
		}
		c.SetGeminiApplied(in)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/desktop/gemini/local", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccountID  int64           `json:"account_id"`
			Credential json.RawMessage `json:"credential"`
		}
		if !decode(w, r, &in) {
			return
		}
		uploaded, err := c.UploadLocalGemini(r.Context(), in.AccountID, in.Credential)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"uploaded": uploaded})
	}))
}
