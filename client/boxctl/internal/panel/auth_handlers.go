package panel

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"boxctl/internal/auth"
)

const (
	sessionCookie = "boxctl_session"
	csrfHeader    = "X-CSRF-Token"
	maxBodyBytes  = 64 << 10
)

type requestSession struct {
	auth.Session
	token string
}

type authFlags int

// allowMustChange：首次登录未改密时仍可访问（改密、登出）。
const allowMustChange authFlags = 1

// authed 校验会话；对写操作额外校验 CSRF token（M1-06/M1-07）。
func (h *handler) authed(fn func(http.ResponseWriter, *http.Request, *requestSession), flags authFlags) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 桌面模式没有 Cookie，跨站页面无法附带 Bearer 令牌，所以不需要 CSRF token。
		if h.DesktopToken != "" {
			if !h.desktopAuthorized(r) {
				writeError(w, http.StatusUnauthorized, "unauthenticated", "令牌无效，请从桌面客户端打开")
				return
			}
			fn(w, r, &requestSession{})
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
			return
		}
		sess, err := h.Auth.Authenticate(r.Context(), c.Value)
		if errors.Is(err, auth.ErrNoSession) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "会话无效或已过期")
			return
		}
		if err != nil {
			h.internalError(w, err)
			return
		}
		if sess.MustChange && flags&allowMustChange == 0 {
			writeError(w, http.StatusForbidden, "password_change_required", "首次登录需先修改密码")
			return
		}
		if !isSafeMethod(r.Method) {
			got := r.Header.Get(csrfHeader)
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
				writeError(w, http.StatusForbidden, "csrf_failed", "CSRF 校验失败")
				return
			}
		}
		fn(w, r, &requestSession{Session: sess, token: c.Value})
	})
}

type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	MustChange    bool   `json:"must_change"`
	CSRFToken     string `json:"csrf_token,omitempty"`
	Desktop       bool   `json:"desktop,omitempty"`
	Server        bool   `json:"server,omitempty"`
	Box           bool   `json:"box,omitempty"`
}

func (h *handler) desktopAuthorized(r *http.Request) bool {
	want := []byte("Bearer " + h.DesktopToken)
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) == 1
}

func (h *handler) desktopSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: h.desktopAuthorized(r), Desktop: true})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, sess, err := h.Auth.Login(r.Context(), clientIP(r), req.Password)
	if h.writeAuthError(w, err) {
		return
	}
	setSessionCookie(w, token, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, MustChange: sess.MustChange, CSRFToken: sess.CSRFToken, Server: h.Server, Box: h.Box})
}

func (h *handler) session(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		writeJSON(w, http.StatusOK, sessionResponse{Server: h.Server, Box: h.Box})
		return
	}
	sess, err := h.Auth.Authenticate(r.Context(), c.Value)
	if errors.Is(err, auth.ErrNoSession) {
		writeJSON(w, http.StatusOK, sessionResponse{Server: h.Server, Box: h.Box})
		return
	}
	if err != nil {
		h.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, MustChange: sess.MustChange, CSRFToken: sess.CSRFToken, Server: h.Server, Box: h.Box})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request, s *requestSession) {
	if err := h.Auth.Logout(r.Context(), s.token); err != nil {
		h.internalError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) changePassword(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, sess, err := h.Auth.ChangePassword(r.Context(), clientIP(r), req.OldPassword, req.NewPassword)
	if h.writeAuthError(w, err) {
		return
	}
	setSessionCookie(w, token, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, sessionResponse{Authenticated: true, MustChange: false, CSRFToken: sess.CSRFToken})
}

// writeAuthError 把认证错误映射成 API 错误；err 为 nil 时返回 false。
// 密码错误用 403 而不是 401：401 在前端意味着“会话失效，跳登录页”。
func (h *handler) writeAuthError(w http.ResponseWriter, err error) bool {
	var locked *auth.LockedError
	switch {
	case err == nil:
		return false
	case errors.As(err, &locked):
		secs := int(math.Ceil(time.Until(locked.Until).Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		writeError(w, http.StatusTooManyRequests, "locked", "失败次数过多，请稍后再试")
	case errors.Is(err, auth.ErrBadPassword):
		writeError(w, http.StatusForbidden, "bad_password", "密码错误")
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "weak_password",
			fmt.Sprintf("新密码至少 %d 位，且不能与旧密码相同", auth.MinPasswordLen))
	default:
		h.internalError(w, err)
	}
	return true
}

func (h *handler) internalError(w http.ResponseWriter, err error) {
	h.Logger.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "内部错误")
}

func setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// decodeJSON 只接受 application/json：浏览器跨站表单无法构造该类型的简单请求。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "bad_content_type", "需要 application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin 拒绝跨站的写请求（包括尚无会话的登录接口）。
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					writeError(w, http.StatusForbidden, "bad_origin", "跨站请求被拒绝")
					return
				}
			} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeError(w, http.StatusForbidden, "bad_origin", "跨站请求被拒绝")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders 加安全响应头。connectSrc 非空时，其返回的来源追加到 CSP 的 connect-src
// （管理服务：浏览器把云盘文件分片直传到 OSS，不放行时 XHR 被 CSP 拦下，只报「网络错误」）。
func securityHeaders(connectSrc func() []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		connect := "'self'"
		if connectSrc != nil {
			for _, src := range connectSrc() {
				connect += " " + src
			}
		}
		// antd 使用 CSS-in-JS 注入 style，需放行 inline style
		h.Set("Content-Security-Policy",
			"default-src 'self'; connect-src "+connect+"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
