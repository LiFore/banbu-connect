package gen

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"boxctl/internal/groups"
	"boxctl/internal/guard"
	"boxctl/internal/killswitch"
)

func input() Input {
	return Input{
		Nodes: []map[string]any{
			{"name": "香港 - 01", "type": "ss", "server": "198.51.100.1", "port": 8388, "cipher": "aes-128-gcm", "password": "x"},
			{"name": "美国 - 01", "type": "ss", "server": "us.example.com", "port": 8388, "cipher": "aes-128-gcm", "password": "x"},
		},
		Groups: []groups.Export{
			{Name: "Claude", Type: groups.TypeSelect, Strict: true, Members: []string{"香港 - 01"}},
			{Name: "代理模式", Type: groups.TypeSelect, Members: []string{"香港 - 01", "DIRECT"}},
			{Name: "漏网之鱼", Type: groups.TypeSelect, Members: []string{"DIRECT", "代理模式"}},
			{Name: "自动", Type: groups.TypeURLTest, Members: []string{"香港 - 01"},
				Options: groups.Options{URL: "http://t", Interval: 300, Tolerance: 50, Hidden: true}},
		},
		Services: []killswitch.Service{
			{Name: "Claude", Enabled: true, Group: "Claude", Suffixes: []string{"claude.ai", "anthropic.com"}},
			{Name: "停用的服务", Enabled: false, Group: "Claude", Suffixes: []string{"off.example.com"}},
		},
		CustomRules: []string{"# 注释", "", "DOMAIN-SUFFIX,ping0.cc,Claude"},
		DNSListen:   "127.0.0.1:1053", Mode: "rule", TUN: true, MixedPort: 7890,
		Controller: "127.0.0.1:19190", Secret: "s",
	}
}

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func rulesOf(t *testing.T, in Input) []string {
	t.Helper()
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range parse(t, r.YAML)["rules"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

func TestDeterministic(t *testing.T) {
	a, err := Build(input())
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		b, err := Build(input())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.YAML, b.YAML) {
			t.Fatal("同一份输入必须生成相同的配置：判断配置是否变化依赖这一点")
		}
	}
}

// 受保护的域名自动带严格规则，这就是“自动同步”。
func TestProtectedSuffixesGetStrictRules(t *testing.T) {
	rules := rulesOf(t, input())
	firstDirect := slices.IndexFunc(rules, func(r string) bool { return strings.Contains(r, ",DIRECT") })
	for _, suffix := range []string{"claude.ai", "anthropic.com", "off.example.com"} {
		rule := slices.Index(rules, "DOMAIN-SUFFIX,"+suffix+",Claude")
		quic := slices.Index(rules, "AND,((DOMAIN-SUFFIX,"+suffix+"),(NETWORK,udp)),REJECT")
		if rule < 0 || quic < 0 {
			t.Fatalf("%s 缺少规则：%v", suffix, rules)
		}
		if quic > rule {
			t.Fatalf("%s 的 QUIC 阻断必须在普通规则之前", suffix)
		}
		if rule > firstDirect {
			t.Fatalf("%s 的严格规则必须在所有直连规则之前", suffix)
		}
	}
}

func TestRuleOrderAndFilters(t *testing.T) {
	rules := rulesOf(t, input())
	if rules[0] != "DOMAIN-SUFFIX,ping0.cc,Claude" {
		t.Fatalf("自定义规则应排在最前，并跳过注释和空行：%v", rules[:2])
	}
	if rules[len(rules)-1] != "MATCH,漏网之鱼" {
		t.Fatalf("最后一条应是兜底规则：%s", rules[len(rules)-1])
	}
	// 目标组不存在的内置规则被跳过，配置仍然有效。
	for _, r := range rules {
		if strings.Contains(r, ",Spotify") || strings.Contains(r, ",电报消息") || strings.Contains(r, ",ChatGPT") {
			t.Fatalf("不存在的组不应出现：%s", r)
		}
	}
	if !slices.Contains(rules, "RULE-SET,claude,Claude") || slices.Index(rules, "RULE-SET,claude,Claude") > slices.Index(rules, "RULE-SET,direct,DIRECT") {
		t.Fatal("严格规则集应排在直连规则之前")
	}
	if !slices.Contains(rules, "RULE-SET,cncidr,DIRECT,no-resolve") {
		t.Fatal("IP 规则应带 no-resolve")
	}
}

