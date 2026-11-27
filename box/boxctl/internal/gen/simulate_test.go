package gen

import (
	"net/netip"
	"testing"
)

func TestSimulate(t *testing.T) {
	rules := []SimRule{
		{Rule: "AND,((DOMAIN-SUFFIX,claude.ai),(NETWORK,udp)),REJECT", Source: "受保护服务"},
		{Rule: "GEOSITE,category-ads,REJECT", Source: "下发规则"},
		{Rule: "DOMAIN-SUFFIX,claude.ai,Claude", Source: "受保护服务"},
		{Rule: "RULE-SET,openai,ChatGPT", Source: "远程规则集"},
		{Rule: "IP-CIDR,10.0.0.0/8,DIRECT,no-resolve", Source: "下发规则"},
		{Rule: "GEOIP,LAN,DIRECT,no-resolve", Source: "远程规则集"},
		{Rule: "OR,((DST-PORT,22),(NOT,((NETWORK,tcp)))),DIRECT", Source: "下发规则"},
		{Rule: "MATCH,漏网之鱼", Source: "兜底"},
	}
	sets := map[string]RuleSetContent{"openai": {Behavior: "classical", Lines: []string{"DOMAIN-SUFFIX,openai.com", "DOMAIN-KEYWORD,chatgpt"}}}

	check := func(input string, port int, network string, wantIndex int, wantTarget string, wantUncertain int) {
		t.Helper()
		q, err := ParseQuery(input, port, network)
		if err != nil {
			t.Fatal(err)
		}
		r := Simulate(q, rules, sets)
		if r.Match == nil || r.Match.Index != wantIndex || r.Match.Target != wantTarget {
			t.Fatalf("%s：match = %+v", input, r.Match)
		}
		if len(r.Uncertain) != wantUncertain {
			t.Fatalf("%s：uncertain = %+v", input, r.Uncertain)
		}
	}
	check("https://claude.ai/new", 443, "tcp", 3, "Claude", 1) // GEOSITE 不确定
	check("claude.ai", 443, "udp", 1, "REJECT", 0)
	check("chatgpt.com", 443, "tcp", 4, "ChatGPT", 1)
	check("10.1.2.3", 443, "tcp", 5, "DIRECT", 0) // IP 查询没有域名，GEOSITE 不命中
	check("192.168.1.1", 80, "tcp", 6, "DIRECT", 0)
	check("example.org", 22, "tcp", 7, "DIRECT", 1)
	check("example.org", 443, "udp", 7, "DIRECT", 1) // NOT tcp
	check("example.org", 443, "tcp", 8, "漏网之鱼", 1)

	if _, err := ParseQuery("a b", 0, ""); err == nil {
		t.Fatal("非法输入")
	}
	if !ipSuffix(mustAddr("10.9.8.7"), mustPrefix("1.1.8.7/16")) || ipSuffix(mustAddr("10.9.8.7"), mustPrefix("1.1.8.6/16")) {
		t.Fatal("IP-SUFFIX")
	}
}

func mustAddr(s string) netip.Addr     { return netip.MustParseAddr(s) }
func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }
