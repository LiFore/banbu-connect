package dist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"boxctl/internal/gen"
)

func TestSimulateRules(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload:\n  - '+.ruleset-only.example'\n"))
	}))
	defer src.Close()
	s := newServer(t)
	seed(t, s)
	ctx := context.Background()
	if err := s.SetRules(ctx, []string{"#@segment:custom", "DOMAIN-SUFFIX,example.com,DIRECT", "#@segment:fallback", "MATCH,Claude"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRuleSets(ctx, []gen.RuleSet{{Name: "openai", Behavior: "domain", URL: src.URL + "/o.yaml", Target: "Claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSelections(ctx, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"www.example.com":        {"DOMAIN-SUFFIX,example.com,DIRECT", "下发规则"},
		"claude.ai":              {"DOMAIN-SUFFIX,claude.ai,Claude", "受保护服务「Claude」（客户端默认）"},
		"a.ruleset-only.example": {"RULE-SET,openai,Claude", "远程规则集"},
		"unknown.org":            {"MATCH,Claude", "兜底（下发）"},
	}
	for q, want := range cases {
		res, err := s.SimulateRules(ctx, SimulateRequest{Query: q, Port: 443})
		if err != nil {
			t.Fatal(err)
		}
		if res.Match == nil || res.Match.Rule != want[0] || res.Match.Source != want[1] {
			t.Errorf("%s：%+v", q, res.Match)
		}
	}
	if _, err := s.SimulateRules(ctx, SimulateRequest{Query: "a b"}); err == nil {
		t.Fatal("非法输入应报错")
	}
}
