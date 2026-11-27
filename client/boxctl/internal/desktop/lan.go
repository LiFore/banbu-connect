package desktop

import (
	"net"
	"strings"

	"boxctl/internal/dist"
)

// lanAddrs 返回本机的局域网地址（私有 IPv4 + 网卡 MAC），随代理连接统计上报：
// 闸门直连时这台电脑的受保护流量经路由盒代理，管理服务据此把路由盒统计到的这部分流量归到本机（BXC-F0-027）。
// 跳过回环、没有 MAC 的虚拟网卡（TUN）。TODO(verify:V-conn-gate) Wi-Fi 私有地址轮换时 MAC 会变。
func lanAddrs() []dist.LANAddr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []dist.LANAddr
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) != 6 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			out = append(out, dist.LANAddr{IP: ipn.IP.String(), MAC: strings.ToLower(ifc.HardwareAddr.String())})
		}
	}
	return out
}
