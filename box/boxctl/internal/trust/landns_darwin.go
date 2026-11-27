package trust

import (
	"context"
	"net"
	"net/netip"
	"syscall"
)

// InterfaceDNS 读某块网卡自己的 DNS 服务器（scutil --dns 的「scoped queries」部分），
// 不受 TUN 改写默认解析器的影响。
func InterfaceDNS(iface string) []netip.Addr {
	return parseScopedDNS(output(context.Background(), "/usr/sbin/scutil", "--dns"), iface)
}

// bindInterface 让套接字只从网卡 iface 发出（IP_BOUND_IF / IPV6_BOUND_IF），不按路由表进 TUN。
func bindInterface(iface string, v6 bool) func(network, address string, c syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(_, _ string, c syscall.RawConn) error {
		ifi, err := net.InterfaceByName(iface)
		if err != nil {
			return err
		}
		var serr error
		err = c.Control(func(fd uintptr) {
			if v6 {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF, ifi.Index)
			} else {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, ifi.Index)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
}
