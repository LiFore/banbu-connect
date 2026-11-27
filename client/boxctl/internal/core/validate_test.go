package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseInProcess(t *testing.T) {
	base := `proxies:
  - {name: hk, type: ss, server: 198.51.100.1, port: 8388, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: Claude, type: select, proxies: [hk]}
rule-providers:
  claude: {type: http, behavior: classical, url: "https://example.com/c.yaml", path: ./c.yaml, interval: 86400}
rules:
  - AND,((DOMAIN-SUFFIX,claude.ai),(NETWORK,udp)),REJECT
  - DOMAIN-SUFFIX,claude.ai,Claude
  - RULE-SET,claude,Claude
  - GEOIP,CN,DIRECT,no-resolve
  - MATCH,DIRECT
`
	if err := parseInProcess([]byte(base)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"的目标「不存在」不存在": strings.Replace(base, "DOMAIN-SUFFIX,claude.ai,Claude", "DOMAIN-SUFFIX,claude.ai,不存在", 1),
		"成员「gone」不存在": strings.Replace(base, "proxies: [hk]", "proxies: [hk, gone]", 1),
		"规则集没有定义":     strings.Replace(base, "RULE-SET,claude,Claude", "RULE-SET,openai,Claude", 1),
		"MATCH":       strings.Replace(base, "  - MATCH,DIRECT\n", "", 1),
		"无法解析":        strings.Replace(base, "cipher: aes-128-gcm", "cipher: no-such-cipher", 1),
		"YAML":        "rules: [",
	}
	for want, cfg := range cases {
		err := parseInProcess([]byte(cfg))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：err = %v", want, err)
		}
	}
}

func TestRuleTarget(t *testing.T) {
	for rule, want := range map[string]string{
		"DOMAIN,a.com,G":                        "G",
		"IP-CIDR,1.0.0.0/8,DIRECT,no-resolve":   "DIRECT",
		"AND,((DOMAIN,a),(NETWORK,udp)),REJECT": "REJECT",
		"MATCH,漏网之鱼":                            "漏网之鱼",
		"DST-PORT,19302,REJECT":                 "REJECT",
	} {
		if got, _ := ruleTarget(rule); got != want {
			t.Errorf("%s → %q，期望 %q", rule, got, want)
		}
	}
}

// 生成器的黄金文件都应通过进程内检查：检查不能比内核更严格，否则正常配置会被拒。
func TestParseInProcessAcceptsGenerated(t *testing.T) {
	files, _ := filepath.Glob("../gen/testdata/golden/*.yaml")
	if len(files) == 0 {
		t.Fatal("没有黄金文件")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := parseInProcess(b); err != nil {
			t.Errorf("%s：%v", filepath.Base(f), err)
		}
	}
}
