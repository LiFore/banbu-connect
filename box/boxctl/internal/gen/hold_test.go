package gen

import (
	"slices"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	_ "github.com/metacubex/mihomo/hub/executor" // 提供 config 包 linkname 的实现
	"go.yaml.in/yaml/v3"
)

func TestWithTarget(t *testing.T) {
	for in, want := range map[string]string{
		"DOMAIN-SUFFIX,claude.ai,Claude":                       "DOMAIN-SUFFIX,claude.ai,X",
		"IP-CIDR,160.79.104.0/23,Claude,no-resolve":            "IP-CIDR,160.79.104.0/23,X,no-resolve",
		"AND,((DOMAIN-SUFFIX,claude.ai),(NETWORK,tcp)),Claude": "AND,((DOMAIN-SUFFIX,claude.ai),(NETWORK,tcp)),X",
		"SUB-RULE,(SRC-IP-CIDR,192.168.1.2/32),dev1":           "SUB-RULE,(SRC-IP-CIDR,192.168.1.2/32),X",
		"MATCH,Claude":           "MATCH,X",
		"RULE-SET,claude,Claude": "RULE-SET,claude,X",
		"AND,((IP-CIDR,1.2.3.0/24,no-resolve),(NETWORK,tcp)),Claude": "AND,((IP-CIDR,1.2.3.0/24,no-resolve),(NETWORK,tcp)),X",
	} {
		if got := withTarget(in, "X"); got != want {
			t.Errorf("withTarget(%q) = %q，应为 %q", in, got, want)
		}
		if ruleTargetOf(withTarget(in, "X")) != "X" {
			t.Errorf("换目标后 ruleTargetOf 应为 X：%q", in)
		}
	}
}

