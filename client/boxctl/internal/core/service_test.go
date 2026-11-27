package core

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// 独立服务：面板启动时接管已经在运行的内核（不重启它），内核离线、换了进程都能发现。
func TestServiceAdoptAndOffline(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() || r.URL.Path != "/version" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"meta":true,"version":"v1.19.33"}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := []byte("listeners:\n- name: probe-0\n  type: http\n  port: 19300\n  proxy: Claude\n- name: gate-direct\n  port: 19390\n  proxy: DIRECT\n")
	if err := os.WriteFile(filepath.Join(dir, configFile), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "core.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	var applied atomic.Int32
	s := &Supervisor{log: slog.New(slog.DiscardHandler), secret: "s", coreDir: dir, controller: strings.TrimPrefix(srv.URL, "http://"),
		hooks: Hooks{OnApplied: func(context.Context, bool) { applied.Add(1) }}}
	s.UseService(ServiceOptions{Init: "/nonexistent", PIDFile: pidFile, LogFile: filepath.Join(dir, "core.log")})
	ctx := context.Background()

	if !s.checkService(ctx) {
		t.Fatal("内核应在线")
	}
	h := s.Health()
	if !s.Status().Running || h.Managed != "service" || h.PID != os.Getpid() || h.Version != "v1.19.33" {
		t.Fatalf("接管：%+v", h)
	}
	if string(s.AppliedConfig()) != string(cfg) {
		t.Fatal("接管时应把磁盘上的配置当作正在运行的配置，生成结果相同时就不用重载")
	}
	if _, probes := s.GatePorts(); probes["Claude"] != 19300 || len(probes) != 1 {
		t.Fatalf("探测入口应从配置里读出：%v", probes)
	}
	if h.NeedsRestart != (h.Version != MihomoVersion()) {
		t.Fatal("版本不同时应提示重启内核")
	}

	down.Store(true)
	if s.checkService(ctx) {
		t.Fatal("内核应离线")
	}
	st, h := s.Status(), s.Health()
	if st.Running || !strings.Contains(st.LastError, "内核离线") || h.Restarts != 1 || h.OfflineAt.IsZero() {
		t.Fatalf("离线：%+v %+v", st, h)
	}
	down.Store(false)
	s.checkService(ctx)
	if !s.Status().Running || s.Status().LastError != "" {
		t.Fatalf("恢复后应清掉离线提示：%+v", s.Status())
	}
	// 用户自己停的：不算意外离线。
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	down.Store(true)
	s.checkService(ctx)
	if s.Health().Restarts != 1 {
		t.Fatal("用户停止不算意外离线")
	}
}
