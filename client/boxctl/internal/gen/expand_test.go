package gen

import (
	"slices"
	"testing"
)

func TestExpandRuleSet(t *testing.T) {
	e := ExpandRuleSet([]string{
		"DOMAIN-SUFFIX,claude.ai", "DOMAIN,api.anthropic.com", "DOMAIN-KEYWORD,anthropic",
		"IP-CIDR,160.79.104.0/23,no-resolve", "IP-CIDR6,2607:6bc0::/48", "GEOSITE,openai", "# c",
	}, "classical")
	if !slices.Equal(e.Suffixes, []string{"claude.ai", "api.anthropic.com"}) {
		t.Fatalf("suffixes = %v", e.Suffixes)
	}
	if !slices.Equal(e.CIDRs, []string{"160.79.104.0/23", "2607:6bc0::/48"}) {
		t.Fatalf("cidrs = %v", e.CIDRs)
	}
	if e.Skipped != 2 || !slices.Equal(e.Samples, []string{"DOMAIN-KEYWORD,anthropic", "GEOSITE,openai"}) {
		t.Fatalf("skipped = %d %v", e.Skipped, e.Samples)
	}

	d := ExpandRuleSet([]string{"+.openai.com", ".chatgpt.com", "oaistatic.com", "*.bad", "localhost"}, "domain")
	if !slices.Equal(d.Suffixes, []string{"openai.com", "chatgpt.com", "oaistatic.com"}) || d.Skipped != 2 {
		t.Fatalf("domain = %+v", d)
	}
	ip := ExpandRuleSet([]string{"1.2.3.4", "10.0.0.1/8", "x"}, "ipcidr")
	if !slices.Equal(ip.CIDRs, []string{"1.2.3.4/32", "10.0.0.0/8"}) || ip.Skipped != 1 {
		t.Fatalf("ipcidr = %+v", ip)
	}
}
