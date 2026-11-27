package otel

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const logsBody = `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},
"scopeLogs":[{"logRecords":[
 {"timeUnixNano":"%d","body":{"stringValue":"claude_code.api_request"},"attributes":[
  {"key":"event.name","value":{"stringValue":"api_request"}},{"key":"session.id","value":{"stringValue":"s1"}},
  {"key":"user.email","value":{"stringValue":"a@x.com"}},
  {"key":"model","value":{"stringValue":"claude-opus-5-5"}},{"key":"cost_usd","value":{"stringValue":"0.25"}},
  {"key":"input_tokens","value":{"stringValue":"100"}},{"key":"output_tokens","value":{"intValue":"50"}},
  {"key":"duration_ms","value":{"stringValue":"1200"}},
  {"key":"workspace.host_paths","value":{"arrayValue":{"values":[{"stringValue":"/work/repo"}]}}}]},
 {"timeUnixNano":"%d","attributes":[{"key":"event.name","value":{"stringValue":"user_prompt"}},{"key":"session.id","value":{"stringValue":"s1"}}]},
 {"timeUnixNano":"%d","attributes":[{"key":"event.name","value":{"stringValue":"tool_result"}},{"key":"session.id","value":{"stringValue":"s1"}},{"key":"success","value":{"stringValue":"false"}}]}
]}]}]}`

const metricsBody = `{"resourceMetrics":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"s1"}}]},
"scopeMetrics":[{"metrics":[{"name":"claude_code.lines_of_code.count","sum":{"aggregationTemporality":1,"dataPoints":[
 {"timeUnixNano":"%d","asDouble":12,"attributes":[{"key":"type","value":{"stringValue":"added"}}]},
 {"timeUnixNano":"%d","asInt":"3","attributes":[{"key":"type","value":{"stringValue":"removed"}}]}]}}]}]}]}`

func fmtNanos(body string, t time.Time) string {
	return strings.ReplaceAll(body, `"%d"`, `"`+strconv.FormatInt(t.UnixNano(), 10)+`"`)
}

func newService(t *testing.T) *Service {
	t.Helper()
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Service{store: st, log: slog.New(slog.DiscardHandler), cfg: defaultConfig()}
}

