package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"boxctl/internal/dist"
)

// 保护服务没有放通时，额度查询一个请求都不发，并记下暂停原因；放通后恢复查询。
func TestAIQuotaPausedWhenGateClosed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":12,"resets_at":"2026-11-23T10:00:00Z"}}`))
	}))
	defer srv.Close()
	old := aiQuotaEndpoints
	aiQuotaEndpoints.Claude = srv.URL
	defer func() { aiQuotaEndpoints = old }()

	var open atomic.Bool
	q := newAIQuota(func(string) (string, error) {
		if !open.Load() {
			return "", errors.New("保护服务没有开启")
		}
		return "", nil
	})
	tok := WorkspaceToken{Kind: dist.KindClaude, Email: "a@example.com", Token: "t"}

	r, status := q.fetchStatus(context.Background(), tok)
	if status != 0 || !strings.Contains(r.Error, "现在不能查询额度") {
		t.Fatalf("关闭时应拒绝：%d %q", status, r.Error)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.SetTokens([]WorkspaceToken{tok})
	go q.Run(ctx)
	waitFor(t, func() bool { return q.Paused(dist.KindClaude) != "" })
	if hits.Load() != 0 || q.Get(dist.KindClaude, tok.Email) != nil {
		t.Fatalf("关闭时不应发请求：hits=%d", hits.Load())
	}

	open.Store(true)
	q.SetTokens([]WorkspaceToken{{Kind: tok.Kind, Email: tok.Email, Token: "t2"}}) // 触发立即重查
	waitFor(t, func() bool { return q.Get(dist.KindClaude, tok.Email) != nil })
	if q.Paused(dist.KindClaude) != "" || hits.Load() == 0 {
		t.Fatalf("放通后应恢复查询：paused=%q hits=%d", q.Paused(dist.KindClaude), hits.Load())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("等待超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
