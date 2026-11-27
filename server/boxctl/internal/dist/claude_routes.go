package dist

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// claudeAdminRoutes：管理页面的「Claude 账号」。Gmail 的 OAuth 客户端密钥和授权只写不读。
func (s *Server) claudeAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	pathID := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return 0, false
		}
		return id, true
	}

	mux.Handle("GET /api/v1/dist/gmail-client", authed(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.GmailClient(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}))
	mux.Handle("PUT /api/v1/dist/gmail-client", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if !decode(w, r, &in) {
			return
		}
		c, err := s.SetGmailClient(r.Context(), in.ClientID, in.ClientSecret)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}))
	mux.Handle("POST /api/v1/dist/claude-accounts/{id}/gmail/authorize", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		u, err := s.StartGmailAuth(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": u})
	}))
	mux.Handle("POST /api/v1/dist/claude-accounts/{id}/gmail/callback", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			URL string `json:"url"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, err := s.FinishGmailAuth(r.Context(), id, in.URL)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))

	// 自建邮箱（IMAP，如 BillionMail）：服务器统一设置，账号的密码只写不读。
	mux.Handle("GET /api/v1/dist/mail-server", authed(func(w http.ResponseWriter, r *http.Request) {
		m, err := s.MailServer(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}))
	mux.Handle("PUT /api/v1/dist/mail-server", authed(func(w http.ResponseWriter, r *http.Request) {
		var in MailServer
		if !decode(w, r, &in) {
			return
		}
		m, err := s.SetMailServer(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}))
	mux.Handle("DELETE /api/v1/dist/claude-accounts/{id}/mailbox", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		a, err := s.ClearMailbox(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))

	mux.Handle("GET /api/v1/dist/claude-accounts", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.ClaudeAccounts(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/dist/claude-accounts", authed(func(w http.ResponseWriter, r *http.Request) {
		var in ClaudeAccountInput
		if !decode(w, r, &in) {
			return
		}
		a, err := s.CreateClaudeAccount(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	mux.Handle("PUT /api/v1/dist/claude-accounts/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in ClaudeAccountInput
		if !decode(w, r, &in) {
			return
		}
		a, err := s.UpdateClaudeAccount(r.Context(), id, in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	mux.Handle("DELETE /api/v1/dist/claude-accounts/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := s.DeleteClaudeAccount(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	// 测试收信（IMAP 登录或 Gmail 授权）：失败是测试结果，不是服务端故障。
	mux.Handle("POST /api/v1/dist/claude-accounts/{id}/test", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := s.TestClaudeAccount(r.Context(), id); err != nil {
			if err == ErrNotFound {
				fail(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	// 托管的 OAuth 凭据（ADR-061）：只写不读。
	mux.Handle("PUT /api/v1/dist/claude-accounts/{id}/credential", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, err := s.SetClaudeCredential(r.Context(), id, in.AccessToken, in.RefreshToken)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	mux.Handle("DELETE /api/v1/dist/claude-accounts/{id}/credential", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		a, err := s.ClearClaudeCredential(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	// 立即续期（ADR-071）：Claude 先通知客户端，宽限期后交给一台在线客户端执行；状态在账号的 refresh 里。
	mux.Handle("POST /api/v1/dist/claude-accounts/{id}/refresh", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		a, err := s.StartAIRefresh(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
	// 鉴权测试交给一台在线的客户端执行，结果在账号列表的 auth_test 里（页面轮询）。
	mux.Handle("POST /api/v1/dist/claude-accounts/{id}/auth-test", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		t, err := s.StartAuthTest(r.Context(), KindClaude, id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}))
	mux.Handle("POST /api/v1/dist/gemini-accounts/{id}/auth-test", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		t, err := s.StartAuthTest(r.Context(), "gemini", id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}))
	mux.Handle("PUT /api/v1/dist/gemini-accounts/{id}/credential", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, err := s.SetGeminiCredential(r.Context(), id, in.AccessToken, in.RefreshToken)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
}

// claudeClientRoutes：客户端列出可用账号、发起登录、等链接、回报结果。
func (s *Server) claudeClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	s.claudeMailRoutes(mux, device, fail)

	// 长轮询同步 Claude / Codex 账号、凭据和鉴权测试（ADR-061）。响应里有凭据，不缓存。
	mux.Handle("GET /api/v1/client/ai/sync", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		canRefresh := slices.Contains(strings.Split(r.URL.Query().Get("caps"), ","), "refresh")
		out, err := s.AISyncWait(r.Context(), d.ID, r.URL.Query().Get("since"), time.Duration(wait)*time.Second, canRefresh)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	}))
	mux.Handle("PUT /api/v1/client/ai/accounts/{id}/credential", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return
		}
		var in AICredentialUpload
		if !decode(w, r, &in) {
			return
		}
		v, err := s.UploadClaudeCredential(r.Context(), d, id, in)
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
	// 续期（ADR-071）：回报对续期通知的处理（本机是否正在用），交回续期结果。
	mux.Handle("POST /api/v1/client/ai/refresh/{id}/ack", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in struct {
			Busy bool `json:"busy"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.AckAIRefresh(r.Context(), d, r.PathValue("id"), in.Busy); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/client/ai/refresh/{id}/result", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in AIRefreshResult
		if !decode(w, r, &in) {
			return
		}
		err := s.FinishAIRefresh(r.Context(), d, r.PathValue("id"), in)
		if errors.Is(err, ErrCredentialConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"code": "conflict", "message": err.Error()})
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/client/ai/quota", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in struct {
			Samples []AIQuotaSample `json:"samples"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.RecordAIQuota(r.Context(), d, in.Samples); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/client/auth-tests/{id}/result", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in AuthTestResult
		if !decode(w, r, &in) {
			return
		}
		if err := s.FinishAuthTest(r.Context(), d, r.PathValue("id"), in); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("GET /api/v1/client/claude-accounts", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var kinds []string
		if k := r.URL.Query().Get("kinds"); k != "" {
			kinds = strings.Split(k, ",")
		}
		list, err := s.ClientClaudeAccounts(r.Context(), d.ID, kinds...)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/client/claude-accounts/{id}/login", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return
		}
		l, err := s.StartClaudeLogin(r.Context(), d.ID, id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, l)
	}))
	// 长轮询：wait 秒内取到链接就立即返回。链接只在交付的那一次响应里出现。
	mux.Handle("GET /api/v1/client/claude-logins/{id}", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		l, err := s.WaitClaudeLink(r.Context(), d.ID, r.PathValue("id"), time.Duration(wait)*time.Second)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, l)
	}))
	mux.Handle("POST /api/v1/client/claude-logins/{id}/result", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in struct {
			OK    bool   `json:"ok"`
			Email string `json:"email"`
			Error string `json:"error"`
		}
		if !decode(w, r, &in) {
			return
		}
		l, err := s.FinishClaudeLogin(r.Context(), d, r.PathValue("id"), in.OK, in.Email, in.Error)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, l)
	}))
}