func TestNoFallbackGroupUsesDirect(t *testing.T) {
	in := input()
	in.Groups = slices.DeleteFunc(in.Groups, func(g groups.Export) bool { return g.Name == "漏网之鱼" })
	rules := rulesOf(t, in)
	if rules[len(rules)-1] != "MATCH,DIRECT" {
		t.Fatal(rules[len(rules)-1])
	}
}

func TestRefusesBrokenProtection(t *testing.T) {
	cases := map[string]func(*Input){
		"组不存在":     func(i *Input) { i.Services[0].Group = "不存在" },
		"组不是严格的":   func(i *Input) { i.Services[0].Group = "代理模式" },
		"后缀重复":     func(i *Input) { i.Services[1].Suffixes = []string{"claude.ai"} },
		"两条 MATCH": func(i *Input) { i.CustomRules = []string{"MATCH,DIRECT", "MATCH,代理模式"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := input()
			mutate(&in)
			if _, err := Build(in); err == nil {
				t.Fatal("应当拒绝")
			}
		})
	}
}

func TestDNSAndTUN(t *testing.T) {
	in := input()
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, r.YAML)
	dns := m["dns"].(map[string]any)
	if dns["listen"] != "127.0.0.1:1053" || dns["enhanced-mode"] != "redir-host" {
		t.Fatalf("%v", dns)
	}
	policy := dns["nameserver-policy"].(map[string]any)
	if _, ok := policy["us.example.com"]; !ok {
		t.Fatal("域名形式的节点地址应走国内 DNS")
	}
	if _, ok := policy["198.51.100.1"]; ok {
		t.Fatal("IP 形式的节点不需要解析")
	}
	if _, ok := dns["fallback"]; ok {
		t.Fatal("不应使用旧式 fallback")
	}
	if strings.Contains(string(r.YAML), "fake-ip") {
		t.Fatal("桌面客户端不使用 fake-ip（ADR-014）")
	}
	sniffer := m["sniffer"].(map[string]any)
	if sniffer["force-dns-mapping"] != true || sniffer["parse-pure-ip"] != true {
		t.Fatalf("真实 IP 的连接要靠嗅探还原域名：%v", sniffer)
	}
	if m["tun"].(map[string]any)["enable"] != true || m["allow-lan"] != false {
		t.Fatalf("%v", m["tun"])
	}
	in.TUN, in.Mode = false, "global"
	r, _ = Build(in)
	m = parse(t, r.YAML)
	if m["tun"].(map[string]any)["enable"] != false || m["mode"] != "global" {
		t.Fatal("开关没有生效")
	}
}

func TestDNSTakeover(t *testing.T) {
	in := input()
	in.StrictRoute = true
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, r.YAML)
	tun := m["tun"].(map[string]any)
	if tun["dns-mode"] != "hijack" || tun["strict-route"] != true {
		t.Fatalf("TUN 要接管系统 DNS：%v", tun)
	}
	dns := m["dns"].(map[string]any)
	policy := dns["nameserver-policy"].(map[string]any)
	for _, list := range [][]any{dns["nameserver"].([]any), policy["geosite:geolocation-!cn"].([]any)} {
		for _, s := range list {
			if !strings.HasSuffix(s.(string), "#代理模式") {
				t.Fatalf("境外 DNS 上游要走代理：%v", s)
			}
		}
	}
	for _, list := range [][]any{policy["geosite:cn"].([]any), policy["us.example.com"].([]any)} {
		for _, s := range list {
			if strings.Contains(s.(string), "#") {
				t.Fatalf("国内 DNS 和节点域名的解析不能依赖代理：%v", s)
			}
		}
	}
}

func TestDNSUpstreamDirectCases(t *testing.T) {
	cases := map[string]func(*Input){
		"直连模式不依赖节点": func(i *Input) { i.Mode = "direct" },
		"代理组被删除": func(i *Input) {
			i.Groups = slices.DeleteFunc(i.Groups, func(g groups.Export) bool { return g.Name == "代理模式" })
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := input()
			mutate(&in)
			r, err := Build(in)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range parse(t, r.YAML)["dns"].(map[string]any)["nameserver"].([]any) {
				if strings.Contains(s.(string), "#") {
					t.Fatalf("不应指定代理组：%v", s)
				}
			}
			if name == "代理组被删除" && len(r.Warnings) == 0 {
				t.Fatal("DNS 直连要提示用户")
			}
		})
	}
}

