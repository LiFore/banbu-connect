// Package gen 从结构化数据生成 Mihomo 配置。
//
// 受保护服务的域名和严格规则来自同一份数据，所以两者不会对不上：
// 这正是单独使用规则脚本时需要人工同步的那部分。
package gen

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"boxctl/internal/groups"
	"boxctl/internal/killswitch"
)

const (
	loyalsoldier = "https://cdn.jsdelivr.net/gh/Loyalsoldier/clash-rules@release/"
	blackmatrix  = "https://cdn.jsdelivr.net/gh/blackmatrix7/ios_rule_script@master/rule/Clash/"
	geoBase      = "https://testingcf.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release/"
	geoHost      = "testingcf.jsdelivr.net"

	dnsProxyGroup = "代理模式" // 境外 DNS 上游走的代理组

	// RouterTUNDevice 是路由盒 TUN 网卡名，须与 guard.TUNDevice 一致（有测试保证）。
	RouterTUNDevice = "boxtun"
)

// Input 是生成一份配置所需的全部数据。
type Input struct {
	Nodes       []map[string]any
	Groups      []groups.Export
	Services    []killswitch.Service
	CustomRules []string
	// RuleSets 是内置规则（远程规则集、GEOIP），按顺序排在自定义规则之后；为 nil 时用 DefaultRuleSets。
	RuleSets  []RuleSet
	DNSListen string
	// LANDNS 是服务的局域网 DNS 转发口（只用于桌面），加进国内 DNS 组兜底，见 trust.ServeLANDNS。
	LANDNS string
	// LocalDNS 是内网域名（LocalSuffixes）专用的上游，如 udp://192.168.1.1:53：桌面是局域网 DNS 转发口，
	// 路由盒是 WAN 口拿到的私有地址 DNS（上级本地 DNS）。为空时内网域名和其他域名一样处理。
	LocalDNS    []string
	Mode        string // rule / global / direct
	TUN         bool
	StrictRoute bool
	MixedPort   int
	// AllowLAN 为 true 时代理端口（mixed-port）对局域网开放：监听所有地址，只接受 LANAllowedIPs 里的来源
	// 和本机回环。只用于路由盒，LANAllowedIPs 为空时不开放。
	AllowLAN      bool
	LANAllowedIPs []string
	Controller    string
	Secret        string
	// Router 为 true 时按路由盒生成：TUN 用 auto-redirect 接管局域网转发的流量；
	// 不劫持 DNS，局域网的 DNS 由 dnsmasq 承接后转发到 DNSListen，这样 dnsmasq 能把
	// 受保护域名的解析结果写进 guard 的集合。
	Router bool
	// RouteExclude 是路由盒 TUN 不接管的目标网段（局域网、管理面）。
	RouteExclude []string
	// ProbePortBase 大于 0 时，给每个严格组开一个只监听本机的 http 入口（端口从它起依次分配），
	// 入站流量直接交给该组，用来探测这条线路的实际出口。
	ProbePortBase int
	// Gate 不为空时，受保护服务改经出口闸门（只用于桌面，需要 ProbePortBase）。
	Gate *GateInput
	// HoldRules 为 true 时，每条指向严格组的规则前插一条同条件、指向 HoldProxyName 的副本（路由盒）。
	// 内核加载配置时副本生效（先阻断），面板确认出口后经 PATCH /rules/disable 关掉；hold 时再打开。
	// 这样经加密 DNS 拿到地址、不在 guard 严格集合里的连接，hold 时在内核里也会被拒绝。
	HoldRules bool
	// Mirror 是下载镜像（已校验），jsDelivr 的地址改写到它上面；为空时不改写（M7-04）。
	Mirror string
	// DeviceRules 是局域网设备的出口规则（SRC-IP-CIDR、SUB-RULE），排在所有规则最前，
	// 包括受保护服务的规则（ADR-035）。目标不存在的跳过并给出提示。
	DeviceRules []string
	// SubRules 是设备自定义规则（SUB-RULE 引用的子规则集），名称 → 规则（F2-007）。
	SubRules map[string][]string
	// ExcludeMAC 是完全不经过内核的设备（路由盒 TUN 的 exclude-mac-address，ADR-040）。
	ExcludeMAC []string
	// ClientProxy 不为空时（路由盒）开一个给客户端用的代理入口（ADR-084）。
	ClientProxy *ProxyEndpoint
	// Box 不为空时（桌面在路由盒网络上，ADR-084）除 DIRECT 外的规则目标都改经路由盒的代理入口，
	// 按路由盒的策略走；内核的 DIRECT 入口（出口探测用）也经它。
	Box *ProxyEndpoint
}

