package gen

import (
	"slices"
	"strings"
	"testing"
)

func TestCustomRuleSets(t *testing.T) {
	in := input()
	in.RuleSets = []RuleSet{
		{Name: "ai", Behavior: "classical", Format: "text", URL: "https://example.com/ai.list", Target: "Claude"},
		{Name: "off", Behavior: "domain", URL: "https://example.com/off.yaml", Target: "DIRECT", Disabled: true},
		{Name: "gone", Behavior: "domain", URL: "https://example.com/gone.yaml", Target: "不存在的组"},
		{Type: TypeGeoIP, Name: "CN", Target: "DIRECT", NoResolve: true},
	}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parse(t, r.YAML)
	rules := rulesOf(t, in)
	ai, geo := slices.Index(rules, "RULE-SET,ai,Claude"), slices.Index(rules, "GEOIP,CN,DIRECT,no-resolve")
	if ai < 0 || geo < ai || rules[len(rules)-1] != "MATCH,漏网之鱼" {
		t.Fatalf("order: %v", rules)
	}
	for _, r := range rules {
		if strings.Contains(r, "RULE-SET,off") || strings.Contains(r, "RULE-SET,gone") || strings.Contains(r, "RULE-SET,claude") {
			t.Fatalf("should be skipped (disabled, missing target, or not in list): %s", r)
		}
	}
	providers := cfg["rule-providers"].(map[string]any)
	if len(providers) != 1 {
		t.Fatalf("only used providers should be generated: %v", providers)
	}
	p := providers["ai"].(map[string]any)
	if p["format"] != "text" || p["path"] != "./ruleset/ai.list" || p["proxy"] != "代理模式" {
		t.Fatalf("provider: %v", p)
	}

	// 空列表（管理员删光了）不回落到默认。
	in.RuleSets = []RuleSet{}
	for _, r := range rulesOf(t, in) {
		if strings.HasPrefix(r, "RULE-SET,") {
			t.Fatalf("empty list should have no rule sets: %s", r)
		}
	}
}

func TestDefaultRuleSetsAreValid(t *testing.T) {
	if err := ValidateRuleSets(DefaultRuleSets()); err != nil {
		t.Fatal(err)
	}
	bad := [][]RuleSet{
		{{Name: "a b", Behavior: "domain", URL: "https://x/a", Target: "DIRECT"}},
		{{Name: "a", Behavior: "domain", URL: "https://x/a", Target: "DIRECT"}, {Name: "a", Behavior: "domain", URL: "https://x/b", Target: "DIRECT"}},
		{{Name: "a", Behavior: "nope", URL: "https://x/a", Target: "DIRECT"}},
		{{Name: "a", Behavior: "domain", URL: "file:///etc/passwd", Target: "DIRECT"}},
		{{Name: "a", Behavior: "classical", Format: "mrs", URL: "https://x/a", Target: "DIRECT"}},
		{{Name: "a", Behavior: "domain", URL: "https://x/a", Target: ""}},
	}
	for i, list := range bad {
		if ValidateRuleSets(list) == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestParseRuleSet(t *testing.T) {
	got, err := ParseRuleSet([]byte("payload:\n  - '+.example.com'\n  - 'DOMAIN,a.com'\n"), "")
	if err != nil || !slices.Equal(got, []string{"+.example.com", "DOMAIN,a.com"}) {
		t.Fatalf("yaml: %v %v", got, err)
	}
	got, err = ParseRuleSet([]byte("# comment\nDOMAIN-SUFFIX,a.com\n\n// x\nIP-CIDR,1.1.1.0/24\n"), "text")
	if err != nil || len(got) != 2 {
		t.Fatalf("text: %v %v", got, err)
	}
	// 写成 yaml 但其实是纯文本的，也能读。
	if got, err = ParseRuleSet([]byte("DOMAIN-SUFFIX,a.com\n"), ""); err != nil || len(got) != 1 {
		t.Fatalf("plain as yaml: %v %v", got, err)
	}
	if _, err := ParseRuleSet([]byte{0, 1}, "mrs"); err == nil {
		t.Fatal("mrs should not be previewable")
	}
}
