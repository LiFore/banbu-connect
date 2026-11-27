package core

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"boxctl/internal/dist"
	"boxctl/internal/gen"
	"boxctl/internal/trust"
)

// Authed 包装需要登录的接口。
type Authed func(http.HandlerFunc) http.Handler

// APIErr 是接口错误的统一格式。
type APIErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON 写 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Fail 按错误类型写响应：内核没运行时 409。
func Fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrCoreDown) {
		WriteJSON(w, http.StatusConflict, APIErr{"core_down", err.Error()})
		return
	}
	WriteJSON(w, http.StatusInternalServerError, APIErr{"failed", err.Error()})
}

// Forward 把请求转发给内核控制接口，原样返回。
func Forward(w http.ResponseWriter, r *http.Request, s *Supervisor, path string, body []byte) {
	code, out, err := s.CallCore(r.Context(), r.Method, path, body)
	if err != nil {
		Fail(w, err)
		return
	}
	// 内核拒绝说明控制接口不是我们的内核（端口被其他程序占用）或密钥不对。
	// 不能原样返回 401：前端会当成登录过期退出。
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		WriteJSON(w, http.StatusBadGateway, APIErr{"core_denied", "内核控制接口拒绝访问：端口可能被其他程序占用，见内核日志"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

// AutoApply 在数据变化后触发重新生成配置：这就是“自动同步规则”。
// 配置没变时生成结果逐字节相同，不会重启或重载内核。
func AutoApply(next http.Handler, s *Supervisor, prefixes ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method == http.MethodGet || r.Method == http.MethodHead || rec.status >= 400 {
			return
		}
		for _, p := range prefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				s.Trigger()
				return
			}
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush 让转发的流式响应（下载更新包）继续可用。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 让 WebSocket 升级（路由盒连接页 /api/v1/box/live）穿过本包装：
// coder/websocket 直接断言 http.Hijacker，不认 Unwrap。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("http.ResponseWriter does not implement http.Hijacker")
	}
	r.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}

// Unwrap 供 http.ResponseController 找到底层 ResponseWriter。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Routes 挂内核启停、线路和严格线路检查接口，路径为 /api/v1/{prefix}/...。
// onStrict 在严格线路结果变化时调用（桌面：重新评估 Kill Switch；路由盒：更新 guard 的阻断）。
func Routes(mux *http.ServeMux, authed Authed, prefix string, s *Supervisor, strict *trust.StrictChecker, onStrict func()) {
	base := "/api/v1/" + prefix
	// 立即经每个严格组探测出口：启用 Kill Switch 前先让用户看到结果，启用时的判断也用这次结果。
	mux.Handle("POST "+base+"/strict/check", authed(func(w http.ResponseWriter, r *http.Request) {
		if strict.Check(r.Context()) && onStrict != nil {
			onStrict()
		}
		WriteJSON(w, http.StatusOK, strict.Status())
	}))
	mux.Handle("POST "+base+"/start", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := s.SetRunning(r.Context(), true); err != nil {
			WriteJSON(w, http.StatusBadRequest, APIErr{"apply_failed", err.Error()})
			return
		}
		WriteJSON(w, http.StatusOK, s.Status())
	}))
	mux.Handle("POST "+base+"/stop", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := s.SetRunning(r.Context(), false); err != nil {
			Fail(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, s.Status())
	}))
	mux.Handle("GET "+base+"/proxies", authed(func(w http.ResponseWriter, r *http.Request) {
		Forward(w, r, s, "/proxies", nil)
	}))
	mux.Handle("PUT "+base+"/proxies/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			WriteJSON(w, http.StatusBadRequest, APIErr{"bad_request", "请求内容无效"})
			return
		}
		body, _ := json.Marshal(in)
		Forward(w, r, s, ProxyPath(r.PathValue("name")), body)
		// 被切换的可能是严格组本身，也可能是它引用的组：一律重新确认严格线路的出口。
		s.LinesChanged()
	}))
	mux.Handle("GET "+base+"/proxies/{name}/delay", authed(func(w http.ResponseWriter, r *http.Request) {
		q := url.Values{"url": {"http://www.gstatic.com/generate_204"}, "timeout": {"5000"}}
		Forward(w, r, s, ProxyPath(r.PathValue("name"))+"/delay?"+q.Encode(), nil)
	}))
}

// SettingsPatch 是设置接口接受的字段；平台不支持的字段由 Allow 过滤。
type SettingsPatch struct {
	Mode        *string   `json:"mode"`
	TUN         *bool     `json:"tun"`
	MixedPort   *int      `json:"mixed_port"`
	CustomRules *[]string `json:"custom_rules"`
	DNSGuard    *bool     `json:"dns_guard"`
	Onboarded   *bool     `json:"onboarded"`
	Mirror      *string   `json:"mirror"`
	AllowLAN    *bool     `json:"allow_lan"`
}