func TestDuplicateNodeNames(t *testing.T) {
	in := input()
	in.Nodes = append(in.Nodes, map[string]any{"name": "香港 - 01", "type": "ss"})
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 || len(parse(t, r.YAML)["proxies"].([]any)) != 2 {
		t.Fatalf("%v", r.Warnings)
	}
}

func TestGroupMappingOptions(t *testing.T) {
	r, err := Build(input())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range parse(t, r.YAML)["proxy-groups"].([]any) {
		m := g.(map[string]any)
		if m["name"] == "自动" && (m["tolerance"] != 50 || m["hidden"] != true || m["url"] != "http://t") {
			t.Fatalf("%v", m)
		}
		if m["name"] == "Claude" {
			if _, ok := m["url"]; ok {
				t.Fatal("select 组不应带 url")
			}
		}
	}
}

func TestStrictProbeListeners(t *testing.T) {
	in := input()
	in.ProbePortBase = 19200
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Probes) != 1 || r.Probes["Claude"] != 19200 {
		t.Fatalf("只有严格组有探测入口：%v", r.Probes)
	}
	ls, _ := parse(t, r.YAML)["listeners"].([]any)
	// 严格组的探测入口在前，后面是每个代理组和 DIRECT 的线路探测入口。
	if len(ls) != 1+1+len(in.Groups) {
		t.Fatalf("listeners：%v", ls)
	}
	if r.Routes["DIRECT"] != 19200+RouteProbeOffset || r.Routes["Claude"] == 0 || len(r.Routes) != 1+len(in.Groups) {
		t.Fatalf("线路探测入口：%v", r.Routes)
	}
	l := ls[0].(map[string]any)
	if l["proxy"] != "Claude" || l["listen"] != "127.0.0.1" || l["port"] != 19200 || l["type"] != "http" {
		t.Fatalf("探测入口：%v", l)
	}
	in.ProbePortBase = 0
	if r, _ := Build(in); parse(t, r.YAML)["listeners"] != nil {
		t.Fatal("不要求时不生成探测入口")
	}
}

func TestRouterTUN(t *testing.T) {
	in := input()
	in.Router, in.TUN = true, false
	in.RouteExclude = []string{"192.168.100.0/24"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, r.YAML)
	tun := m["tun"].(map[string]any)
	if tun["enable"] != true || tun["auto-redirect"] != true || tun["auto-route"] != true {
		t.Fatalf("路由盒应始终用 TUN 接管局域网：%v", tun)
	}
	if hijack := tun["dns-hijack"].([]any); len(hijack) != 0 {
		t.Fatalf("路由盒不劫持 DNS，由 dnsmasq 转发进来：%v", hijack)
	}
	if ex := tun["route-exclude-address"].([]any); len(ex) != 1 || ex[0] != "192.168.100.0/24" {
		t.Fatalf("%v", ex)
	}
	if tun["device"] != guard.TUNDevice {
		t.Fatalf("TUN 网卡名须与 guard 放行的一致：%v", tun["device"])
	}
	if m["allow-lan"] != false || m["bind-address"] != "127.0.0.1" {
		t.Fatal("局域网走 TUN，代理端口不对局域网开放")
	}
	if _, ok := m["tun"].(map[string]any)["dns-mode"]; ok {
		t.Fatal("dns-mode 只用于桌面")
	}
}

func TestAllowLAN(t *testing.T) {
	in := input()
	in.Router, in.AllowLAN = true, true
	in.LANAllowedIPs = []string{"192.168.100.0/24", "10.99.0.0/16"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, r.YAML)
	if m["allow-lan"] != true || m["bind-address"] != "*" {
		t.Fatalf("打开后代理端口应对局域网开放：%v %v", m["allow-lan"], m["bind-address"])
	}
	want := []any{"192.168.100.0/24", "10.99.0.0/16", "127.0.0.0/8", "::1/128"}
	if got := m["lan-allowed-ips"].([]any); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("来源须限定在局域网网段并放行回环：%v", got)
	}
	if m["external-controller"] != in.Controller {
		t.Fatal("控制接口仍只监听本机")
	}

	in.LANAllowedIPs = nil
	if r, _ := Build(in); parse(t, r.YAML)["allow-lan"] != false {
		t.Fatal("不知道局域网网段时不开放")
	}
}

