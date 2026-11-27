package trust

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSendARP = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("SendARP")

// Router 返回 iface 上默认网关的标识「IP/MAC」（见 Network.Router），读不到 MAC 时只有 IP。
// 网关读 GetAdaptersAddresses 的网关列表；MAC 用 SendARP（邻居缓存里有就直接返回，没有会发一次 ARP 请求，
// 最多等一两秒，所以只在定时检查里调用，不在路由变化的回调里）。
//
// TODO(verify:V-router-win) Windows 实机确认：网关和 MAC 能读到，换到同网段、同网关 IP 的另一台路由器时 MAC 变化被识别。
func (d *systemDetector) Router(_ context.Context, iface string) string {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return ""
	}
	gw := adapterGateway(uint32(ifi.Index))
	if !gw.IsValid() {
		return ""
	}
	b := gw.As4()
	dst := *(*uint32)(unsafe.Pointer(&b[0])) // IPAddr 是网络字节序的 4 字节，按内存原样传
	var mac [8]byte
	n := uint32(len(mac))
	if r, _, _ := procSendARP.Call(uintptr(dst), 0, uintptr(unsafe.Pointer(&mac[0])), uintptr(unsafe.Pointer(&n))); r != 0 || n != 6 {
		return gw.String()
	}
	return gw.String() + "/" + net.HardwareAddr(mac[:6]).String()
}

// adapterGateway 返回网卡（按 IfIndex）的第一个 IPv4 网关。
func adapterGateway(index uint32) netip.Addr {
	const flags = windows.GAA_FLAG_INCLUDE_GATEWAYS | windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER
	size := uint32(16 << 10)
	for range 3 {
		buf := make([]byte, size)
		a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0, a, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue
		}
		if err != nil {
			return netip.Addr{}
		}
		for ; a != nil; a = a.Next {
			if a.IfIndex != index {
				continue
			}
			for g := a.FirstGatewayAddress; g != nil; g = g.Next {
				sa, err := g.Address.Sockaddr.Sockaddr()
				if err != nil {
					continue
				}
				if v4, ok := sa.(*syscall.SockaddrInet4); ok {
					return netip.AddrFrom4(v4.Addr)
				}
			}
		}
		return netip.Addr{}
	}
	return netip.Addr{}
}
