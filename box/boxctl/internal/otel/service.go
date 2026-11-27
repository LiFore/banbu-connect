package otel

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultPort 不用 OTLP 的标准端口 4318：开发机上常有别的接收器（collector、open-agentcode）占着它。
const DefaultPort = 19182

const maxBody = 32 << 20

// Config 是遥测设置，存在数据目录的 otel.json。
type Config struct {
	// Enabled 默认开启：客户端启动即接收，托盘外壳据此自动配置 Claude Code。
	// 但用户没有明确选择过（Decided 为 false）、而这台电脑上已经配置过 Claude Code 的 OTel 时，不自动开启，
	// 只在首页提示（见 Suppressed）。
	Enabled bool `json:"enabled"`
	// Decided 表示用户在界面上明确开关过。明确开启时外壳会覆盖已有的 OTel 配置（备份原值，关闭时恢复）。
	Decided       bool `json:"decided"`
	Port          int  `json:"port"`
	RetentionDays int  `json:"retention_days"`
}

func defaultConfig() Config { return Config{Enabled: true, Port: DefaultPort, RetentionDays: 30} }

// Existing 是外壳发现的、不是本客户端写入的 OTel 配置。
type Existing struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Source：settings（用户的 settings.json）、managed（企业托管配置，优先级最高，不能覆盖）、env（环境变量）。
	Source string `json:"source"`
}

// ClaudeReport 是托盘外壳回报的 Claude Code 配置结果。
type ClaudeReport struct {
	Configured   bool       `json:"configured"`
	SettingsPath string     `json:"settings_path"`
	Existing     []Existing `json:"existing"`
	Error        string     `json:"error,omitempty"`
	At           time.Time  `json:"at"`
}

// Status 是接口返回的状态。Env 是外壳应写入 Claude Code settings.json 的 env；为空表示应移除之前写入的项。
type Status struct {
	Config Config `json:"config"`
	// Active 是实际生效的开关：Enabled 且没有被已有配置挡住。
	Active bool `json:"active"`
	// Suppressed：用户没有明确选择过，而这台电脑上已有 OTel 配置，所以没有自动开启（首页提示）。
	Suppressed bool `json:"suppressed"`
	// Force：用户明确开启，外壳覆盖 settings.json 里已有的 OTel 配置。
	Force     bool              `json:"force"`
	Listening bool              `json:"listening"`
	Endpoint  string            `json:"endpoint"`
	Error     string            `json:"error,omitempty"`
	Env       map[string]string `json:"env"`
	// ManualEnv 是手动配置时要写进 settings.json env 的内容（不论是否开启都给出）。
	ManualEnv map[string]string `json:"manual_env"`
	Counts    Counts            `json:"counts"`
	Claude    *ClaudeReport     `json:"claude,omitempty"`
	Upload    UploadStatus      `json:"upload"`
}

