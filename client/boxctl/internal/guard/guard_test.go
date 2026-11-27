package guard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func policy() Policy {
	return Policy{
		Suffixes:    []string{"Claude.ai", ".anthropic.com", "claude.ai"},
		CIDRs:       []string{"160.79.104.0/23", "2607:6bc0::/48", "1.2.3.4"},
		DNSUpstream: "127.0.0.1:1053",
		ForwardAll:  true,
	}
}

func TestNormalize(t *testing.T) {
	p, err := policy().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Suffixes, ",") != "anthropic.com,claude.ai" {
		t.Fatalf("后缀应去重、小写、去掉前导点：%v", p.Suffixes)
	}
	if strings.Join(p.CIDRs, ",") != "1.2.3.4/32,160.79.104.0/23,2607:6bc0::/48" {
		t.Fatalf("%v", p.CIDRs)
	}
	for _, bad := range []Policy{
		{Suffixes: []string{"a b.com"}},
		{Suffixes: []string{"x.com;rm"}},
		{CIDRs: []string{"1.2.3.4/33"}},
		{DNSUpstream: "8.8.8.8:53"},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Fatalf("应拒绝：%+v", bad)
		}
	}
	a, _ := policy().Normalize()
	b, _ := Policy{Suffixes: []string{"claude.ai", "anthropic.com"}, CIDRs: []string{"2607:6bc0::/48", "160.79.104.0/23", "1.2.3.4/32"},
		DNSUpstream: "127.0.0.1:1053", ForwardAll: true}.Normalize()
	if a.Version() != b.Version() {
		t.Fatal("相同内容的版本应相同")
	}
}

func TestRuleset(t *testing.T) {
	p, _ := policy().Normalize()
	s := Ruleset(p, "br-lan")
	for _, want := range []string{
		"add element inet proxy_guard strict_v4_net { 1.2.3.4/32, 160.79.104.0/23 }",
		"add element inet proxy_guard strict_v6_net { 2607:6bc0::/48 }",
		`add rule inet proxy_guard forward oifname "boxtun" accept`,
		"add rule inet proxy_guard forward meta nfproto ipv6 counter drop",
		"add rule inet proxy_guard forward ip daddr @strict_v4 counter drop",
		"add rule inet proxy_guard output ip daddr @strict_v4_net counter drop",
		`iifname "br-lan" meta l4proto { tcp, udp } th dport 53`,
		versionTag + p.Version(),
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q：\n%s", want, s)
		}
	}
	if strings.Contains(s, "delete table") {
		t.Fatal("不能删表重建，否则 dnsmasq 写入的地址会丢失")
	}
	// 进 TUN 的流量必须在丢弃规则之前放行。
	if strings.Index(s, `forward oifname "boxtun" accept`) > strings.Index(s, "forward ip daddr @strict_v4 counter drop") {
		t.Fatal("TUN 放行应在丢弃之前")
	}
	if strings.Contains(s, "add rule inet proxy_guard hold") {
		t.Fatal("没有 hold 时不应有阻断规则")
	}
	p.Hold = true
	s = Ruleset(p, "br-lan")
	if !strings.Contains(s, `add rule inet proxy_guard hold iifname "br-lan" ip daddr @strict_v4 counter drop`) {
		t.Fatalf("hold 应在 prerouting 阻断局域网：\n%s", s)
	}
}

func TestDnsmasqConf(t *testing.T) {
	p, _ := policy().Normalize()
	c := DnsmasqConf(p)
	for _, want := range []string{
		"server=/claude.ai/127.0.0.1#1053\n",
		"nftset=/claude.ai/4#inet#proxy_guard#strict_v4,6#inet#proxy_guard#strict_v6\n",
		"server=127.0.0.1#1053\nno-resolv\n",
		"rebind-domain-ok=/lan/local/localdomain/home/home.arpa/internal/\n",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("缺少 %q：\n%s", want, c)
		}
	}
	if strings.Contains(c, "bogus-priv") {
		t.Fatal("没接管 boguspriv 时不写")
	}
	p.RFC6761 = true
	c = DnsmasqConf(p)
	if !strings.Contains(c, "bogus-priv\n") || !strings.Contains(c, "server=/localhost/\n") || !strings.Contains(c, "server=/onion/\n") || strings.Contains(c, "server=/local/") {
		t.Fatalf("接管 boguspriv 时补回 rfc6761.conf（.local 除外）：\n%s", c)
	}
	p.ForwardAll = false
	if c := DnsmasqConf(p); strings.Contains(c, "no-resolv") || !strings.Contains(c, "bogus-priv\n") {
		t.Fatal("内核不在时其他域名应回到原来的上游")
	}
}

func TestEngineLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sys := &FakeSystem{}
	e := NewEngine(sys, dir, "br-lan", true, slog.New(slog.DiscardHandler))
	e.Start(ctx)
	if st := e.Status(); st.State != StateHealthy || !st.Installed {
		t.Fatalf("没有策略时也应装上空规则（IPv6 不转发）：%+v", st)
	}
	if err := e.Apply(ctx, policy()); err != nil {
		t.Fatal(err)
	}
	_, dns, applies := sys.Snapshot()
	if !strings.Contains(dns, "claude.ai") {
		t.Fatal("应写 dnsmasq 片段")
	}
	if err := e.Apply(ctx, policy()); err != nil {
		t.Fatal(err)
	}
	if _, _, n := sys.Snapshot(); n != applies {
		t.Fatal("相同策略不应重装")
	}

	// fw4 restart 删了表：自检时恢复并记事件。
	sys.Drop()
	e.Check(ctx)
	if _, _, n := sys.Snapshot(); n != applies+1 {
		t.Fatal("规则缺失时应重装")
	}
	if ev := e.Status().Events; len(ev) == 0 || !strings.Contains(ev[0].Message, "已恢复") {
		t.Fatalf("%+v", ev)
	}

	hold := policy()
	hold.Hold, hold.Reason = true, "Claude 出口不符"
	if err := e.Apply(ctx, hold); err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.State != StateBlocked || !st.Hold || st.Reason != "Claude 出口不符" {
		t.Fatalf("%+v", st)
	}

	// 重启后先加载上一份策略（M9-02）。
	sys2 := &FakeSystem{}
	e2 := NewEngine(sys2, dir, "br-lan", true, slog.New(slog.DiscardHandler))
	e2.Start(ctx)
	if st := e2.Status(); st.State != StateBlocked || st.Domains != 2 {
		t.Fatalf("应加载上次的策略：%+v", st)
	}

	sys2.Fail = errors.New("nft 不存在")
	sys2.Drop()
	e2.Check(ctx)
	if st := e2.Status(); st.State != StateError || st.LastError == "" {
		t.Fatalf("%+v", st)
	}
	if err := e2.Apply(ctx, Policy{Suffixes: []string{"bad domain"}}); err == nil {
		t.Fatal("无效策略应拒绝")
	}
}

func TestRulesetDeviceBlocks(t *testing.T) {
	p, err := Policy{Blocks: []Block{
		{IP: "192.168.1.20"},
		{IP: "192.168.1.10", Windows: []Window{{Days: []int{6, 7}, Start: "09:00", End: "21:00"}, {Days: []int{7}, Start: "22:00", End: "06:30"}}},
	}}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocks[0].IP != "192.168.1.10" {
		t.Fatal("按地址排序，版本稳定")
	}
	s := Ruleset(p, "br-lan")
	for _, want := range []string{
		`add chain inet proxy_guard device_block { type filter hook prerouting priority raw - 1; policy accept; }`,
		`add rule inet proxy_guard device_block iifname "br-lan" ip saddr 192.168.1.10 jump dev_0`,
		`add rule inet proxy_guard dev_0 meta day { "Saturday", "Sunday" } meta hour "09:00"-"21:00" return`,
		`add rule inet proxy_guard dev_0 meta day { "Sunday" } meta hour "22:00"-"23:59:59" return`,
		`add rule inet proxy_guard dev_0 meta day { "Monday" } meta hour "00:00"-"06:30" return`,
		`add rule inet proxy_guard dev_1 counter drop comment "device 192.168.1.20"`,
		`add chain inet proxy_guard dev_2`,
		`delete chain inet proxy_guard dev_2`,
	} {
		if !strings.Contains(s, want+"\n") {
			t.Errorf("缺少：%s", want)
		}
	}
	for _, bad := range []Policy{
		{Blocks: []Block{{IP: "fe80::1"}}},
		{Blocks: []Block{{IP: "192.168.1.2", Windows: []Window{{Start: "25:00", End: "01:00"}}}}},
		{Blocks: []Block{{IP: "192.168.1.2", Windows: []Window{{Days: []int{8}, Start: "01:00", End: "02:00"}}}}},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Errorf("应拒绝：%+v", bad.Blocks)
		}
	}
	// 没有管控时版本不变：不影响旧策略文件。
	a, _ := Policy{Suffixes: []string{"claude.ai"}}.Normalize()
	if strings.Contains(string(mustJSON(a)), "blocks") {
		t.Fatal("没有设备管控时不输出 blocks")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
