package desktop

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"boxctl/internal/dist"
)

// claudeRoutes：一键发起 Claude Code 登录（BXC-F1-085），流程见 dist/claude_client.go；
// 查看账号 Gmail 里 Anthropic 的邮件、请求在浏览器打开登录链接（见 dist/claude_mail.go）。
// 页面用 accounts、mails、login、open-link；托盘外壳用 claim、link、result、open-link/claim。
func claudeRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, c *dist.Client, check func(kind string) error) {
	fail := func(w http.ResponseWriter, err error) {
		var in *dist.InputError
		var pe *ProtectionError
		switch {
		case errors.As(err, &pe):
			writeJSON(w, http.StatusConflict, map[string]string{"code": "protection", "message": pe.Reason, "fix": pe.Fix})
		case errors.As(err, &in):
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", in.Msg})
		case errors.Is(err, dist.ErrNotFound):
			writeJSON(w, http.StatusNotFound, apiErr{"not_found", "这次登录已结束或不存在"})
		default:
			writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
		}
	}

	mux.Handle("GET /api/v1/desktop/claude/accounts", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := c.ClaudeAccounts(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("GET /api/v1/desktop/claude/login", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"login": c.ClaudeLogin()})
	}))
	mux.Handle("POST /api/v1/desktop/claude/login", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccountID int64 `json:"account_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.AccountID <= 0 {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		// 登录前检查 Claude 的保护状态（BXC-F0-041）。
		if check != nil {
			if err := check(dist.KindClaude); err != nil {
				fail(w, err)
				return
			}
		}
		l, err := c.StartClaudeLogin(r.Context(), in.AccountID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": l})
	}))

	// 账号 Gmail 里 Anthropic 发来的邮件；不要求先点「登录」。
	mux.Handle("GET /api/v1/desktop/claude/accounts/{id}/mails", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "账号无效"})
			return
		}
		list, err := c.ClaudeMails(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, list)
	}))
	// 请求托盘外壳在浏览器打开邮件里的登录链接（页面自己没有打开外部链接的权限）。
	mux.Handle("POST /api/v1/desktop/claude/open-link", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		if err := c.RequestOpenLink(in.URL); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": c.ClaudeLogin()})
	}))

	// 以下由托盘外壳调用。
	mux.Handle("POST /api/v1/desktop/claude/open-link/{id}/claim", authed(func(w http.ResponseWriter, r *http.Request) {
		link, err := c.ClaimOpenLink(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": link})
	}))
	mux.Handle("POST /api/v1/desktop/claude/login/{id}/claim", authed(func(w http.ResponseWriter, r *http.Request) {
		l, err := c.ClaimClaudeLogin(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": l})
	}))
	// 长轮询 magic link：邮件到了、且用户在页面上点了「在浏览器打开」才返回链接，外壳拿到后用浏览器打开。
	mux.Handle("GET /api/v1/desktop/claude/login/{id}/link", authed(func(w http.ResponseWriter, r *http.Request) {
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		if wait <= 0 || wait > 25 {
			wait = 25
		}
		l, err := c.WaitClaudeLink(r.Context(), r.PathValue("id"), time.Duration(wait)*time.Second)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, l)
	}))
	mux.Handle("POST /api/v1/desktop/claude/login/{id}/result", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			OK    bool   `json:"ok"`
			Email string `json:"email"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		l, err := c.FinishClaudeLogin(r.Context(), r.PathValue("id"), in.OK, in.Email, in.Error)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": l})
	}))
}