func TestGateRetargetsProtectedRules(t *testing.T) {
	in := input()
	in.ProbePortBase = 19200
	in.CustomRules = []string{"DOMAIN-KEYWORD,claude,Claude", "IP-CIDR,160.79.104.0/23,Claude,no-resolve", "DOMAIN,example.com,DIRECT"}
	in.Gate = &GateInput{Port: 19180, Secret: "s3cret", DirectPort: 19181}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.GateDirect != 19181 {
		t.Fatalf("GateDirect = %d", r.GateDirect)
	}
	cfg := parse(t, r.YAML)
	gate := GateProxyName("Claude")
	var rules []string
	for _, x := range cfg["rules"].([]any) {
		rules = append(rules, x.(string))
	}
	for _, want := range []string{
		"DOMAIN-KEYWORD,claude," + gate,
		"IP-CIDR,160.79.104.0/23," + gate + ",no-resolve",
		"DOMAIN,example.com,DIRECT",
		"RULE-SET,claude," + gate,
	} {
		if !slices.Contains(rules, want) {
			t.Fatalf("缺少规则 %q：%v", want, rules)
		}
	}
	for _, r := range rules {
		if strings.HasSuffix(r, ",Claude") || strings.Contains(r, ",Claude,") {
			t.Fatalf("还有直接指向严格组的规则：%s", r)
		}
	}
	var found bool
	for _, x := range cfg["proxies"].([]any) {
		p := x.(map[string]any)
		if p["name"] == gate {
			found = true
			if p["type"] != "socks5" || p["server"] != "127.0.0.1" || p["port"] != 19180 || p["username"] != "Claude" || p["password"] != "s3cret" || p["udp"] != false {
				t.Fatalf("闸门出站：%v", p)
			}
		}
	}
	if !found {
		t.Fatal("没有生成闸门出站")
	}
	ls := cfg["listeners"].([]any)
	last := ls[len(ls)-1].(map[string]any)
	if last["proxy"] != "DIRECT" || last["port"] != 19181 || last["listen"] != "127.0.0.1" {
		t.Fatalf("DIRECT 入口：%v", last)
	}
	// 严格组本身和它的探测入口保持不变：闸门不允许直连时经它出去。
	if r.Probes["Claude"] != 19200 {
		t.Fatalf("探测入口：%v", r.Probes)
	}
}

// redir-host DNS 模板（M7-01）：国内、境外上游分离，按 geosite 分流；国内上游只有国内 DNS，不用 fallback 旧写法。
func TestDNSTemplate(t *testing.T) {
	for _, router := range []bool{false, true} {
		in := input()
		in.Router = router
		if !router {
			in.LANDNS = "127.0.0.1:19182" // 桌面端由 desktop 传入
		}
		r, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		dns := parse(t, r.YAML)["dns"].(map[string]any)
		if dns["enhanced-mode"] != "redir-host" {
			t.Fatalf("enhanced-mode = %v", dns["enhanced-mode"])
		}
		for _, k := range []string{"fallback", "fallback-filter"} {
			if _, ok := dns[k]; ok {
				t.Fatalf("不应使用 %s", k)
			}
		}
		domestic := map[string]bool{"https://223.5.5.5/dns-query": true, "https://1.12.12.12/dns-query": true}
		policy := dns["nameserver-policy"].(map[string]any)
		cn := policy["geosite:cn"].([]any)
		if len(cn) == 0 {
			t.Fatal("geosite:cn 没有上游")
		}
		// 桌面端国内组还要有局域网 DNS 转发口兜底：路由盒网络拦公共 DoH（BXC-D0-002）。路由盒没有。
		hasLAN := false
		for _, u := range cn {
			if u == "udp://127.0.0.1:19182" {
				hasLAN = true
				continue
			}
			if !domestic[u.(string)] {
				t.Fatalf("国内上游混入了 %v", u)
			}
		}
		if hasLAN == router {
			t.Fatalf("router=%v 时国内组局域网 DNS 兜底 = %v", router, hasLAN)
		}
		for _, u := range dns["default-nameserver"].([]any) {
			if !domestic[u.(string)] {
				t.Fatalf("default-nameserver 混入了 %v", u)
			}
		}
		for _, u := range dns["nameserver"].([]any) {
			if domestic[u.(string)] || !strings.HasSuffix(u.(string), "#代理模式") {
				t.Fatalf("境外上游应经代理组：%v", u)
			}
		}
	}
}