// ProxyEndpoint 是路由盒给客户端的代理入口（boxfind.ProxyPort）。路由盒上 Server 是监听地址。
type ProxyEndpoint struct {
	Server   string
	Port     int
	User     string
	Password string
}

// BoxProxyName 是桌面配置里经路由盒代理入口的出站名。
const BoxProxyName = "路由盒"

// GateInput 让受保护服务的连接先交给出口闸门：每个受保护服务的严格组生成一个指向闸门的 socks5 出站，
// 用户名是组名；指向这些组的规则都改指向对应的出站。闸门确认出口后把连接转给 DIRECT 入口直连，
// 否则转给该组的探测入口（经代理）。
type GateInput struct {
	Port       int // 闸门在 127.0.0.1 上的端口
	Secret     string
	DirectPort int // 内核的 DIRECT 入口端口（http，只监听本机）
}

// GateDirectListener 是内核 DIRECT 入口的监听名（出口闸门放行的连接、出口探测经它直连）。
const GateDirectListener = "gate-direct"

// GateProxyName 是某个严格组对应的闸门出站名。
func GateProxyName(group string) string { return "出口闸门·" + group }

// BoxRoutePrefix 是「路由盒切换」选择组的名字前缀（ADR-073）。开了闸门直连、不在路由盒网络上时，除 DIRECT / REJECT
// 和严格组外，每个规则目标 T 都包一层选择组「路由盒·T」（成员 T、DIRECT）：在受信 Wi-Fi 上服务把它切到 DIRECT，
// 其他时候是 T（经本机代理）。默认选中 T，切换失败时总是经本机代理。在路由盒网络上改经路由盒的代理入口（Input.Box）。
const BoxRoutePrefix = "路由盒·"

// BoxRouteName 是规则目标 target 对应的「路由盒切换」选择组名。
func BoxRouteName(target string) string { return BoxRoutePrefix + target }

// RouteProbeOffset 是线路探测入口相对 ProbePortBase 的偏移：严格组的探测入口在前面，最多 100 个。
const RouteProbeOffset = 100

// Result 是生成结果。Warnings 是不影响运行、但用户应该知道的情况。
type Result struct {
	YAML     []byte
	Warnings []string
	Probes   map[string]int // 严格组 → 探测入口端口
	// Routes 是每个代理组（和 DIRECT）的线路探测入口端口：线路预览、指定域名的出口测试经它请求
	// Cloudflare trace，看这个组当前选中的线路的出口。只监听本机，端口从 ProbePortBase+RouteProbeOffset 起。
	Routes map[string]int
	// GateDirect 是内核 DIRECT 入口的端口；没有生成闸门时为 0。
	GateDirect int
	// BoxRoutes 是「路由盒切换」选择组 → 原来的规则目标；没开闸门时为空。
	BoxRoutes map[string]string
}

// Build 生成配置。同一份输入的结果逐字节相同，用于判断配置是否真的变了。
func Build(in Input) (Result, error) {
	var warnings []string
	nodeNames := map[string]bool{}
	proxies := make([]map[string]any, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		name, _ := n["name"].(string)
		if nodeNames[name] {
			warnings = append(warnings, "节点名称重复，已跳过：「"+name+"」")
			continue
		}
		nodeNames[name] = true
		proxies = append(proxies, n)
	}

	groupByName := map[string]groups.Export{}
	proxyGroups := make([]map[string]any, 0, len(in.Groups))
	for _, g := range in.Groups {
		groupByName[g.Name] = g
		proxyGroups = append(proxyGroups, groupMapping(g))
	}
	targetExists := func(t string) bool {
		_, ok := groupByName[t]
		return ok || t == "DIRECT" || t == "REJECT" || nodeNames[t]
	}

	rules := []string{}
	fallback := "" // 自定义规则里的 MATCH 目标
	for _, r := range in.DeviceRules {
		if t := ruleTargetOf(r); strings.HasPrefix(r, "SUB-RULE,") {
			if _, ok := in.SubRules[t]; !ok {
				warnings = append(warnings, "设备规则引用的子规则不存在，已跳过："+r)
				continue
			}
		} else if !targetExists(t) {
			warnings = append(warnings, "设备规则的出口「"+t+"」不存在，已跳过："+r)
			continue
		}
		rules = append(rules, r)
	}
	for _, r := range in.CustomRules {
		r = strings.TrimSpace(r)
		if r == "" || strings.HasPrefix(r, "#") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(r), "MATCH,") {
			// 兜底规则（M5-03）：只能有一条，不管写在哪里都放到最后（规则集之后），代替默认的兜底组。
			if fallback != "" {
				return Result{}, fmt.Errorf("MATCH 只能有一条：%s", r)
			}
			fallback = strings.TrimSpace(r[len("MATCH,"):])
			continue
		}
		rules = append(rules, r)
	}

	protected, err := protectedRules(in.Services, groupByName)
	if err != nil {
		return Result{}, err
	}
	rules = append(rules, protected...)

	// 内核下载 geo 数据也走自身规则；固定直连，否则节点不可用时下载失败，依赖 ASN 的严格规则会被跳过。
	rules = append(rules, "DOMAIN,"+mirrorHost(in.Mirror)+",DIRECT")
	ruleSets := in.RuleSets
	if ruleSets == nil {
		ruleSets = DefaultRuleSets()
	}

	mode := in.Mode
	if mode == "" {
		mode = "rule"
	}
	// 内核自身的 DNS 查询不经过规则，不指定代理组就从物理网卡直连上游。
	dnsProxy := dnsProxyGroup
	if mode == "direct" {
		dnsProxy = ""
	} else if _, ok := groupByName[dnsProxyGroup]; !ok {
		dnsProxy = ""
		warnings = append(warnings, "代理组「"+dnsProxyGroup+"」不存在，境外 DNS 查询会直连上游")
	}
	setRules, providers := ruleSetRules(ruleSets, targetExists, dnsProxy, in.Mirror)
	rules = append(rules, setRules...)
	switch _, ok := groupByName["漏网之鱼"]; {
	case fallback != "" && targetExists(fallback):
		rules = append(rules, "MATCH,"+fallback)
	case fallback != "":
		warnings = append(warnings, "兜底规则的目标「"+fallback+"」不存在，改用默认兜底")
		fallthrough
	case ok:
		if ok {
			rules = append(rules, "MATCH,漏网之鱼")
		} else {
			rules = append(rules, "MATCH,DIRECT")
		}
	default:
		rules = append(rules, "MATCH,DIRECT")
	}
	cfg := map[string]any{
		"mixed-port":          in.MixedPort,
		"allow-lan":           false,
		"bind-address":        "127.0.0.1",
		"mode":                mode,
		"log-level":           "warning",
		"ipv6":                true,
		"external-controller": in.Controller,
		"secret":              in.Secret,
		"unified-delay":       true,
		"tcp-concurrent":      true,
		"find-process-mode":   "strict",
		"geodata-mode":        true,
		"geox-url": map[string]string{
			"geoip":   MirrorURL(geoBase+"geoip-lite.dat", in.Mirror),
			"geosite": MirrorURL(geoBase+"geosite.dat", in.Mirror),
			"mmdb":    MirrorURL(geoBase+"country-lite.mmdb", in.Mirror),
			"asn":     MirrorURL(geoBase+"GeoLite2-ASN.mmdb", in.Mirror),
		},
		"profile": map[string]bool{"store-selected": true},
		// 应用拿到的是真实 IP：DNS 映射查不到域名时靠嗅探还原，否则受保护的域名会落到兜底直连。
		"sniffer": map[string]any{
			"enable":            true,
			"force-dns-mapping": true,
			"parse-pure-ip":     true,
			"sniff": map[string]any{
				"TLS":  map[string]any{"ports": []any{443, 8443}},
				"QUIC": map[string]any{"ports": []any{443, 8443}},
				"HTTP": map[string]any{"ports": []any{80, "8080-8880"}, "override-destination": true},
			},
		},
		"tun":            tunConfig(in),
		"dns":            dnsConfig(in.DNSListen, proxies, dnsProxy, in.LANDNS, in.LocalDNS),
		"proxies":        proxies,
		"proxy-groups":   proxyGroups,
		"rule-providers": providers,
		"rules":          rules,
	}
	if in.AllowLAN && len(in.LANAllowedIPs) > 0 {
		// 监听所有地址：面板自己经 127.0.0.1 用代理端口，不能只绑 LAN 地址。
		// 来源限定在局域网网段；mihomo 不会自动放行回环，本机和只监听回环的入口要显式加上。
		cfg["allow-lan"] = true
		cfg["bind-address"] = "*"
		cfg["lan-allowed-ips"] = append(append([]string{}, in.LANAllowedIPs...), "127.0.0.0/8", "::1/128")
	}
	if len(in.SubRules) > 0 {
		sub := map[string][]string{}
		for name, list := range in.SubRules {
			var kept []string
			for _, r := range list {
				if t := ruleTargetOf(r); targetExists(t) {
					kept = append(kept, r)
				} else {
					warnings = append(warnings, "子规则「"+name+"」的出口「"+t+"」不存在，已跳过："+r)
				}
			}
			sub[name] = kept
		}
		cfg["sub-rules"] = sub
	}
	probes := map[string]int{}
	routes := map[string]int{}
	gateDirect := 0
	var boxRoutes map[string]string
	if in.ProbePortBase > 0 {
		var listeners []map[string]any
		for _, g := range in.Groups {
			if !g.Strict {
				continue
			}
			port := in.ProbePortBase + len(probes)
			listeners = append(listeners, map[string]any{
				"name": fmt.Sprintf("probe-%d", len(probes)), "type": "http",
				"listen": "127.0.0.1", "port": port, "proxy": g.Name,
			})
			probes[g.Name] = port
		}
		// 线路探测入口：每个代理组一个，外加 DIRECT。
		routeTargets := []string{"DIRECT"}
		for _, g := range in.Groups {
			routeTargets = append(routeTargets, g.Name)
		}
		for i, name := range routeTargets {
			port := in.ProbePortBase + RouteProbeOffset + i
			routes[name] = port
			listeners = append(listeners, map[string]any{
				"name": fmt.Sprintf("route-%d", i), "type": "http", "listen": "127.0.0.1", "port": port, "proxy": name,
			})
		}
		if in.Gate != nil {
			retarget := map[string]string{}
			directVia := "DIRECT"
			if b := in.Box; b != nil {
				// 在路由盒网络上（ADR-084）：除 DIRECT 外的规则目标（代理组、节点、受保护服务、REJECT）都经路由盒的
				// 代理入口，按路由盒的策略走；本机只决定哪些直连。出口探测的 DIRECT 入口也经它，看到的是路由盒的出口。
				proxies = append(proxies, map[string]any{
					"name": BoxProxyName, "type": "socks5", "server": b.Server, "port": b.Port,
					"username": b.User, "password": b.Password, "udp": true,
				})
				cfg["proxies"] = proxies
				for _, r := range rules {
					if t := ruleTargetOf(r); t != "DIRECT" && t != "PASS" {
						retarget[t] = BoxProxyName
					}
				}
				directVia = BoxProxyName
			} else {
				gated := gateGroups(in.Services, groupByName)
				for _, g := range gated {
					proxies = append(proxies, map[string]any{
						"name": GateProxyName(g), "type": "socks5", "server": "127.0.0.1", "port": in.Gate.Port,
						"username": g, "password": in.Gate.Secret, "udp": false,
					})
					retarget[g] = GateProxyName(g)
				}
				cfg["proxies"] = proxies
				// 其余规则目标（代理组、节点）包一层「路由盒切换」选择组：在受信 Wi-Fi 上切到 DIRECT。严格组不包：
				// 没有受保护服务的严格组也不能未经确认就直连，始终经本机代理。
				boxRoutes = map[string]string{}
				for _, r := range rules {
					t := ruleTargetOf(r)
					if _, done := retarget[t]; done || t == "DIRECT" || t == "REJECT" || t == "REJECT-DROP" || t == "PASS" {
						continue
					}
					if g, ok := groupByName[t]; (ok && !g.Strict) || (!ok && nodeNames[t]) {
						retarget[t] = BoxRouteName(t)
						boxRoutes[BoxRouteName(t)] = t
					}
				}
				names := make([]string, 0, len(boxRoutes))
				for name := range boxRoutes {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					proxyGroups = append(proxyGroups, map[string]any{"name": name, "type": "select", "proxies": []string{boxRoutes[name], "DIRECT"}})
				}
				cfg["proxy-groups"] = proxyGroups
			}
			cfg["rules"] = retargetRules(rules, retarget)
			gateDirect = in.Gate.DirectPort
			listeners = append(listeners, map[string]any{
				"name": GateDirectListener, "type": "http", "listen": "127.0.0.1", "port": gateDirect, "proxy": directVia,
			})
		}
		if len(listeners) > 0 {
			cfg["listeners"] = listeners
		}
	}
	if in.HoldRules {
		if err := addHoldRules(cfg, groupByName, nodeNames); err != nil {
			warnings = append(warnings, err.Error())
		}
	}
	if p := in.ClientProxy; p != nil {
		ls, _ := cfg["listeners"].([]map[string]any)
		cfg["listeners"] = append(ls, map[string]any{
			"name": ClientListener, "type": "mixed", "listen": p.Server, "port": p.Port, "udp": true,
			"users": []map[string]string{{"username": p.User, "password": p.Password}},
		})
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return Result{}, err
	}
	return Result{YAML: out, Warnings: warnings, Probes: probes, Routes: routes, GateDirect: gateDirect, BoxRoutes: boxRoutes}, nil
}

