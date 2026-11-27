package guard

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// Table 是 guard 独占的 nft 表，与 fw4 的 inet fw4 表分开，fw4 reload 不会动它（M9-01）。
	Table = "proxy_guard"
	// TUNDevice 是路由盒内核的 TUN 网卡名，生成器写进配置，规则据此放行进内核的流量。
	TUNDevice = "boxtun"
	// setTimeout 是 dnsmasq 写入的解析结果在集合里保留的时间，须不短于常见 TTL（M10-02）。
	setTimeout    = "6h"
	setTimeoutDur = 6 * time.Hour
	versionTag    = "boxctl:"
)

// Ruleset 生成一份完整的 nft 脚本，用 `nft -f -` 在一个事务里原子生效。
//
// 不删表重建：dnsmasq 动态写进 strict_v4/strict_v6 的地址要保留，否则客户端缓存了
// 解析结果、内核又恰好不在时，这些地址不在集合里会被直连放行。
//
// 被 auto-redirect 接管的 TCP 进的是本机（input），走 TUN 的 UDP 出口是 TUNDevice，
// 都不会被 forward 链里的丢弃规则命中：正常经代理的流量不受影响。
func Ruleset(p Policy, lanDev string) string {
	var b strings.Builder
	t := "inet " + Table
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("add table %s", t)
	w("add set %s strict_v4 { type ipv4_addr; flags timeout; timeout %s; }", t, setTimeout)
	w("add set %s strict_v6 { type ipv6_addr; flags timeout; timeout %s; }", t, setTimeout)
	w("add set %s strict_v4_net { type ipv4_addr; flags interval; auto-merge; }", t)
	w("add set %s strict_v6_net { type ipv6_addr; flags interval; auto-merge; }", t)
	w("flush set %s strict_v4_net", t)
	w("flush set %s strict_v6_net", t)
	if v4 := p.v4(); len(v4) > 0 {
		w("add element %s strict_v4_net { %s }", t, strings.Join(v4, ", "))
	}
	if v6 := p.v6(); len(v6) > 0 {
		w("add element %s strict_v6_net { %s }", t, strings.Join(v6, ", "))
	}

	drops := func(chain string) {
		w("add rule %s %s ip daddr @strict_v4 counter drop", t, chain)
		w("add rule %s %s ip daddr @strict_v4_net counter drop", t, chain)
		w("add rule %s %s ip6 daddr @strict_v6 counter drop", t, chain)
		w("add rule %s %s ip6 daddr @strict_v6_net counter drop", t, chain)
	}

	// forward：局域网直连受保护服务一律丢弃；IPv6 不转发（M9-08）。
	w("add chain %s forward { type filter hook forward priority filter - 5; policy accept; }", t)
	w("flush chain %s forward", t)
	w(`add rule %s forward oifname "%s" accept`, t, TUNDevice)
	w(`add rule %s forward meta nfproto ipv6 counter drop comment "no ipv6 forward"`, t)
	drops("forward")

	// output：本机直连受保护服务丢弃，防止内核 DIRECT 规则或其他程序绕过代理（M9-04 的子集）。
	// 第一条带版本注释，自检时据此判断系统上的规则是不是当前这一份。
	w("add chain %s output { type filter hook output priority filter - 5; policy accept; }", t)
	w("flush chain %s output", t)
	w(`add rule %s output oifname { "lo", "%s" } accept comment "%s%s"`, t, TUNDevice, versionTag, p.Version())
	drops("output")

	in := iifMatch(lanDev)

	// 局域网里手工指定的 DNS 也交给本机 dnsmasq，受保护域名的解析结果才能进集合（M9-10 DNS 绕过）。
	w("add chain %s dns_redirect { type nat hook prerouting priority dstnat - 5; policy accept; }", t)
	w("flush chain %s dns_redirect", t)
	if in != "" {
		w(`add rule %s dns_redirect %smeta l4proto { tcp, udp } th dport 53 fib daddr type != local redirect to :53`, t, in)
	}

	// 加密 DNS（DoT、DoQ、知名 DoH 服务的地址）：绕过 dnsmasq，解析结果进不了严格集合，hold 也拦不住。
	// 受保护服务存在时一律丢弃，客户端（浏览器、Android 私人 DNS 自动模式）会退回普通 DNS。
	// 只拦局域网：内核自己用 DoH 解析（default-nameserver），走的是 output。
	w("add set %s doh_v4_net { type ipv4_addr; flags interval; auto-merge; }", t)
	w("add set %s doh_v6_net { type ipv6_addr; flags interval; auto-merge; }", t)
	w("flush set %s doh_v4_net", t)
	w("flush set %s doh_v6_net", t)
	w("add chain %s encrypted_dns { type filter hook prerouting priority raw - 2; policy accept; }", t)
	w("flush chain %s encrypted_dns", t)
	if len(p.Suffixes) > 0 && in != "" {
		w("add element %s doh_v4_net { %s }", t, strings.Join(dohV4, ", "))
		w("add element %s doh_v6_net { %s }", t, strings.Join(dohV6, ", "))
		w(`add rule %s encrypted_dns %stcp dport 853 counter drop comment "DoT"`, t, in)
		w(`add rule %s encrypted_dns %sudp dport { 853, 8853 } counter drop comment "DoQ"`, t, in)
		w(`add rule %s encrypted_dns %sip daddr @doh_v4_net meta l4proto { tcp, udp } th dport 443 counter drop comment "DoH"`, t, in)
		w(`add rule %s encrypted_dns %sip6 daddr @doh_v6_net meta l4proto { tcp, udp } th dport 443 counter drop comment "DoH"`, t, in)
	}

	// 内核离线或异常（ADR-045）：受保护服务存在时，局域网除了访问路由盒自身和内网之外全部丢弃。
	// 内核不在时没法按域名认出受保护服务的连接（经加密 DNS 拿到的地址、缓存超过集合时效的地址），只能整体阻断。
	w("add chain %s core_down { type filter hook prerouting priority raw - 3; policy accept; }", t)
	w("flush chain %s core_down", t)
	if p.CoreDown && len(p.Suffixes) > 0 && in != "" {
		w("add rule %s core_down %sfib daddr type local accept", t, in)
		w("add rule %s core_down %sip daddr { %s } accept", t, in, privateNets)
		w("add rule %s core_down %sip6 daddr { fc00::/7, fe80::/10, ff00::/8 } accept", t, in)
		w(`add rule %s core_down %scounter drop comment "core down"`, t, in)
	}

	// hold：严格线路出口不符时，在进内核之前丢弃去受保护服务的局域网流量（ADR-026）。
	w("add chain %s hold { type filter hook prerouting priority raw; policy accept; }", t)
	w("flush chain %s hold", t)
	if p.Hold {
		if in != "" {
			w(`add rule %s hold %sip daddr @strict_v4 counter drop`, t, in)
			w(`add rule %s hold %sip daddr @strict_v4_net counter drop`, t, in)
			w(`add rule %s hold %sip6 daddr @strict_v6 counter drop`, t, in)
			w(`add rule %s hold %sip6 daddr @strict_v6_net counter drop`, t, in)
		} else {
			drops("hold")
		}
	}
	renderBlocks(w, t, lanDev, p.Blocks)
	return b.String()
}