// 路由盒：每条指向严格组的规则前有一条同条件、指向拒绝出站的副本，其余规则不变；内核能加载这份配置。
func TestHoldRules(t *testing.T) {
	in := input()
	in.Router, in.HoldRules, in.ProbePortBase = true, true, 19300
	in.DeviceRules = []string{"SUB-RULE,(SRC-IP-CIDR,192.168.1.2/32),dev1"}
	in.SubRules = map[string][]string{
		"dev1": {"DOMAIN,example.com,DIRECT", "DOMAIN-SUFFIX,claude.ai,Claude"},
		"dev2": {"DOMAIN,example.org,DIRECT"},
	}
	in.CustomRules = []string{"DOMAIN,api.claude.ai,DIRECT", "IP-CIDR,160.79.104.0/23,Claude,no-resolve"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	rules := rulesOf(t, in)
	plain := input()
	plain.Router, plain.ProbePortBase = true, 19300
	plain.DeviceRules, plain.SubRules, plain.CustomRules = in.DeviceRules, in.SubRules, in.CustomRules
	base := rulesOf(t, plain)

	// 去掉副本后与不加副本时完全相同；每个副本紧挨在原规则前，条件相同。
	var stripped []string
	for i, rule := range rules {
		tgt := ruleTargetOf(rule)
		if !IsHoldTarget(tgt) {
			stripped = append(stripped, rule)
			continue
		}
		if i+1 >= len(rules) {
			t.Fatalf("副本后面没有原规则：%s", rule)
		}
		next := rules[i+1]
		orig := ruleTargetOf(next)
		if withTarget(next, tgt) != rule {
			t.Fatalf("副本 %q 与原规则 %q 条件不同", rule, next)
		}
		if tgt == HoldProxyName && orig != "Claude" {
			t.Fatalf("副本只应加在指向严格组的规则前：%q", next)
		}
	}
	if !slices.Equal(stripped, base) {
		t.Fatalf("去掉副本后规则变了：\n%v\n%v", stripped, base)
	}
	for _, want := range []string{
		"SUB-RULE,(SRC-IP-CIDR,192.168.1.2/32)," + HoldProxyName + "·dev1",
		"DOMAIN-SUFFIX,claude.ai," + HoldProxyName,
		"IP-CIDR,160.79.104.0/23," + HoldProxyName + ",no-resolve",
		"RULE-SET,claude," + HoldProxyName,
	} {
		if !slices.Contains(rules, want) {
			t.Fatalf("缺少副本 %q：%v", want, rules)
		}
	}
	if slices.Contains(rules, "DOMAIN,api.claude.ai,"+HoldProxyName) {
		t.Fatal("指向 DIRECT 的规则不应有副本")
	}

	cfg := parse(t, r.YAML)
	sub := cfg["sub-rules"].(map[string]any)
	hs, ok := sub[HoldProxyName+"·dev1"].([]any)
	if !ok || len(hs) != 2 || hs[0] != "DOMAIN,example.com,DIRECT" || hs[1] != "DOMAIN-SUFFIX,claude.ai,"+HoldProxyName {
		t.Fatalf("子规则副本：%v", sub)
	}
	if _, ok := sub[HoldProxyName+"·dev2"]; ok {
		t.Fatal("没有严格目标的子规则不应有副本")
	}

	// 内核能解析：拒绝出站存在，副本规则在顶层（可以按索引开关）。
	// 去掉 DNS 和 GEOIP、GEOSITE 规则：它们会让内核下载地理数据库，测试不访问网络。
	C.SetHomeDir(t.TempDir())
	delete(cfg, "dns")
	var kept []any
	for _, x := range cfg["rules"].([]any) {
		if s := x.(string); !strings.HasPrefix(s, "GEOIP,") && !strings.HasPrefix(s, "GEOSITE,") {
			kept = append(kept, s)
		}
	}
	cfg["rules"] = kept
	offline, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse(offline)
	if err != nil {
		t.Fatalf("内核解析失败：%v", err)
	}
	if p, ok := parsed.Proxies[HoldProxyName]; !ok || p.Type() != C.Reject {
		t.Fatalf("拒绝出站：%v", parsed.Proxies[HoldProxyName])
	}
	n := 0
	for _, rule := range parsed.Rules {
		if IsHoldTarget(rule.Adapter()) {
			if _, ok := rule.(C.RuleWrapper); !ok {
				t.Fatal("顶层规则应能单独开关")
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("内核里没有副本规则")
	}
}

func TestNoHoldRulesByDefault(t *testing.T) {
	in := input()
	for _, r := range rulesOf(t, in) {
		if strings.Contains(r, HoldProxyName) {
			t.Fatalf("没开 HoldRules 时不应有副本：%s", r)
		}
	}
}

// mihomoParse 去掉需要联网下载数据库的部分后交给内核解析（测试不访问网络）。
func mihomoParse(t *testing.T, yml []byte) *config.Config {
	t.Helper()
	var cfg map[string]any
	if err := yaml.Unmarshal(yml, &cfg); err != nil {
		t.Fatal(err)
	}
	C.SetHomeDir(t.TempDir())
	delete(cfg, "dns")
	delete(cfg, "rule-providers")
	var kept []any
	for _, x := range cfg["rules"].([]any) {
		if s := x.(string); !strings.HasPrefix(s, "GEOIP,") && !strings.HasPrefix(s, "GEOSITE,") && !strings.HasPrefix(s, "RULE-SET,") {
			kept = append(kept, s)
		}
	}
	cfg["rules"] = kept
	offline, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse(offline)
	if err != nil {
		t.Fatalf("内核解析失败：%v", err)
	}
	return parsed
}

// 内核能解析路由盒的客户端代理入口（带 users 的 mixed 监听）和桌面的「路由盒」出站（ADR-084）。
func TestBoxProxyParsesInMihomo(t *testing.T) {
	in := input()
	in.ClientProxy = &ProxyEndpoint{Server: "127.0.0.1", Port: 19444, User: "banbu", Password: "pw"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mihomoParse(t, r.YAML).Listeners["box-clients"]; !ok {
		t.Fatal("内核里没有 box-clients 监听")
	}

	in = input()
	in.ProbePortBase = 19200
	in.Gate = &GateInput{Port: 19180, Secret: "s3cret", DirectPort: 19181}
	in.Box = &ProxyEndpoint{Server: "192.168.10.100", Port: 19444, User: "banbu", Password: "pw"}
	if r, err = Build(in); err != nil {
		t.Fatal(err)
	}
	if p, ok := mihomoParse(t, r.YAML).Proxies[BoxProxyName]; !ok || p.Type() != C.Socks5 {
		t.Fatalf("路由盒出站：%v", p)
	}
}