// ClientListener 是路由盒给客户端的代理入口（ADR-084）在内核里的监听名；连接的 inboundName 是它时，
// 来源就是经入口接入的 Banbu Connect 客户端（设备页据此显示）。
const ClientListener = "box-clients"

// HoldProxyName 是 hold 副本规则指向的拒绝出站；SUB-RULE 的副本指向「HoldProxyName·子规则名」的子规则集。
const HoldProxyName = "严格线路阻断"

// IsHoldTarget 报告内核规则的目标是不是 hold 副本（面板按它在 GET /rules 里找出要开关的规则）。
func IsHoldTarget(t string) bool {
	return t == HoldProxyName || strings.HasPrefix(t, HoldProxyName+"·")
}

// addHoldRules 在每条指向严格组的规则前插入 hold 副本：顺序不变，副本只是把目标换成拒绝，
// 所以打开副本的效果正好是「原本走严格组的连接被拒绝，其余不变」。
func addHoldRules(cfg map[string]any, groupByName map[string]groups.Export, nodeNames map[string]bool) error {
	strict := map[string]bool{}
	for name, g := range groupByName {
		if g.Strict {
			strict[name] = true
		}
	}
	if len(strict) == 0 {
		return nil
	}
	if _, ok := groupByName[HoldProxyName]; ok || nodeNames[HoldProxyName] {
		return fmt.Errorf("已有名为「%s」的节点或代理组，严格线路阻断时内核里的拒绝规则没有生成", HoldProxyName)
	}
	sub, _ := cfg["sub-rules"].(map[string][]string)
	holdSub := map[string]bool{}
	names := make([]string, 0, len(sub))
	for name := range sub {
		names = append(names, name)
	}
	for _, name := range names {
		list := sub[name]
		var copyList []string
		hit := false
		for _, r := range list {
			if strict[ruleTargetOf(r)] {
				r, hit = withTarget(r, HoldProxyName), true
			}
			copyList = append(copyList, r)
		}
		if hit {
			sub[HoldProxyName+"·"+name] = copyList
			holdSub[name] = true
		}
	}
	rules, _ := cfg["rules"].([]string)
	out := make([]string, 0, len(rules)*2)
	added := false
	for _, r := range rules {
		t := ruleTargetOf(r)
		switch {
		case strings.HasPrefix(r, "SUB-RULE,") && holdSub[t]:
			out = append(out, withTarget(r, HoldProxyName+"·"+t))
			added = true
		case strict[t]:
			out = append(out, withTarget(r, HoldProxyName))
			added = true
		}
		out = append(out, r)
	}
	if !added {
		return nil
	}
	cfg["rules"] = out
	proxies, _ := cfg["proxies"].([]map[string]any)
	cfg["proxies"] = append(proxies, map[string]any{"name": HoldProxyName, "type": "reject"})
	return nil
}

