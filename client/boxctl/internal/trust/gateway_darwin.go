package trust

import (
	"context"
	"net"
	"net/netip"
	"regexp"
	"syscall"

	"golang.org/x/net/route"
)

// Router 返回 iface 上默认网关的标识「IP/MAC」（见 Network.Router），读不到 MAC 时只有 IP。
// 网关 IP 读内核路由表；MAC 用 arp -n 查邻居表：IPv4 的邻居条目经 sysctl 读不到（实测 macOS 27 上
// NET_RT_FLAGS + RTF_LLINFO 对 AF_INET 返回空，系统自带的 arp 能读），每次检查起一个子进程，代价同 ipconfig。
func (d *systemDetector) Router(ctx context.Context, iface string) string {
	gw := defaultGateway(iface)
	if !gw.IsValid() {
		return ""
	}
	m := arpMAC.FindStringSubmatch(output(ctx, "/usr/sbin/arp", "-n", gw.String()))
	if m == nil {
		return gw.String()
	}
	if hw, err := net.ParseMAC(padMAC(m[1])); err == nil {
		return gw.String() + "/" + hw.String()
	}
	return gw.String()
}

// arp -n 的输出：「? (192.168.3.1) at f8:ce:21:e5:4c:b9 on en0 ifscope [ethernet]」，没有条目时是「(incomplete)」或「no entry」。
var arpMAC = regexp.MustCompile(`\) at ([0-9a-fA-F]{1,2}(?::[0-9a-fA-F]{1,2}){5}) `)

// padMAC 把 arp 输出里省略前导零的「0:e2:6c:68:32:23」补成两位一组，net.ParseMAC 才认。
func padMAC(s string) string {
	b := make([]byte, 0, 17)
	part := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ':' {
			if part == 1 {
				b = append(b[:len(b)-1], '0', s[i-1])
			}
			if i < len(s) {
				b = append(b, ':')
			}
			part = 0
			continue
		}
		b = append(b, s[i])
		part++
	}
	return string(b)
}

// defaultGateway 读内核路由表里 iface 上 IPv4 默认路由的网关，不起子进程。读不到时无效。
func defaultGateway(iface string) netip.Addr {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return netip.Addr{}
	}
	b, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return netip.Addr{}
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, b)
	if err != nil {
		return netip.Addr{}
	}
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Index != ifi.Index || rm.Flags&syscall.RTF_GATEWAY == 0 || len(rm.Addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		if dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr); !ok || dst.IP != [4]byte{} {
			continue
		}
		if len(rm.Addrs) > syscall.RTAX_NETMASK {
			if mask, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); ok && mask.IP != [4]byte{} {
				continue
			}
		}
		if g, ok := rm.Addrs[syscall.RTAX_GATEWAY].(*route.Inet4Addr); ok {
			return netip.AddrFrom4(g.IP)
		}
	}
	return netip.Addr{}
}