// SettingsRoutes 挂设置的读写接口。desktopOnly 为 false 时忽略 TUN、DNS 防泄漏（路由盒始终开 TUN）；
// 为 true 时忽略局域网代理（只用于路由盒）。
func SettingsRoutes(mux *http.ServeMux, authed Authed, prefix string, s *Supervisor, desktopOnly bool) {
	base := "/api/v1/" + prefix + "/settings"
	mux.Handle("GET "+base, authed(func(w http.ResponseWriter, r *http.Request) {
		st, err := s.Settings().Load(r.Context())
		if err != nil {
			Fail(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}))
	mux.Handle("PUT "+base, authed(func(w http.ResponseWriter, r *http.Request) {
		var in SettingsPatch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			WriteJSON(w, http.StatusBadRequest, APIErr{"bad_request", "请求内容无效"})
			return
		}
		st, err := s.Settings().Load(r.Context())
		if err != nil {
			Fail(w, err)
			return
		}
		if in.Mode != nil {
			if *in.Mode != "rule" && *in.Mode != "global" && *in.Mode != "direct" {
				WriteJSON(w, http.StatusBadRequest, APIErr{"invalid", "模式须为 rule、global 或 direct"})
				return
			}
			st.Mode = *in.Mode
		}
		if in.MixedPort != nil {
			if *in.MixedPort < 1024 || *in.MixedPort > 65535 {
				WriteJSON(w, http.StatusBadRequest, APIErr{"invalid", "端口须在 1024–65535"})
				return
			}
			st.MixedPort = *in.MixedPort
		}
		if in.CustomRules != nil {
			st.CustomRules = *in.CustomRules
		}
		if in.Onboarded != nil {
			st.Onboarded = *in.Onboarded
		}
		if in.Mirror != nil {
			m, err := gen.ValidateMirror(*in.Mirror)
			if err != nil {
				WriteJSON(w, http.StatusBadRequest, APIErr{"invalid", err.Error()})
				return
			}
			st.Mirror = m
		}
		if desktopOnly {
			if in.TUN != nil {
				st.TUN = *in.TUN
			}
			if in.DNSGuard != nil {
				st.DNSGuard = *in.DNSGuard
			}
		} else if in.AllowLAN != nil {
			st.AllowLAN = *in.AllowLAN
		}
		if err := s.Settings().Save(r.Context(), st); err != nil {
			Fail(w, err)
			return
		}
		s.RefreshStatus(r.Context()) // 界面马上看到新值，内核稍后重载
		WriteJSON(w, http.StatusOK, st)
	}))
}

// Device 描述本机，用于接入申请。
type Device struct {
	Name      func() string
	OSVersion func() string
}

// ServerRoutes 挂绑定管理服务和接入申请的接口，路径为 /api/v1/{prefix}/server...。
func ServerRoutes(mux *http.ServeMux, authed Authed, prefix string, c *dist.Client, s *Supervisor, dev Device) {
	base := "/api/v1/" + prefix + "/server"
	fail := func(w http.ResponseWriter, err error) {
		var in *dist.InputError
		if errors.As(err, &in) {
			WriteJSON(w, http.StatusBadRequest, APIErr{"invalid", in.Msg})
			return
		}
		WriteJSON(w, http.StatusBadGateway, APIErr{"failed", err.Error()})
	}
	mux.Handle("GET "+base, authed(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, c.Status())
	}))
	mux.Handle("POST "+base+"/bind", authed(func(w http.ResponseWriter, r *http.Request) {
		var in dist.Binding
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			WriteJSON(w, http.StatusBadRequest, APIErr{"bad_request", "请求内容无效"})
			return
		}
		st, err := c.Bind(r.Context(), in)
		if err != nil {
			fail(w, err)
			return
		}
		_ = c.CancelEnrollment() // 手动绑定后，之前的申请没用了
		s.Trigger()
		WriteJSON(w, http.StatusOK, st)
	}))
	// 接入申请：提交设备信息，管理员授权后点「刷新授权」自动领取令牌并绑定。
	enrollView := func(e *dist.EnrollmentView) map[string]any {
		return map[string]any{"default_url": dist.DefaultServerURL, "default_name": dev.Name(), "enrollment": e}
	}
	mux.Handle("GET "+base+"/enroll", authed(func(w http.ResponseWriter, _ *http.Request) {
		e, err := c.Enrollment()
		if err != nil {
			fail(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, enrollView(e))
	}))
	mux.Handle("POST "+base+"/enroll", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL         string `json:"url"`
			Fingerprint string `json:"fingerprint"`
			Name        string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			WriteJSON(w, http.StatusBadRequest, APIErr{"bad_request", "请求内容无效"})
			return
		}
		if strings.TrimSpace(in.URL) == "" {
			in.URL = dist.DefaultServerURL
		}
		e, err := c.Enroll(r.Context(), in.URL, in.Fingerprint, dist.EnrollRequest{
			Name: in.Name, Platform: runtime.GOOS + "/" + runtime.GOARCH, OSVersion: dev.OSVersion(),
		})
		if err != nil {
			fail(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, enrollView(e))
	}))
	mux.Handle("POST "+base+"/enroll/refresh", authed(func(w http.ResponseWriter, r *http.Request) {
		e, bound, err := c.RefreshEnrollment(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if bound {
			s.Trigger()
		}
		out := enrollView(e)
		out["bound"] = bound
		out["server"] = c.Status()
		WriteJSON(w, http.StatusOK, out)
	}))
	mux.Handle("DELETE "+base+"/enroll", authed(func(w http.ResponseWriter, _ *http.Request) {
		if err := c.CancelEnrollment(); err != nil {
			fail(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, enrollView(nil))
	}))
	mux.Handle("POST "+base+"/unbind", authed(func(w http.ResponseWriter, _ *http.Request) {
		if err := c.Unbind(); err != nil {
			fail(w, err)
			return
		}
		s.Trigger()
		WriteJSON(w, http.StatusOK, c.Status())
	}))
	mux.Handle("POST "+base+"/sync", authed(func(w http.ResponseWriter, r *http.Request) {
		changed, err := c.Sync(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if changed {
			s.Trigger()
		}
		WriteJSON(w, http.StatusOK, c.Status())
	}))
}