// withTarget 把规则的目标换成 t，保留条件和末尾的 no-resolve、src 参数。
func withTarget(r, t string) string {
	if i := strings.LastIndex(r, "),"); i >= 0 {
		head, rest := r[:i+2], r[i+2:]
		if _, params, ok := strings.Cut(rest, ","); ok {
			return head + t + "," + params
		}
		return head + t
	}
	parts := strings.Split(r, ",")
	idx := len(parts) - 1
	for idx > 1 && (parts[idx] == "no-resolve" || parts[idx] == "src") {
		idx--
	}
	parts[idx] = t
	return strings.Join(parts, ",")
}

// ruleTargetOf 返回规则的目标：最后一段，逻辑规则和 SUB-RULE 取最后一个 ")," 之后；末尾的 no-resolve、src 不算。
func ruleTargetOf(r string) string {
	if i := strings.LastIndex(r, "),"); i >= 0 {
		r = r[i+2:]
		t, _, _ := strings.Cut(r, ",")
		return strings.TrimSpace(t)
	}
	parts := strings.Split(r, ",")
	for len(parts) > 2 && (parts[len(parts)-1] == "no-resolve" || parts[len(parts)-1] == "src") {
		parts = parts[:len(parts)-1]
	}
	return strings.TrimSpace(parts[len(parts)-1])
}

