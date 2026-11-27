package dist

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// geminiAdminRoutes：管理页面的「Gemini 账号」。凭据只写不读。
func (s *Server) geminiAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	pathID := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return 0, false
		}
		return id, true
	}

	mux.Handle("GET /api/v1/dist/gemini-oauth", authed(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.GeminiOAuthInfo(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}))
	mux.Handle("PUT /api/v1/dist/gemini-oauth", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if !decode(w, r, &in) {
			return
		}
		c, err := s.SetGeminiOAuth(r.Context(), in.ClientID, in.ClientSecret)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}))
	mux.Handle("GET /api/v1/dist/gemini-accounts", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.GeminiAccounts(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/dist/gemini-accounts", authed(func(w http.ResponseWriter, r *http.Request) {
		var in GeminiAccountInput
		if !decode(w, r, &in) {
			return
		}
		a, err := s.CreateGeminiAccount(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	mux.Handle("PUT /api/v1/dist/gemini-accounts/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in GeminiAccountInput
		if !decode(w, r, &in) {
			return
		}
		a, err := s.UpdateGeminiAccount(r.Context(), id, in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	mux.Handle("DELETE /api/v1/dist/gemini-accounts/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := s.DeleteGeminiAccount(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	// 额度、各客户端当前账号、建议顺序和切换建议（BXC-F0-023）。
	mux.Handle("GET /api/v1/dist/gemini-overview", authed(func(w http.ResponseWriter, r *http.Request) {
		o, err := s.GeminiOverview(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, o)
	}))
	// 让客户端切换到指定账号，客户端下一次同步时执行（通常几秒内）。
	mux.Handle("POST /api/v1/dist/gemini-devices/{id}/switch", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			AccountID int64 `json:"account_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		sw, err := s.RequestGeminiSwitch(r.Context(), id, in.AccountID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sw)
	}))
	mux.Handle("DELETE /api/v1/dist/gemini-accounts/{id}/credential", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		a, err := s.ClearGeminiCredential(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
}

// geminiClientRoutes：客户端同步账号与凭据、上传凭据、回报当前账号。
func (s *Server) geminiClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }

	// 长轮询：hash 与 since 不同或等满 wait 秒返回。响应里有凭据，不缓存。
	mux.Handle("GET /api/v1/client/gemini/sync", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		out, err := s.GeminiSyncWait(r.Context(), d.ID, r.URL.Query().Get("since"), time.Duration(wait)*time.Second)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	}))
	// 版本号不是最新时返回 409 和最新版本号，客户端等下一次同步拿到最新凭据。
	mux.Handle("PUT /api/v1/client/gemini/accounts/{id}/credential", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return
		}
		var in CredentialUpload
		if !decode(w, r, &in) {
			return
		}
		v, err := s.UploadGeminiCredential(r.Context(), d, id, in)
		if errors.Is(err, ErrCredentialConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"code": "conflict", "message": err.Error(), "version": v})
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"version": v})
	}))
	mux.Handle("POST /api/v1/client/gemini/quota", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in struct {
			Samples []GeminiQuotaSample `json:"samples"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.RecordGeminiQuota(r.Context(), d.ID, in.Samples); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/client/gemini/state", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in GeminiStateReport
		if !decode(w, r, &in) {
			return
		}
		if err := s.ReportGeminiState(r.Context(), d.ID, in); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}
