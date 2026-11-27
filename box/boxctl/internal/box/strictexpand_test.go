package box

import (
	"context"
	"errors"
	"slices"
	"testing"

	"boxctl/internal/core"
	"boxctl/internal/gen"
	"boxctl/internal/guard"
)

func TestExpanderMergesAndKeepsLastGood(t *testing.T) {
	ctx := context.Background()
	src := []core.StrictRuleSet{
		{RuleSet: gen.RuleSet{Name: "claude", Behavior: "classical", Target: "Claude"}, URL: "https://x/claude.yaml"},
		{RuleSet: gen.RuleSet{Name: "openai", Behavior: "domain", Target: "ChatGPT"}, URL: "https://x/openai.yaml"},
	}
	bodies := map[string]string{
		"https://x/claude.yaml": "payload:\n  - DOMAIN-SUFFIX,claude.ai\n  - DOMAIN-KEYWORD,claude\n  - IP-CIDR,160.79.104.0/23,no-resolve\n",
		"https://x/openai.yaml": "payload:\n  - '+.openai.com'\n  - '+.chatgpt.com'\n",
	}
	dir := t.TempDir()
	e := newExpander(dir, func(context.Context) ([]core.StrictRuleSet, error) { return src, nil }, func(context.Context) string { return "" })
	e.fetch = func(_ context.Context, u, _ string) ([]byte, error) {
		b, ok := bodies[u]
		if !ok {
			return nil, errors.New("404")
		}
		return []byte(b), nil
	}
	if !e.Refresh(ctx, false) {
		t.Fatal("首次展开应报告变化")
	}
	p := guard.Policy{Suffixes: []string{"claude.ai"}}
	e.Merge(&p)
	if !slices.Equal(p.Suffixes, []string{"claude.ai", "openai.com", "chatgpt.com"}) || !slices.Equal(p.CIDRs, []string{"160.79.104.0/23"}) {
		t.Fatalf("merge = %+v", p)
	}
	if _, err := p.Normalize(); err != nil {
		t.Fatalf("合并后的策略应能通过 guard 校验：%v", err)
	}
	r := e.Result()
	if r.RuleSets[0].Skipped != 1 || r.RuleSets[0].Group != "Claude" {
		t.Fatalf("result = %+v", r.RuleSets[0])
	}

	// 未到刷新时间、列表没变：不重新下载。
	if e.Refresh(ctx, false) {
		t.Fatal("没有变化")
	}
	// 下载失败：保留上一次的结果并记录错误。
	delete(bodies, "https://x/openai.yaml")
	if e.Refresh(ctx, true) {
		t.Fatal("失败时保留结果，条目不变")
	}
	r = e.Result()
	if r.RuleSets[1].Error == "" || !slices.Equal(r.RuleSets[1].Suffixes, []string{"openai.com", "chatgpt.com"}) {
		t.Fatalf("应保留上一次成功的结果：%+v", r.RuleSets[1])
	}

	// 重启后从磁盘恢复。
	e2 := newExpander(dir, e.sources, e.proxy)
	if len(e2.Result().RuleSets) != 2 {
		t.Fatal("应从磁盘恢复展开结果")
	}

	// 规则集不再指向严格组：移出保护范围。
	src = src[:1]
	if !e.Refresh(ctx, false) {
		t.Fatal("列表变化应重新展开")
	}
	p = guard.Policy{}
	e.Merge(&p)
	if slices.Contains(p.Suffixes, "openai.com") {
		t.Fatal("移除的规则集不应再合并")
	}
}
