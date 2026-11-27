package desktop

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"
)

// 信任 Bypass：打开后账号操作（登录、启动、切号、额度查询、刷新令牌、启动 Antigravity）不再检查保护状态，
// 保护服务在网络层的阻断照旧。打开要输入固定口令，只为防止误操作，不是安全边界；关闭不需要口令。
// 打开期间状态里带 trust_bypass，页面顶部一直显示红色提示。保存在数据目录，重启后保持。
const (
	trustBypassPassword = "Banbu20250325!"
	trustBypassFile     = "trust-bypass.json"
)

// TrustBypassBrief 是状态里给页面的摘要。
type TrustBypassBrief struct {
	Enabled bool      `json:"enabled"`
	Since   time.Time `json:"since,omitzero"`
}

type trustBypass struct {
	path   string
	record func(level, msg string)

	mu    sync.Mutex
	state TrustBypassBrief
}

func newTrustBypass(path string, record func(level, msg string)) *trustBypass {
	b := &trustBypass{path: path, record: record}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &b.state)
	}
	return b
}

// Enabled 为真时跳过账号操作的保护检查。b 为空时不跳过。
func (b *trustBypass) Enabled() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.Enabled
}

func (b *trustBypass) Brief() TrustBypassBrief {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

var errBypassPassword = errors.New("口令不正确")

// Set 打开（要口令）或关闭（不要口令）。
func (b *trustBypass) Set(on bool, password string) (TrustBypassBrief, error) {
	if on && password != trustBypassPassword {
		return b.Brief(), errBypassPassword
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Enabled == on {
		return b.state, nil
	}
	next := TrustBypassBrief{Enabled: on}
	if on {
		next.Since = time.Now()
	}
	raw, _ := json.Marshal(next)
	if b.path != "" {
		if err := os.WriteFile(b.path, raw, 0o600); err != nil {
			return b.state, err
		}
	}
	b.state = next
	if b.record != nil {
		if on {
			b.record("warn", "已打开信任 Bypass：账号操作不再检查保护状态")
		} else {
			b.record("info", "已关闭信任 Bypass")
		}
	}
	return b.state, nil
}

func bypassRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, b *trustBypass) {
	mux.Handle("GET /api/v1/desktop/trust-bypass", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.Brief())
	}))
	mux.Handle("PUT /api/v1/desktop/trust-bypass", authed(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled  bool   `json:"enabled"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		st, err := b.Set(req.Enabled, req.Password)
		switch {
		case errors.Is(err, errBypassPassword):
			writeJSON(w, http.StatusForbidden, apiErr{"password", err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, apiErr{"failed", "保存失败：" + err.Error()})
		default:
			writeJSON(w, http.StatusOK, st)
		}
	}))
}
