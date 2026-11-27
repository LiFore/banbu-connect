package dist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"boxctl/internal/gen"
)

func TestRuleSetsSettingAndProfile(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d, _ := s.Draft(ctx)
	if d.RuleSets != nil {
		t.Fatal("unset rule sets should not be published (clients use defaults)")
	}
	var ie *InputError
	if err := s.SetRuleSets(ctx, []gen.RuleSet{{Name: "x", Behavior: "bad", URL: "https://e/x", Target: "DIRECT"}}); !errors.As(err, &ie) {
		t.Fatalf("want input error: %v", err)
	}
	list := []gen.RuleSet{{Type: gen.TypeRuleSet, Name: "ai", Behavior: "classical", URL: "https://e/ai.yaml", Target: "Claude"}}
	if err := s.SetRuleSets(ctx, list); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Draft(ctx)
	if len(d.RuleSets) != 1 || d.RuleSets[0].Type != "" {
		t.Fatalf("draft: %+v", d.RuleSets)
	}
	// 空列表要原样下发，不能被当成「没配置」。
	if err := s.SetRuleSets(ctx, []gen.RuleSet{}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Draft(ctx)
	b, _ := json.Marshal(d)
	var back Profile
	_ = json.Unmarshal(b, &back)
	if back.RuleSets == nil || len(back.RuleSets) != 0 {
		t.Fatalf("empty list lost in transit: %s", b)
	}
	if m := Merge(back, nil, nil, nil); m.RuleSets == nil {
		t.Fatal("merge should keep the empty list")
	}
	// 恢复默认。
	if err := s.SetRuleSets(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Draft(ctx); d.RuleSets != nil {
		t.Fatal("reset should clear")
	}
}

func TestPreviewRuleSet(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.yaml":
			_, _ = w.Write([]byte("payload:\n  - DOMAIN-SUFFIX,claude.ai\n  - DOMAIN-SUFFIX,anthropic.com\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer src.Close()
	s := newServer(t)
	p, err := s.PreviewRuleSet(context.Background(), PreviewQuery{URL: src.URL + "/ok.yaml"})
	if err != nil || p.Count != 2 || p.Items[0].Value != "claude.ai" || p.Kinds["suffix"] != 2 {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if _, err := s.PreviewRuleSet(context.Background(), PreviewQuery{URL: src.URL + "/missing"}); err == nil {
		t.Fatal("404 should fail")
	}
	var ie *InputError
	if _, err := s.PreviewRuleSet(context.Background(), PreviewQuery{URL: "file:///etc/passwd"}); !errors.As(err, &ie) {
		t.Fatalf("non-http should be rejected: %v", err)
	}
}

// 规则集缓存：第二次读缓存，源站挂了也能用旧缓存；统计只读缓存时没缓存的标记 missing。
func TestRuleSetCache(t *testing.T) {
	hits := 0
	up := true
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !up {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("payload:\n  - DOMAIN,a.com\n  - DOMAIN-SUFFIX,b.com\n  - IP-CIDR,1.1.1.0/24\n  - PROCESS-NAME,x\n"))
	}))
	defer src.Close()
	ctx := context.Background()
	s := newServer(t)
	u := src.URL + "/c.yaml"
	sets := []gen.RuleSet{{Name: "c", Behavior: "classical", URL: u, Target: "DIRECT"}}
	if st := s.RuleSetStats(ctx, sets, false, false); len(st) != 1 || !st[0].Missing {
		t.Fatalf("stats before fetch = %+v", st)
	}
	p, err := s.PreviewRuleSet(ctx, PreviewQuery{URL: u, Behavior: "classical", Kind: "domains"})
	if err != nil || p.Cached || p.Count != 4 || p.Matched != 2 || p.Kinds[kindIP] != 1 || p.Kinds[kindOther] != 1 {
		t.Fatalf("preview = %+v, err = %v", p, err)
	}
	if p, _ = s.PreviewRuleSet(ctx, PreviewQuery{URL: u, Q: "b.com"}); !p.Cached || hits != 1 || p.Matched != 1 {
		t.Fatalf("second preview should use cache: %+v hits=%d", p, hits)
	}
	up = false
	if p, err = s.PreviewRuleSet(ctx, PreviewQuery{URL: u, Refresh: true}); err != nil || p.Warning == "" || p.Count != 4 {
		t.Fatalf("refresh failure should fall back to cache: %+v %v", p, err)
	}
	st := s.RuleSetStats(ctx, sets, false, false)
	if len(st) != 1 || st[0].Count != 4 || !slices.Equal(st[0].Sample, []string{"a.com", "b.com"}) {
		t.Fatalf("stats = %+v", st)
	}
}