// gateGroups 是受保护服务用到的严格组，排序后返回。
func gateGroups(services []killswitch.Service, groupByName map[string]groups.Export) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range services {
		if g, ok := groupByName[s.Group]; ok && g.Strict && len(s.Suffixes) > 0 && !seen[s.Group] {
			seen[s.Group] = true
			out = append(out, s.Group)
		}
	}
	sort.Strings(out)
	return out
}

// retargetRules 把规则的目标按 to 改指向新的出站（闸门出站、路由盒切换组）。规则的目标是最后一段，
// 带 no-resolve、src 选项时是倒数第二段。
func retargetRules(rules []string, to map[string]string) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		parts := strings.Split(r, ",")
		t := len(parts) - 1
		for t > 0 && (parts[t] == "no-resolve" || parts[t] == "src") {
			t--
		}
		if n, ok := to[parts[t]]; t > 0 && ok {
			parts[t] = n
			r = strings.Join(parts, ",")
		}
		out[i] = r
	}
	return out
}

func tunConfig(in Input) map[string]any {
	if in.Router {
		// 路由盒：stack system 由内核协议栈处理，性能好于 gVisor；auto-redirect 用 nftables
		// 把转发的 TCP 重定向进内核，比策略路由少一次 TUN 往返。TODO(verify:R1) iStoreOS 实机确认。
		tun := map[string]any{
			"enable":                true,
			"device":                RouterTUNDevice,
			"stack":                 "system",
			"auto-route":            true,
			"auto-redirect":         true,
			"auto-detect-interface": true,
			"dns-hijack":            []string{},
		}
		if len(in.RouteExclude) > 0 {
			tun["route-exclude-address"] = in.RouteExclude
		}
		if len(in.ExcludeMAC) > 0 {
			tun["exclude-mac-address"] = in.ExcludeMAC
		}
		return tun
	}
	return map[string]any{
		"enable":                in.TUN,
		"stack":                 "mixed",
		"auto-route":            true,
		"auto-detect-interface": true,
		"dns-hijack":            []string{"any:53"},
		// macOS 上默认不接管系统 DNS，局域网内的 DNS 服务器走网卡直连路由，到不了 TUN。
		"dns-mode": "hijack",
		// Windows 会同时向各网卡的 DNS 查询，靠它拦截物理网卡上的 53 端口。macOS 上无效。
		"strict-route": in.StrictRoute,
	}
}