func TestMirror(t *testing.T) {
	if _, err := ValidateMirror("http://m.example.com"); err == nil {
		t.Fatal("应只接受 https")
	}
	m, err := ValidateMirror(" https://m.example.com/jsd/ ")
	if err != nil || m != "https://m.example.com/jsd" {
		t.Fatalf("ValidateMirror = %q %v", m, err)
	}
	if got := MirrorURL("https://cdn.jsdelivr.net/gh/a/b@release/x.txt", m); got != "https://m.example.com/jsd/gh/a/b@release/x.txt" {
		t.Fatalf("MirrorURL = %s", got)
	}
	if got := MirrorURL("https://example.org/x.yaml", m); got != "https://example.org/x.yaml" {
		t.Fatal("非 jsDelivr 地址不改写")
	}

	in := input()
	in.Mirror = m
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(r.YAML, []byte("jsdelivr.net")) {
		t.Fatalf("设置镜像后不应再出现 jsDelivr 地址：\n%s", r.YAML)
	}
	if !slices.Contains(rulesOf(t, in), "DOMAIN,m.example.com,DIRECT") {
		t.Fatal("镜像主机应固定直连")
	}
}

func TestCustomMatchBecomesFallback(t *testing.T) {
	in := input()
	in.CustomRules = []string{"MATCH,代理模式", "DOMAIN,a.com,DIRECT"}
	rules := rulesOf(t, in)
	if rules[len(rules)-1] != "MATCH,代理模式" || slices.Contains(rules[:len(rules)-1], "MATCH,代理模式") {
		t.Fatalf("自定义 MATCH 应放到最后：%v", rules[len(rules)-3:])
	}
	in.CustomRules = []string{"MATCH,不存在"}
	if rules := rulesOf(t, in); rules[len(rules)-1] != "MATCH,漏网之鱼" {
		t.Fatalf("目标不存在时用默认兜底：%v", rules[len(rules)-1])
	}
}

// 闸门直连、不在路由盒网络上时，除 DIRECT / REJECT 和受保护服务外的规则目标都经「路由盒切换」选择组（ADR-073）。
func TestGateWrapsOtherTargetsForBox(t *testing.T) {
	in := input()
	in.ProbePortBase = 19200
	in.CustomRules = []string{"DOMAIN,example.com,DIRECT", "DOMAIN,ads.example,REJECT", "DOMAIN-SUFFIX,google.com,代理模式"}
	in.Gate = &GateInput{Port: 19180, Secret: "s3cret", DirectPort: 19181}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.BoxRoutes[BoxRouteName("代理模式")] != "代理模式" || r.BoxRoutes[BoxRouteName("漏网之鱼")] != "漏网之鱼" {
		t.Fatalf("BoxRoutes = %v", r.BoxRoutes)
	}
	for name := range r.BoxRoutes {
		if name == BoxRouteName("Claude") || name == BoxRouteName("REJECT") {
			t.Fatalf("严格组、REJECT 不能包进路由盒切换组：%s", name)
		}
	}
	cfg := parse(t, r.YAML)
	var rules []string
	for _, x := range cfg["rules"].([]any) {
		rules = append(rules, x.(string))
	}
	for _, want := range []string{"DOMAIN,example.com,DIRECT", "DOMAIN,ads.example,REJECT", "DOMAIN-SUFFIX,google.com," + BoxRouteName("代理模式"), "MATCH," + BoxRouteName("漏网之鱼")} {
		if !slices.Contains(rules, want) {
			t.Fatalf("缺少规则 %q：%v", want, rules)
		}
	}
	var group map[string]any
	for _, x := range cfg["proxy-groups"].([]any) {
		if g := x.(map[string]any); g["name"] == BoxRouteName("代理模式") {
			group = g
		}
	}
	if group == nil || group["type"] != "select" || fmt.Sprint(group["proxies"]) != "[代理模式 DIRECT]" {
		t.Fatalf("路由盒切换组：%v", group)
	}
	// 没开闸门时不生成。
	in.Gate = nil
	if r, _ := Build(in); len(r.BoxRoutes) != 0 {
		t.Fatalf("没开闸门时 BoxRoutes = %v", r.BoxRoutes)
	}
}

