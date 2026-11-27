package guard

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultipleLANDevices(t *testing.T) {
	p, _ := policy().Normalize()
	p.Hold = true
	s := Ruleset(p, "br-lan, br-guest,br-lan,bad name;")
	for _, want := range []string{
		`dns_redirect iifname { "br-lan", "br-guest" } meta l4proto { tcp, udp } th dport 53`,
		`hold iifname { "br-lan", "br-guest" } ip daddr @strict_v4 counter drop`,
		`hold iifname { "br-lan", "br-guest" } ip6 daddr @strict_v6_net counter drop`,
		`encrypted_dns iifname { "br-lan", "br-guest" } tcp dport 853 counter drop`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q：\n%s", want, s)
		}
	}
	if strings.Contains(s, "bad") {
		t.Fatal("非法网卡名不应写进规则")
	}
}

func TestEncryptedDNSBlockedOnlyWithProtection(t *testing.T) {
	p, _ := policy().Normalize()
	s := Ruleset(p, "br-lan")
	for _, want := range []string{
		`encrypted_dns iifname "br-lan" tcp dport 853 counter drop`,
		`encrypted_dns iifname "br-lan" udp dport { 853, 8853 } counter drop`,
		`encrypted_dns iifname "br-lan" ip daddr @doh_v4_net meta l4proto { tcp, udp } th dport 443 counter drop`,
		"add element inet proxy_guard doh_v4_net { 1.0.0.1,",
		"type filter hook prerouting priority raw - 2",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q", want)
		}
	}
	conf := DnsmasqConf(p)
	for _, want := range []string{"local=/dns.google/", "local=/use-application-dns.net/", "local=/mask.icloud.com/"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("dnsmasq 缺少 %q：\n%s", want, conf)
		}
	}
	// 没有受保护的服务（Kill Switch 关闭）：不改用户的加密 DNS，但链要在（清空旧规则）。
	empty, _ := Policy{}.Normalize()
	s = Ruleset(empty, "br-lan")
	if strings.Contains(s, "add rule inet proxy_guard encrypted_dns") || !strings.Contains(s, "flush chain inet proxy_guard encrypted_dns") {
		t.Fatalf("关闭保护时不应拦加密 DNS：\n%s", s)
	}
	if strings.Contains(DnsmasqConf(empty), "local=/") {
		t.Fatal("关闭保护时 dnsmasq 不应改写 DoH 域名")
	}
}

func TestParseSetJSONAndRestore(t *testing.T) {
	out := `{"nftables": [{"metainfo": {"json_schema_version": 1}}, {"set": {"family": "inet", "name": "strict_v4", "table": "proxy_guard",
		"type": "ipv4_addr", "flags": ["timeout"], "timeout": 21600,
		"elem": [{"elem": {"val": "160.79.104.10", "timeout": 21600, "expires": 2000}}, "1.2.3.4"]}}]}`
	list, err := parseSetJSON([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != (SetElem{"160.79.104.10", 2000}) || list[1].Addr != "1.2.3.4" {
		t.Fatalf("%+v", list)
	}
	script := RestoreScript(map[string][]SetElem{
		"strict_v4": append(list, SetElem{"2001:db8::1", 10}, SetElem{"not-an-ip", 1}),
		"strict_v6": {{"2001:db8::1", 999999}, {"9.9.9.9", 5}},
	})
	for _, want := range []string{
		"add element inet proxy_guard strict_v4 { 160.79.104.10 timeout 2000s, 1.2.3.4 timeout 21600s }",
		"add element inet proxy_guard strict_v6 { 2001:db8::1 timeout 21600s }",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("缺少 %q：\n%s", want, script)
		}
	}
	if RestoreScript(nil) != "" {
		t.Fatal("没有保存的集合时不应生成脚本")
	}
}

func TestEngineRestoresSavedSets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, setsFile), []byte(`{"strict_v4":[{"addr":"160.79.104.10","ttl":100}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sys := &FakeSystem{}
	e := NewEngine(sys, dir, "br-lan", true, slog.New(slog.DiscardHandler))
	e.Start(context.Background())
	script, _, applies := sys.Snapshot()
	if applies != 2 || !strings.Contains(script, "add element inet proxy_guard strict_v4 { 160.79.104.10 timeout 100s }") {
		t.Fatalf("启动时应先装规则再写回集合：applies=%d\n%s", applies, script)
	}
}

func TestMonitorHit(t *testing.T) {
	for line, want := range map[string]bool{
		"delete table inet proxy_guard":                 true,
		"delete chain inet proxy_guard forward":         true,
		"delete rule inet proxy_guard forward handle 5": true,
		"flush ruleset": true,
		"delete element inet proxy_guard strict_v4 { 1.2.3.4 }":         false,
		"add element inet proxy_guard strict_v4 { 1.2.3.4 timeout 6h }": false,
		"delete table inet fw4":             false,
		"delete table inet proxy_guard_old": false,
	} {
		if got := monitorHit(line); got != want {
			t.Errorf("monitorHit(%q) = %v", line, got)
		}
	}
}