// protectedRules 为受保护的服务生成规则：QUIC 阻断在前，否则永远匹配不到。
// 未启用 Kill Switch 的服务同样走严格组：路由选择和故障保护是两回事。
func protectedRules(services []killswitch.Service, groupByName map[string]groups.Export) ([]string, error) {
	var rules []string
	seen := map[string]bool{}
	for _, s := range services {
		if len(s.Suffixes) == 0 {
			continue
		}
		g, ok := groupByName[s.Group]
		if !ok {
			return nil, fmt.Errorf("受保护服务「%s」的代理组「%s」不存在", s.Name, s.Group)
		}
		if !g.Strict {
			return nil, fmt.Errorf("受保护服务「%s」的代理组「%s」没有标记为严格，它可能走到直连", s.Name, s.Group)
		}
		suffixes := append([]string(nil), s.Suffixes...)
		sort.Strings(suffixes)
		for _, suffix := range suffixes {
			if seen[suffix] {
				return nil, fmt.Errorf("域名后缀「%s」出现在多个受保护的服务里", suffix)
			}
			seen[suffix] = true
			rules = append(rules,
				"AND,((DOMAIN-SUFFIX,"+suffix+"),(NETWORK,udp)),REJECT",
				"DOMAIN-SUFFIX,"+suffix+","+s.Group)
		}
	}
	return rules, nil
}

func groupMapping(g groups.Export) map[string]any {
	m := map[string]any{"name": g.Name, "type": g.Type, "proxies": g.Members}
	o := g.Options
	if g.Type != groups.TypeSelect {
		m["url"] = o.URL
		m["interval"] = o.Interval
		if o.Timeout > 0 {
			m["timeout"] = o.Timeout
		}
		if o.MaxFailedTimes > 0 {
			m["max-failed-times"] = o.MaxFailedTimes
		}
		if o.Lazy {
			m["lazy"] = true
		}
	}
	if g.Type == groups.TypeURLTest {
		m["tolerance"] = o.Tolerance
	}
	if g.Type == groups.TypeLoadBalance {
		m["strategy"] = o.Strategy
	}
	if o.Hidden {
		m["hidden"] = true
	}
	return m
}

// lanDNS 不为空时（只有桌面端）国内组再加服务的局域网 DNS 转发口：路由盒网络会拦公共 DoH，这时靠
// 路由盒自己的 DNS（它本身防泄漏）。同组上游并发查询、取第一个成功的结果，不拖慢查询；不在受信网络上时
// 转发口立即拒绝，由 DoH 作答。不用 Mihomo 的 system：TUN 起来后 /etc/resolv.conf 只剩 TUN 地址（BXC-D0-002）。
// LocalSuffixes 是只在内网存在的域名后缀（路由器、上级本地 DNS 里的自定义记录），以及私有地址的反向解析。
// 它们只交给 LocalDNS，不和公共 DNS 并发：公共 DNS 的 NXDOMAIN 可能先到，自定义记录就丢了。
var LocalSuffixes = func() []string {
	out := []string{"+.lan", "+.local", "+.localdomain", "+.home", "+.home.arpa", "+.internal",
		"+.10.in-addr.arpa", "+.168.192.in-addr.arpa"}
	for i := 16; i <= 31; i++ {
		out = append(out, fmt.Sprintf("+.%d.172.in-addr.arpa", i))
	}
	return out
}()

func dnsConfig(listen string, proxies []map[string]any, proxyGroup string, lanDNS string, localDNS []string) map[string]any {
	cn := []string{"https://223.5.5.5/dns-query", "https://1.12.12.12/dns-query"}
	if lanDNS != "" {
		cn = append(cn, "udp://"+lanDNS)
	}
	trust := []string{"tls://1.1.1.1:853", "https://1.1.1.1/dns-query"} // TODO(verify:V6) 无软路由环境需复核
	if proxyGroup != "" {
		for i := range trust {
			trust[i] += "#" + proxyGroup
		}
	}
	policy := map[string]any{"geosite:cn": cn, "geosite:geolocation-!cn": trust}
	if len(localDNS) > 0 {
		for _, sfx := range LocalSuffixes {
			policy[sfx] = localDNS
		}
	}
	// 域名形式的节点地址走国内 DNS；IP 形式不需要解析。
	for _, p := range proxies {
		if server, _ := p["server"].(string); server != "" && net.ParseIP(server) == nil {
			policy[server] = cn
		}
	}
	return map[string]any{
		"enable":        true,
		"listen":        listen,
		"enhanced-mode": "redir-host",
		"prefer-h3":     false,
		// 只用来解析域名形式的上游。用 DoH（443）而不是明文 53：DNS 防泄漏会拦物理网卡上的 53 端口。
		"default-nameserver": []string{"https://223.5.5.5/dns-query", "https://1.12.12.12/dns-query"},
		"nameserver":         trust,
		"nameserver-policy":  policy,
	}
}