// iifMatch 返回匹配局域网入口网卡的条件（带结尾空格）。lanDev 可以是逗号分隔的多个网桥
// （主网络之外还有访客、IoT 等网络时）；为空时返回空，不限定入口（开发环境）。
func iifMatch(lanDev string) string {
	devs := LANDevices(lanDev)
	switch len(devs) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(`iifname "%s" `, devs[0])
	}
	quoted := make([]string, len(devs))
	for i, d := range devs {
		quoted[i] = `"` + d + `"`
	}
	return "iifname { " + strings.Join(quoted, ", ") + " } "
}

var ifnameRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,15}$`)

// LANDevices 拆分逗号分隔的网卡名，去掉空项和重复项；名字不合法时跳过（会写进 nft 规则）。
func LANDevices(lanDev string) []string {
	var out []string
	for _, d := range strings.Split(lanDev, ",") {
		d = strings.TrimSpace(d)
		if d != "" && ifnameRE.MatchString(d) && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// 知名公共 DNS 的地址：这些地址的 443 端口提供 DoH（浏览器、系统可以直接按 IP 使用，不经过 DNS 解析）。
var (
	dohV4 = []string{
		"1.0.0.1", "1.0.0.2", "1.0.0.3", "1.1.1.1", "1.1.1.2", "1.1.1.3", // Cloudflare
		"8.8.4.4", "8.8.8.8", // Google
		"9.9.9.9", "9.9.9.10", "9.9.9.11", "149.112.112.112", "149.112.112.11", // Quad9
		"208.67.220.220", "208.67.222.222", // OpenDNS
		"94.140.14.14", "94.140.15.15", "94.140.14.140", "94.140.14.141", // AdGuard
		"45.90.28.0/24", "45.90.30.0/24", // NextDNS
		"76.76.2.0/24", "76.76.10.0/24", // Control D
		"185.228.168.9", "185.228.169.9", // CleanBrowsing
		"194.242.2.2", "194.242.2.3", // Mullvad
		"185.222.222.222", "45.11.45.11", // DNS.SB
		"101.101.101.101",        // TWNIC
		"223.5.5.5", "223.6.6.6", // 阿里
		"1.12.12.12", "120.53.53.53", // 腾讯 DNSPod
		"101.226.4.6", "218.30.118.6", // 360
	}
	dohV6 = []string{
		"2606:4700:4700::1111", "2606:4700:4700::1001", // Cloudflare
		"2001:4860:4860::8888", "2001:4860:4860::8844", // Google
		"2620:fe::fe", "2620:fe::9", // Quad9
		"2620:119:35::35", "2620:119:53::53", // OpenDNS
		"2a10:50c0::ad1:ff", "2a10:50c0::ad2:ff", // AdGuard
		"2400:3200::1", "2400:3200:baba::1", // 阿里
	}
	// dohHosts 是 DoH/DoT 服务的域名：本机 dnsmasq 对它们返回 NXDOMAIN，按域名配置的加密 DNS 拿不到地址就会退回普通 DNS。
	dohHosts = []string{
		"cloudflare-dns.com", "one.one.one.one", "dns.google", "dns.google.com", "dns.quad9.net",
		"doh.opendns.com", "dns.opendns.com", "dns.adguard.com", "adguard-dns.com", "dns.nextdns.io",
		"dns.controld.com", "freedns.controld.com", "doh.cleanbrowsing.org", "dns.mullvad.net", "doh.mullvad.net",
		"doh.dns.sb", "dns.twnic.tw", "dns.alidns.com", "doh.pub", "dns.pub", "doh.360.cn",
	}
	// canaryHosts 返回 NXDOMAIN 时，Firefox 不再自动启用 DoH，苹果设备不再使用 iCloud 专用代理（Private Relay）。
	// 专用代理把流量交给苹果的中继，出口不是严格线路的出口，内核也看不到真实目标。
	canaryHosts = []string{"use-application-dns.net", "mask.icloud.com", "mask-h2.icloud.com"}
)

// RemoveScript 删除 guard 的表（卸载时用）。
func RemoveScript() string { return "delete table inet " + Table + "\n" }

// rfc6761Local 是 OpenWrt rfc6761.conf 里只在本地应答的后缀，去掉了 local。
var rfc6761Local = []string{"bind", "invalid", "localhost", "onion", "test"}

// LocalDomains 是内网域名后缀，须与 gen.LocalSuffixes 里的域名部分一致（box 包的测试核对）。
var LocalDomains = []string{"lan", "local", "localdomain", "home", "home.arpa", "internal"}

// DnsmasqConf 生成 dnsmasq 片段：受保护域名只向内核 DNS 查询，结果写进严格集合（M10-02）。
func DnsmasqConf(p Policy) string {
	var b strings.Builder
	b.WriteString("# 由 boxctl guard 生成，勿手改\n")
	if p.RFC6761 {
		// 代替 OpenWrt 的 boguspriv（含 rfc6761.conf），只少了 server=/local/（ADR-081）。
		b.WriteString("bogus-priv\n")
		for _, d := range rfc6761Local {
			fmt.Fprintf(&b, "server=/%s/\n", d)
		}
	}
	if len(p.Suffixes) > 0 {
		// 只在本地应答（没有 hosts 记录就是 NXDOMAIN）：加密 DNS 和专用代理会绕过严格集合。
		for _, h := range append(append([]string{}, dohHosts...), canaryHosts...) {
			fmt.Fprintf(&b, "local=/%s/\n", h)
		}
	}
	if p.DNSUpstream == "" {
		return b.String()
	}
	upstream := strings.Replace(p.DNSUpstream, ":", "#", 1)
	if strings.HasPrefix(p.DNSUpstream, "[") {
		// [::1]:1053 → ::1#1053
		host, port, _ := strings.Cut(strings.TrimPrefix(p.DNSUpstream, "["), "]:")
		upstream = host + "#" + port
	}
	for _, s := range p.Suffixes {
		fmt.Fprintf(&b, "server=/%s/%s\n", s, upstream)
		fmt.Fprintf(&b, "nftset=/%s/4#inet#%s#strict_v4,6#inet#%s#strict_v6\n", s, Table, Table)
	}
	if p.ForwardAll {
		// 内核在运行：全部查询交给内核，境外域名由它按规则走代理解析，避免污染。
		fmt.Fprintf(&b, "server=%s\nno-resolv\n", upstream)
		// 内网域名由内核转问上级本地 DNS（ADR-079），答案是私有地址；OpenWrt 默认开着 rebind_protection，
		// 会把上游（这里是内核）给的私有地址答案丢掉，gitlab.xxx.local 之类就查不到。
		fmt.Fprintf(&b, "rebind-domain-ok=/%s/\n", strings.Join(LocalDomains, "/"))
	}
	return b.String()
}