func TestLocalDNSPolicy(t *testing.T) {
	in := input()
	in.LocalDNS = []string{"udp://192.168.1.1:53"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	policy := parse(t, r.YAML)["dns"].(map[string]any)["nameserver-policy"].(map[string]any)
	for _, sfx := range []string{"+.lan", "+.local", "+.home.arpa", "+.internal", "+.168.192.in-addr.arpa", "+.31.172.in-addr.arpa"} {
		got, ok := policy[sfx].([]any)
		if !ok || len(got) != 1 || got[0] != "udp://192.168.1.1:53" {
			t.Fatalf("%s 应只问内网 DNS：%v", sfx, policy[sfx])
		}
	}
	// 内网后缀排在 geosite 前面：mihomo 按顺序匹配，先命中的生效。
	y := string(r.YAML)
	if strings.Index(y, "+.lan:") > strings.Index(y, "geosite:cn:") {
		t.Fatal("内网后缀须排在 geosite 之前")
	}

	in.LocalDNS = nil
	r, _ = Build(in)
	if _, ok := parse(t, r.YAML)["dns"].(map[string]any)["nameserver-policy"].(map[string]any)["+.lan"]; ok {
		t.Fatal("没有内网 DNS 时不加")
	}
}

// 在路由盒网络上（ADR-084）：除 DIRECT 外的规则目标（含受保护服务、REJECT）都经路由盒的代理入口，
// 不生成闸门出站和路由盒切换组；出口探测的 DIRECT 入口也经路由盒。
func TestBoxRoutesEverythingButDirect(t *testing.T) {
	in := input()
	in.ProbePortBase = 19200
	in.CustomRules = []string{"DOMAIN,example.com,DIRECT", "DOMAIN,ads.example,REJECT"}
	in.Gate = &GateInput{Port: 19180, Secret: "s3cret", DirectPort: 19181}
	in.Box = &ProxyEndpoint{Server: "192.168.10.100", Port: 19444, User: "banbu", Password: "pw"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.BoxRoutes) != 0 {
		t.Fatalf("在路由盒网络上不需要切换组：%v", r.BoxRoutes)
	}
	cfg := parse(t, r.YAML)
	var box map[string]any
	for _, x := range cfg["proxies"].([]any) {
		p := x.(map[string]any)
		if p["name"] == BoxProxyName {
			box = p
		}
		if strings.HasPrefix(fmt.Sprint(p["name"]), "出口闸门·") {
			t.Fatalf("在路由盒网络上不经出口闸门：%v", p["name"])
		}
	}
	if box == nil || box["type"] != "socks5" || box["server"] != "192.168.10.100" || box["port"] != 19444 || box["username"] != "banbu" || box["udp"] != true {
		t.Fatalf("路由盒出站：%v", box)
	}
	for _, x := range cfg["rules"].([]any) {
		r := x.(string)
		if t2 := ruleTargetOf(r); t2 != "DIRECT" && t2 != BoxProxyName && !strings.HasSuffix(r, ",no-resolve") {
			t.Fatalf("规则应直连或经路由盒：%s", r)
		}
	}
	rules := fmt.Sprint(cfg["rules"])
	for _, want := range []string{"DOMAIN,example.com,DIRECT", "DOMAIN,ads.example," + BoxProxyName, "MATCH," + BoxProxyName} {
		if !strings.Contains(rules, want) {
			t.Fatalf("缺少 %q：%s", want, rules)
		}
	}
	for _, x := range cfg["listeners"].([]any) {
		if l := x.(map[string]any); l["name"] == "gate-direct" && l["proxy"] != BoxProxyName {
			t.Fatalf("出口探测入口应经路由盒：%v", l)
		}
	}
}

// 路由盒给客户端的代理入口（ADR-084）：mixed 监听，带独立用户（不受 allow-lan 限制）。
func TestClientProxyListener(t *testing.T) {
	in := input()
	in.ClientProxy = &ProxyEndpoint{Server: "192.168.10.100", Port: 19444, User: "banbu", Password: "pw"}
	r, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]any
	for _, x := range parse(t, r.YAML)["listeners"].([]any) {
		if m := x.(map[string]any); m["name"] == "box-clients" {
			l = m
		}
	}
	if l == nil || l["type"] != "mixed" || l["listen"] != "192.168.10.100" || l["port"] != 19444 || l["udp"] != true ||
		fmt.Sprint(l["users"]) != "[map[password:pw username:banbu]]" {
		t.Fatalf("客户端代理入口：%v", l)
	}
}