func post(t *testing.T, h http.Handler, path, ctype string, body []byte, gz bool) *httptest.ResponseRecorder {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(body)
		_ = w.Close()
		body = buf.Bytes()
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:19182"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReceiverAndSummary(t *testing.T) {
	s := newService(t)
	h := s.Handler()
	now := time.Now()
	logs := []byte(fmtNanos(logsBody, now))
	if rec := post(t, h, "/v1/logs", "application/json", logs, false); rec.Code != 200 {
		t.Fatalf("logs: %d %s", rec.Code, rec.Body)
	}
	// 重发同一批（导出器重试）不重复计数。
	if rec := post(t, h, "/v1/logs", "application/json", logs, true); rec.Code != 200 {
		t.Fatalf("gzip logs: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, "/v1/metrics", "application/json", []byte(fmtNanos(metricsBody, now)), false); rec.Code != 200 {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, "/v1/traces", "application/x-protobuf", []byte{1, 2}, false); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("protobuf should be 415, got %d", rec.Code)
	}
	if rec := post(t, h, "/v1/logs", "application/json", []byte("{"), false); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json should be 400, got %d", rec.Code)
	}

	sum, err := s.store.Summarize(context.Background(), 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	tt := sum.Totals
	if tt.Requests != 1 || tt.CostUSD != 0.25 || tt.Tokens.Input != 100 || tt.Tokens.Output != 50 {
		t.Fatalf("totals: %+v", tt)
	}
	if tt.Prompts != 1 || tt.ToolCalls != 1 || tt.ToolFailures != 1 || tt.Sessions != 1 {
		t.Fatalf("counts: %+v", tt)
	}
	if tt.LinesAdded != 12 || tt.LinesRemoved != 3 {
		t.Fatalf("lines: %+v", tt)
	}
	if len(sum.Models) != 1 || sum.Models[0].Model != "claude-opus-5-5" || sum.Models[0].P50Ms != 1200 {
		t.Fatalf("models: %+v", sum.Models)
	}
	// 按账号：api_request 带 user.email，其他事件没有，单独一行（空邮箱）。
	if len(sum.Accounts) != 2 || sum.Accounts[0].Email != "a@x.com" || sum.Accounts[0].Requests != 1 || sum.Accounts[0].CostUSD != 0.25 {
		t.Fatalf("accounts: %+v", sum.Accounts)
	}
	if len(sum.Days) != 7 || sum.Days[6].Requests != 1 {
		t.Fatalf("days: %+v", sum.Days)
	}
	if len(sum.Sessions) != 1 || sum.Sessions[0].Cwd != "/work/repo" || sum.Sessions[0].Prompts != 1 {
		t.Fatalf("sessions: %+v", sum.Sessions)
	}
}

func TestReceiverRejectsForeignHost(t *testing.T) {
	s := newService(t)
	req := httptest.NewRequest(http.MethodPost, "http://evil.example:19182/v1/logs", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestPrune(t *testing.T) {
	s := newService(t)
	old := time.Now().AddDate(0, 0, -40)
	if rec := post(t, s.Handler(), "/v1/logs", "application/json", []byte(fmtNanos(logsBody, old)), false); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	n, err := s.store.Prune(context.Background(), 30)
	if err != nil || n == 0 {
		t.Fatalf("prune: %d %v", n, err)
	}
	c, _ := s.store.Counts(context.Background())
	if c.Logs != 0 || c.Sessions != 0 {
		t.Fatalf("counts after prune: %+v", c)
	}
}

func TestServiceListensAndConfig(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 用随机端口，避免和本机其他进程冲突。
	if _, err := s.SetConfig(Config{Enabled: true, Port: freePort(t), RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	go s.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for !s.Status(ctx).Listening && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	st := s.Status(ctx)
	if !st.Listening || st.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] != st.Endpoint {
		t.Fatalf("status: %+v", st)
	}
	resp, err := http.Post(st.Endpoint+"/v1/metrics", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	if _, err := s.SetConfig(Config{Enabled: false, Port: st.Config.Port}); err != nil {
		t.Fatal(err)
	}
	st = s.Status(ctx)
	if st.Listening || len(st.Env) != 0 {
		t.Fatalf("disabled status: %+v", st)
	}
	// 设置写盘，重新打开后保留。
	again, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if again.Config().Enabled {
		t.Fatal("config not persisted")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestPromptsAndResponsesAreNotStored(t *testing.T) {
	s := newService(t)
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[
	 {"timeUnixNano":"1","attributes":[{"key":"event.name","value":{"stringValue":"user_prompt"}},
	  {"key":"prompt","value":{"stringValue":"秘密提示词"}},{"key":"prompt_length","value":{"intValue":"5"}}]},
	 {"timeUnixNano":"2","attributes":[{"key":"event.name","value":{"stringValue":"assistant_response"}},
	  {"key":"response","value":{"stringValue":"秘密回复"}}]}]}]}]}`
	if rec := post(t, s.Handler(), "/v1/logs", "application/json", []byte(body), false); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	var n int
	if err := s.store.db.QueryRow(`select count(*) from otel_logs where attrs like '%秘密%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("content stored: %d %v", n, err)
	}
	if err := s.store.db.QueryRow(`select count(*) from otel_logs where json_extract(attrs, '$.prompt_length') = 5`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("length should be kept: %d %v", n, err)
	}
}

func TestUploadRoundTrip(t *testing.T) {
	client := newService(t)
	server, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	now := time.Now()
	post(t, client.Handler(), "/v1/logs", "application/json", []byte(fmtNanos(logsBody, now)), false)
	post(t, client.Handler(), "/v1/metrics", "application/json", []byte(fmtNanos(metricsBody, now)), false)

	calls := 0
	client.SetUploader(func(ctx context.Context, b Batch) (bool, error) {
		calls++
		// 客户端发来的数据里即使带了原文，服务端也要删掉。
		b.Logs[0].Attrs["prompt"] = "x"
		_, err := server.Import(ctx, 7, b)
		return true, err
	})
	ctx := context.Background()
	if err := client.Upload(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.store.PendingCount(ctx); n != 0 {
		t.Fatalf("pending after upload: %d", n)
	}
	// 没有新数据时不再发。
	if err := client.Upload(ctx); err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	sum, err := server.Summarize(ctx, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.CostUSD != 0.25 || sum.Totals.LinesAdded != 12 || len(sum.Sessions) != 1 {
		t.Fatalf("server summary: %+v", sum.Totals)
	}
	var n int
	_ = server.db.QueryRow(`select count(*) from otel_logs where json_extract(attrs, '$.prompt') is not null`).Scan(&n)
	if n != 0 {
		t.Fatal("server kept prompt")
	}
	devices, _ := server.Devices(ctx, 7)
	if len(devices) != 1 || devices[0].DeviceID != 7 || devices[0].Requests != 1 {
		t.Fatalf("devices: %+v", devices)
	}

	// 未绑定：不确认进度，数据留着下次发。
	post(t, client.Handler(), "/v1/logs", "application/json", []byte(fmtNanos(logsBody, now.Add(time.Second))), false)
	client.SetUploader(func(context.Context, Batch) (bool, error) { return false, nil })
	_ = client.Upload(ctx)
	if n, _ := client.store.PendingCount(ctx); n == 0 {
		t.Fatal("unbound upload should keep pending rows")
	}
}

func TestExistingConfigSuppressesUntilDecided(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := s.Brief(); len(env) == 0 {
		t.Fatal("enabled by default when nothing is configured")
	}
	s.ReportClaude(ClaudeReport{Existing: []Existing{{Key: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "https://corp", Source: "settings"}}})
	st := s.Status(context.Background())
	if st.Active || !st.Suppressed || len(st.Env) != 0 {
		t.Fatalf("should be suppressed: %+v", st)
	}
	// 用户在首页点「开启」：明确选择，覆盖已有配置。
	if _, err := s.SetConfig(Config{Enabled: true, Decided: true, Port: DefaultPort, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	env, force := s.Brief()
	if len(env) == 0 || !force || s.Status(context.Background()).Suppressed {
		t.Fatalf("decided enable: env=%d force=%v", len(env), force)
	}
}
