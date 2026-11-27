package box

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"boxctl/internal/guard"
	"boxctl/internal/ipc"
	"boxctl/internal/panel"
)

func setup(t *testing.T) (*Box, http.Handler, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// unix socket 路径有长度限制，不用 t.TempDir()。
	dir, err := os.MkdirTemp("", "bx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "g.sock")
	log := slog.New(slog.DiscardHandler)
	go func() {
		_ = guard.Run(ctx, guard.Options{Socket: sock, DataDir: dir, LANDev: "br-lan", Fake: true, Logger: log})
	}()

	svc, err := panel.OpenServices(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.DB.Close() })
	b := New(ctx, Options{DataDir: dir, GuardSocket: sock, Logger: log, FetchRuleSet: func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("测试中不访问网络")
	}})
	admin, _ := b.Extend(svc)
	mux := http.NewServeMux()
	admin(mux, func(fn http.HandlerFunc) http.Handler { return fn })
	return b, b.Wrap(mux), sock
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func guardStatus(t *testing.T, sock string) guard.Status {
	t.Helper()
	var st guard.Status
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := ipc.Call(context.Background(), sock, guard.MethodStatus, nil, &st)
		if err == nil || time.Now().After(deadline) {
			if err != nil {
				t.Fatal(err)
			}
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestKillSwitchConfigAndGuardPolicy(t *testing.T) {
	b, h, sock := setup(t)
	ctx := context.Background()

	w := call(t, h, "GET", "/api/v1/box/killswitch/config", "")
	var cfg KillSwitch
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil || !cfg.Enabled || len(cfg.Services) == 0 {
		t.Fatalf("路由盒默认开启保护：%s", w.Body)
	}

	p, err := b.Policy(ctx)
	if err != nil || !slices.Contains(p.Suffixes, "claude.ai") || p.Hold || p.ForwardAll {
		t.Fatalf("内核没运行时：受保护域名要在策略里，不转发全部 DNS：%+v %v", p, err)
	}

	if w := call(t, h, "PUT", "/api/v1/box/killswitch/config", `{"services":[{"name":"X","enabled":true,"group":"Claude","suffixes":["bad domain"]}]}`); w.Code != 400 {
		t.Fatalf("无效后缀应拒绝：%d %s", w.Code, w.Body)
	}

	// 关闭保护：guard 收到空策略。
	if w := call(t, h, "POST", "/api/v1/box/killswitch/disable", ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if st := guardStatus(t, sock); st.Domains != 0 || st.State != guard.StateHealthy {
		t.Fatalf("%+v", st)
	}
	if w := call(t, h, "POST", "/api/v1/box/killswitch/enable", ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if st := guardStatus(t, sock); st.Domains == 0 || st.CIDRs == 0 {
		t.Fatalf("启用后 guard 应拿到受保护服务：%+v", st)
	}

	w = call(t, h, "GET", "/api/v1/box/status", "")
	var sum Summary
	if err := json.Unmarshal(w.Body.Bytes(), &sum); err != nil || sum.Guard == nil || sum.Core.Running {
		t.Fatalf("%s", w.Body)
	}
}

func TestSettingsIgnoreDesktopOnlyFields(t *testing.T) {
	_, h, _ := setup(t)
	w := call(t, h, "PUT", "/api/v1/box/settings", `{"mode":"global","tun":false,"dns_guard":true}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var st struct {
		Mode string `json:"mode"`
		TUN  bool   `json:"tun"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Mode != "global" || !st.TUN {
		t.Fatalf("路由盒始终开 TUN：%s", w.Body)
	}
	if w := call(t, h, "GET", "/api/v1/box/proxies", ""); w.Code != http.StatusConflict {
		t.Fatalf("内核没运行时线路接口应返回 409：%d", w.Code)
	}
}
