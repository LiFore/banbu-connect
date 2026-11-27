package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

func TestCheckProtection(t *testing.T) {
	cfg := killswitch.Config{Services: []killswitch.Service{
		{Name: "Claude", Enabled: true, Group: "Claude", Suffixes: []string{"anthropic.com", "claude.ai"}},
		{Name: "ChatGPT", Enabled: true, Group: "OpenAI", Suffixes: []string{".openai.com"}},
	}}
	ok := killswitch.Status{Enabled: true, State: killswitch.StateProtecting}
	strict := trust.StrictStatus{Groups: []trust.StrictGroup{{Group: "Claude", Allowed: true}, {Group: "OpenAI", Allowed: false, Reason: "出口 1.2.3.4 不在允许列表里"}}}

	if err := checkProtection(ok, cfg, strict, "claude"); err != nil {
		t.Fatalf("claude ok: %v", err)
	}
	var pe *ProtectionError
	if err := checkProtection(ok, cfg, strict, "codex"); !errors.As(err, &pe) || pe.Fix != "/advanced/egress" || !strings.Contains(pe.Reason, "OpenAI") {
		t.Fatalf("codex strict = %v", err)
	}
	if err := checkProtection(killswitch.Status{}, cfg, strict, "claude"); err == nil {
		t.Fatal("protection off should block")
	}
	if err := checkProtection(killswitch.Status{Enabled: true, State: killswitch.StateBlocking, Verdict: killswitch.Verdict{Reason: "内核没有运行"}}, cfg, strict, "claude"); err == nil || err.Error() != "内核没有运行" {
		t.Fatalf("blocking = %v", err)
	}
	// 受信网络上直连放行：出口由路由盒负责，不看本机的严格组。
	if err := checkProtection(killswitch.Status{Enabled: true, State: killswitch.StateDirect}, cfg, strict, "codex"); err != nil {
		t.Fatalf("direct = %v", err)
	}
	if err := checkProtection(ok, killswitch.Config{}, strict, "claude"); err == nil {
		t.Fatal("no protected service should block")
	}
}

func TestWorkspaceActions(t *testing.T) {
	w := newWorkspaces(nil, nil, nil) // 没有保护服务引擎：不检查
	if _, err := w.Enqueue(WorkspaceAction{Action: "login", Kind: "claude", Email: "a@x.com"}); err == nil {
		t.Fatal("claude login goes through one-click login")
	}
	if _, err := w.Enqueue(WorkspaceAction{Action: "app_login", Kind: "claude", Email: "a@x.com"}); err == nil {
		t.Fatal("ChatGPT app login is codex only")
	}
	if _, err := w.Enqueue(WorkspaceAction{Action: "desktop_app", Kind: "codex"}); err == nil {
		t.Fatal("desktop app is claude only")
	}
	// Claude Desktop 用它自己的登录：不需要账号，传了也清掉。
	if d, err := w.Enqueue(WorkspaceAction{Action: "desktop_app", Kind: "claude", Email: "a@x.com", Cwd: "/tmp"}); err != nil || d.Email != "" || d.Cwd != "" {
		t.Fatalf("desktop app = %+v %v", d, err)
	}
	w.actions = nil
	if _, err := w.Enqueue(WorkspaceAction{Action: "rm -rf", Kind: "codex", Email: "a@x.com"}); err == nil {
		t.Fatal("unknown action")
	}
	a, err := w.Enqueue(WorkspaceAction{Action: "login", Kind: "codex", Email: "a@x.com"})
	if err != nil || a.Status != "pending" {
		t.Fatalf("enqueue = %+v %v", a, err)
	}
	// 同一个操作没做完时不重复排队。
	if b, _ := w.Enqueue(WorkspaceAction{Action: "login", Kind: "codex", Email: "a@x.com"}); b.ID != a.ID {
		t.Fatal("duplicate should return the existing action")
	}
	if w.Pending() != a.ID {
		t.Fatal("pending")
	}
	if _, err := w.Claim(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Claim(a.ID); err == nil || w.Pending() != "" {
		t.Fatal("claim twice")
	}
	_ = w.Finish(a.ID, false, "1455 端口被占用")
	if got := w.Actions()[0]; got.Status != "failed" || got.Error != "1455 端口被占用" {
		t.Fatalf("finished = %+v", got)
	}
	// 外壳没认领：过期后标为失败。
	b, _ := w.Enqueue(WorkspaceAction{Action: "terminal", Kind: "claude"})
	w.mu.Lock()
	for _, x := range w.actions {
		if x.ID == b.ID {
			x.CreatedAt = time.Now().Add(-3 * time.Minute)
		}
	}
	w.mu.Unlock()
	if w.Pending() != "" || !strings.Contains(w.Actions()[0].Error, "托盘") {
		t.Fatalf("stale = %+v", w.Actions())
	}
}

func TestAIQuota(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /claude", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-beta") == "" || r.Header.Get("Authorization") != "Bearer ca" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":23.5,"resets_at":"2026-11-23T15:00:00Z"},"seven_day":{"utilization":60,"resets_at":"2026-11-27T00:00:00Z"},"seven_day_opus":null,"extra_usage":{"is_enabled":false}}`))
	})
	mux.HandleFunc("GET /codex", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-Id") != "acc" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1795000000},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	old := aiQuotaEndpoints
	aiQuotaEndpoints.Claude, aiQuotaEndpoints.Codex = ts.URL+"/claude", ts.URL+"/codex"
	defer func() { aiQuotaEndpoints = old }()

	q := newAIQuota(nil)
	ctx := context.Background()
	c := q.fetch(ctx, WorkspaceToken{Kind: "claude", Email: "a", Token: "ca"})
	if c.Error != "" || len(c.Windows) != 2 || c.Windows[0].Label != "5 小时" || c.Windows[0].UsedPercent != 23.5 || c.Windows[1].Label != "每周" {
		t.Fatalf("claude = %+v", c)
	}
	x := q.fetch(ctx, WorkspaceToken{Kind: "codex", Email: "b", Token: "t", AccountID: "acc"})
	if x.Error != "" || x.Plan != "plus" || len(x.Windows) != 2 || x.Windows[0].Label != "5 小时" || x.Windows[1].Label != "每周" ||
		x.Windows[0].ResetAt == "" || x.Windows[1].ResetAt == "" {
		t.Fatalf("codex = %+v", x)
	}
	// 令牌过期：提示在终端里刷新，不自己刷新。
	if e := q.fetch(ctx, WorkspaceToken{Kind: "claude", Token: "old"}); !strings.Contains(e.Error, "运行一次 claude") {
		t.Fatalf("expired = %+v", e)
	}
	// 外壳报来新令牌后立即查；去掉的账号清除结果。
	q.SetTokens([]WorkspaceToken{{Kind: "claude", Email: "a", Token: "ca"}})
	runCtx, cancel := context.WithCancel(ctx)
	go q.Run(runCtx)
	deadline := time.Now().Add(3 * time.Second)
	for q.Get("claude", "a") == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if r := q.Get("claude", "a"); r == nil || len(r.Windows) != 2 {
		t.Fatalf("run = %+v", r)
	}
	q.SetTokens(nil)
	if q.Get("claude", "a") != nil {
		t.Fatal("removed token should drop the result")
	}
}
