package killswitch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

const maxBody = 1 << 20

// ConfigView 是 API 上的配置：控制接口和密钥由程序内部维护，不经过 API。
type ConfigView struct {
	Enabled   bool      `json:"enabled"`
	Services  []Service `json:"services"`
	DNSListen string    `json:"dns_listen"`
}

// Routes 注册 API。wrap 负责认证。
func (e *Engine) Routes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/v1/killswitch/status", wrap(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, e.Status())
	}))
	mux.Handle("GET /api/v1/killswitch/config", wrap(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, viewOf(e.Config()))
	}))
	mux.Handle("PUT /api/v1/killswitch/config", wrap(func(w http.ResponseWriter, r *http.Request) {
		var in ConfigView
		if !decode(w, r, &in) {
			return
		}
		next := e.Config()
		next.Services, next.DNSListen = in.Services, in.DNSListen
		if err := e.UpdateConfig(r.Context(), next); err != nil {
			writeOpErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, viewOf(e.Config()))
	}))
	mux.Handle("POST /api/v1/killswitch/enable", wrap(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Force bool `json:"force"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := e.Enable(r.Context(), in.Force); err != nil {
			writeOpErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, e.Status())
	}))
	mux.Handle("POST /api/v1/killswitch/disable", wrap(func(w http.ResponseWriter, r *http.Request) {
		if err := e.Disable(r.Context()); err != nil {
			writeOpErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, e.Status())
	}))
}

func viewOf(c Config) ConfigView {
	return ConfigView{Enabled: c.Enabled, Services: c.Services, DNSListen: c.DNSListen}
}

// SetController 设置内部使用的 Mihomo 控制接口，用于校验运行中的规则。
func (e *Engine) SetController(url, secret string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.Controller, e.cfg.ControllerSecret = url, secret
}

// RefreshRuleChecks 在内核加载新配置后立即重新校验，否则要等下一轮检查才更新结果。
func (e *Engine) RefreshRuleChecks(ctx context.Context) {
	e.setRuleChecks(checkRules(ctx, e.config()))
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求内容无效")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

func writeOpErr(w http.ResponseWriter, err error) {
	var notReady *NotReadyError
	var invalid *ValidationError
	switch {
	case errors.As(err, &notReady):
		writeErr(w, http.StatusConflict, "not_ready", notReady.Reason)
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, "invalid", invalid.Msg)
	default:
		writeErr(w, http.StatusInternalServerError, "failed", err.Error())
	}
}