// UploadStatus 是上报到管理服务的情况。
type UploadStatus struct {
	Bound     bool       `json:"bound"`
	Pending   int64      `json:"pending"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// Uploader 把一批数据发给管理服务；没有绑定时返回 false、不报错。
type Uploader func(ctx context.Context, b Batch) (bound bool, err error)

const (
	uploadFirst = time.Minute
	uploadEvery = 5 * time.Minute
	// 一次上报最多发这么多批，剩下的下一轮再发。
	uploadMaxBatches = 50
)

// ClaudeEnv 是写入 Claude Code settings.json env 的遥测配置，指向本机接收器；数据会上报到管理服务。
// 不收集提示词和助手回复：不开 OTEL_LOG_USER_PROMPTS、OTEL_LOG_ASSISTANT_RESPONSES（入库时也会再删一次，见 scrub）；
// 也不开 OTEL_LOG_RAW_API_BODIES（完整请求/响应体，含对话内容）。
func ClaudeEnv(port int) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_METRICS_EXPORTER":               "otlp",
		"OTEL_LOGS_EXPORTER":                  "otlp",
		"OTEL_TRACES_EXPORTER":                "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/json",
		"OTEL_EXPORTER_OTLP_ENDPOINT":         fmt.Sprintf("http://127.0.0.1:%d", port),
		// 工具参数与错误详情、工具输出（span 事件）
		"OTEL_LOG_TOOL_DETAILS": "1",
		"OTEL_LOG_TOOL_CONTENT": "1",
		// 指标附加属性：版本、入口、仓库
		"OTEL_METRICS_INCLUDE_VERSION":    "true",
		"OTEL_METRICS_INCLUDE_ENTRYPOINT": "true",
		"OTEL_METRICS_INCLUDE_REPOSITORY": "true",
	}
}

// Service 管理接收器的生命周期、设置和定期清理。
type Service struct {
	store *Store
	path  string
	log   *slog.Logger

	mu       sync.Mutex
	cfg      Config
	srv      *http.Server
	listenOK bool
	err      string
	claude   *ClaudeReport
	baseCtx  context.Context

	syncReq int64 // 页面上点「立即写入配置」的次数，外壳看到变化就立即核对

	uploader Uploader
	upMu     sync.Mutex // 同一时间只有一次上报
	upload   UploadStatus
}

// SetUploader 设置上报方式；Run 之前调用。
func (s *Service) SetUploader(u Uploader) { s.uploader = u }

// Open 打开遥测库和设置。
func Open(dataDir string, log *slog.Logger) (*Service, error) {
	store, err := OpenStore(filepath.Join(dataDir, "otel.db"))
	if err != nil {
		return nil, err
	}
	s := &Service{store: store, path: filepath.Join(dataDir, "otel.json"), log: log, cfg: defaultConfig()}
	if b, err := os.ReadFile(s.path); err == nil {
		cfg := defaultConfig()
		if json.Unmarshal(b, &cfg) == nil {
			s.cfg = normalize(cfg)
		}
	}
	return s, nil
}

func normalize(c Config) Config {
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = DefaultPort
	}
	if c.RetentionDays < 0 {
		c.RetentionDays = 0
	}
	return c
}

// Store 返回遥测库（测试用）。
func (s *Service) Store() *Store { return s.store }

// Run 按设置启动接收器并定期清理过期数据，直到 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()
	s.apply()
	s.prune(ctx)
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	up := time.NewTimer(uploadFirst)
	defer up.Stop()
	for {
		select {
		case <-up.C:
			if err := s.Upload(ctx); err != nil {
				s.log.Info("上报遥测失败", "err", err)
			}
			up.Reset(uploadEvery)
		case <-ctx.Done():
			s.mu.Lock()
			s.stopLocked()
			s.mu.Unlock()
			_ = s.store.Close()
			return
		case <-t.C:
			s.prune(ctx)
		}
	}
}

// Upload 把还没上报的数据分批发给管理服务，每批成功后记下进度。关闭遥测或没有绑定时什么也不做。
func (s *Service) Upload(ctx context.Context) error {
	if s.uploader == nil || !s.Active() {
		return nil
	}
	s.upMu.Lock()
	defer s.upMu.Unlock()
	err := s.uploadBatches(ctx)
	s.mu.Lock()
	if err != nil {
		s.upload.LastError = err.Error()
	} else {
		s.upload.LastError = ""
	}
	s.mu.Unlock()
	return err
}

func (s *Service) uploadBatches(ctx context.Context) error {
	for range uploadMaxBatches {
		b, next, err := s.store.Pending(ctx)
		if err != nil {
			return err
		}
		if b.Len() == 0 {
			return nil
		}
		bound, err := s.uploader(ctx, b)
		s.mu.Lock()
		s.upload.Bound = bound
		s.mu.Unlock()
		if err != nil || !bound {
			return err
		}
		if err := s.store.Ack(ctx, next); err != nil {
			return err
		}
		now := time.Now()
		s.mu.Lock()
		s.upload.LastAt = &now
		s.mu.Unlock()
	}
	return nil
}

func (s *Service) prune(ctx context.Context) {
	if n, err := s.store.Prune(ctx, s.Config().RetentionDays); err != nil {
		s.log.Warn("清理过期遥测失败", "err", err)
	} else if n > 0 {
		s.log.Info("已清理过期遥测", "rows", n)
	}
}

// Config 返回当前设置。
func (s *Service) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// SetConfig 保存设置并按需重启接收器。
func (s *Service) SetConfig(c Config) (Config, error) {
	c = normalize(c)
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return s.Config(), err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return s.Config(), err
	}
	s.mu.Lock()
	was := s.activeLocked()
	changed := c.Port != s.cfg.Port
	s.cfg = c
	changed = changed || was != s.activeLocked()
	s.mu.Unlock()
	if changed {
		s.apply()
	}
	return c, nil
}

// Active 是实际生效的开关。
func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked()
}

func (s *Service) activeLocked() bool { return s.cfg.Enabled && !s.suppressedLocked() }

// suppressedLocked：用户没明确选择过，且外壳发现了已有的 OTel 配置。外壳还没回报时按未发现处理：
// 这期间外壳自己也会先检查，发现已有配置就不写（见 client/app 的 claude_otel.rs）。
func (s *Service) suppressedLocked() bool {
	return s.cfg.Enabled && !s.cfg.Decided && s.claude != nil && len(s.claude.Existing) > 0
}

// Brief 是给托盘外壳的：应写入 Claude Code 的 env（为空表示移除之前写入的），以及是否覆盖已有配置。
func (s *Service) Brief() (env map[string]string, force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeLocked() {
		return map[string]string{}, false
	}
	return ClaudeEnv(s.cfg.Port), s.cfg.Decided
}

// apply 按设置启停接收器。Run 之前调用时什么都不做。
func (s *Service) apply() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseCtx == nil {
		return
	}
	s.stopLocked()
	if !s.activeLocked() {
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.err = fmt.Sprintf("端口 %d 无法监听（%v），请在设置中更换端口", s.cfg.Port, err)
		s.log.Warn("遥测接收器启动失败", "addr", addr, "err", err)
		return
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.srv, s.listenOK, s.err = srv, true, ""
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.mu.Lock()
			if s.srv == srv {
				s.listenOK, s.err = false, err.Error()
			}
			s.mu.Unlock()
		}
	}()
	s.log.Info("遥测接收器已启动", "addr", addr)
}

func (s *Service) stopLocked() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	s.srv, s.listenOK, s.err = nil, false, ""
}

// Status 返回状态；counts 读库失败时为零值。
func (s *Service) Status(ctx context.Context) Status {
	counts, _ := s.store.Counts(ctx)
	pending, _ := s.store.PendingCount(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Config: s.cfg, Listening: s.listenOK, Error: s.err, Counts: counts, Claude: s.claude, Env: map[string]string{}, Upload: s.upload}
	st.Upload.Pending = pending
	st.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port)
	st.Active, st.Suppressed = s.activeLocked(), s.suppressedLocked()
	st.ManualEnv = ClaudeEnv(s.cfg.Port)
	if st.Active {
		st.Env = ClaudeEnv(s.cfg.Port)
		st.Force = s.cfg.Decided
	}
	return st
}

// RequestSync 让外壳立即重新核对、写入 Claude Code 配置。
func (s *Service) RequestSync() {
	s.mu.Lock()
	s.syncReq++
	s.mu.Unlock()
}

// SyncRequest 是给外壳的同步请求计数。
func (s *Service) SyncRequest() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncReq
}

// ReportClaude 记下托盘外壳配置 Claude Code 的结果。
func (s *Service) ReportClaude(r ClaudeReport) {
	r.At = time.Now()
	if r.Existing == nil {
		r.Existing = []Existing{}
	}
	s.mu.Lock()
	was := s.activeLocked()
	s.claude = &r
	changed := was != s.activeLocked()
	s.mu.Unlock()
	if changed {
		s.apply()
	}
}

var signalPaths = map[string]Signal{"/v1/traces": Traces, "/v1/logs": Logs, "/v1/metrics": Metrics}

// Handler 是 OTLP/HTTP 接收器：只接受 http/json（protobuf 返回 415），支持 gzip。
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(code int, msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			if msg == "" {
				_, _ = io.WriteString(w, "{}")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
		// 防 DNS 重绑定：只接受本机地址。
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
			reply(http.StatusForbidden, "只允许通过本机地址访问")
			return
		}
		signal, ok := signalPaths[r.URL.Path]
		if r.Method != http.MethodPost || !ok {
			reply(http.StatusNotFound, "not found")
			return
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "json") {
			reply(http.StatusUnsupportedMediaType, "只支持 http/json，请设置 OTEL_EXPORTER_OTLP_PROTOCOL=http/json")
			return
		}
		var body io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			gz, err := gzip.NewReader(body)
			if err != nil {
				reply(http.StatusBadRequest, "gzip 解压失败")
				return
			}
			defer gz.Close()
			body = io.LimitReader(gz, maxBody+1)
		}
		raw, err := io.ReadAll(body)
		if err != nil || len(raw) > maxBody {
			reply(http.StatusRequestEntityTooLarge, "请求体超过 32 MB")
			return
		}
		if _, _, err := s.store.Ingest(r.Context(), signal, raw); err != nil {
			var re *RequestError
			if errors.As(err, &re) {
				reply(http.StatusBadRequest, err.Error())
				return
			}
			s.log.Warn("写入遥测失败", "signal", signal, "err", err)
			reply(http.StatusInternalServerError, err.Error())
			return
		}
		reply(http.StatusOK, "")
	})
}

// Routes 注册管理接口：/api/v1/desktop/otel/...
func (s *Service) Routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, writeJSON func(http.ResponseWriter, int, any)) {
	type apiErr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	mux.Handle("GET /api/v1/desktop/otel/status", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Status(r.Context()))
	}))
	mux.Handle("PUT /api/v1/desktop/otel/config", authed(func(w http.ResponseWriter, r *http.Request) {
		var patch struct {
			Enabled       *bool `json:"enabled"`
			Port          *int  `json:"port"`
			RetentionDays *int  `json:"retention_days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		in := s.Config()
		if patch.Enabled != nil {
			// 在界面上开关过就算明确选择：之后不再因为已有配置自动关闭。
			in.Enabled, in.Decided = *patch.Enabled, true
		}
		if patch.Port != nil {
			in.Port = *patch.Port
		}
		if patch.RetentionDays != nil {
			in.RetentionDays = *patch.RetentionDays
		}
		if in.Port != 0 && (in.Port < 1024 || in.Port > 65535) {
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", "端口须在 1024–65535 之间"})
			return
		}
		if _, err := s.SetConfig(in); err != nil {
			writeJSON(w, http.StatusInternalServerError, apiErr{"failed", err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.Status(r.Context()))
	}))
	mux.Handle("GET /api/v1/desktop/otel/summary", authed(func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 || days > 366 {
			days = 7
		}
		sum, err := s.store.Summarize(r.Context(), 0, days)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiErr{"failed", err.Error()})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, sum)
	}))
	mux.Handle("POST /api/v1/desktop/otel/upload", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Upload(r.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.Status(r.Context()))
	}))
	// 页面上点「立即写入配置」：外壳下一次读状态（3 秒内）就核对并写入，结果经 /claude 回报。
	mux.Handle("POST /api/v1/desktop/otel/claude/sync", authed(func(w http.ResponseWriter, r *http.Request) {
		s.RequestSync()
		writeJSON(w, http.StatusOK, s.Status(r.Context()))
	}))
	// 托盘外壳回报 Claude Code 配置结果。
	mux.Handle("POST /api/v1/desktop/otel/claude", authed(func(w http.ResponseWriter, r *http.Request) {
		var in ClaudeReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		s.ReportClaude(in)
		writeJSON(w, http.StatusOK, s.Status(r.Context()))
	}))
}
