package trust

import (
	"context"
	"net"
	"strings"
)

// Network 是当前承载出站流量的网络。只看默认路由所在的网卡：
// 同时连着有线和 Wi-Fi 时，受信 Wi-Fi 不代表流量真的从它出去。
type Network struct {
	Interface string `json:"interface"` // 默认路由所在的网卡，空表示没有网络
	WiFi      bool   `json:"wifi"`
	SSID      string `json:"ssid"`
	// Hint 说明 SSID 为什么读不到（系统隐藏、缺少定位权限等）。
	Hint string `json:"hint,omitempty"`
	// Router 是默认网关的标识「IP/MAC」（读不到 MAC 时只有 IP，读不到网关时为空），见 RouterDetector。
	// 换到同名 Wi-Fi、同网段的另一台路由器时网卡、地址、SSID 都可能不变，要靠网关的 MAC 识别。
	Router string `json:"router,omitempty"`
}

func (n Network) fingerprint() string { return n.Interface + "\x00" + n.SSID }

// RouterDetector 能读网卡默认网关的标识（Network.Router）。可能起子进程或发一次 ARP 请求，只在定时检查里调用。
type RouterDetector interface {
	Router(ctx context.Context, iface string) string
}

// routerChanged 判断网关是否换了：IP 都读到且不同，或者 IP 相同、MAC 都读到且不同。
// 邻居表的条目会过期、刚连上时可能还没有，一边读不到 MAC 不算变化（否则会周期性地误判换网络）。
func routerChanged(prev, cur string) bool {
	if prev == "" || cur == "" {
		return false
	}
	pip, pmac, _ := strings.Cut(prev, "/")
	cip, cmac, _ := strings.Cut(cur, "/")
	return pip != cip || pmac != "" && cmac != "" && pmac != cmac
}

// keepRouterMAC 在这次读不到 MAC、网关 IP 没变时沿用上次的 MAC，让之后的比较仍有依据。
func keepRouterMAC(prev, cur string) string {
	pip, pmac, _ := strings.Cut(prev, "/")
	if cip, cmac, _ := strings.Cut(cur, "/"); cmac == "" && pmac != "" && cip == pip {
		return prev
	}
	return cur
}

type Detector interface {
	Detect(ctx context.Context) Network
}

// InterfaceDetector 能读指定网卡的网络信息。TUN 接管时用它看物理网卡（由 Watcher 给出）。
type InterfaceDetector interface {
	DetectOn(ctx context.Context, iface string) Network
}

// defaultInterface 问路由表：UDP “连接”不发送任何数据，只选出口地址。
// 开着别的 VPN 时这里是它的虚拟网卡，会被当成不是 Wi-Fi 而阻断，这是有意的。
func defaultInterface() string {
	c, err := net.Dial("udp4", "1.1.1.1:443")
	if err != nil {
		return ""
	}
	defer c.Close()
	local := c.LocalAddr().(*net.UDPAddr).IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
				return i.Name
			}
		}
	}
	return ""
}

// ---- 平台无关的解析，便于在任意平台测试 ----

// parseWiFiDevices 从 networksetup -listallhardwareports 取 Wi-Fi 网卡。
func parseWiFiDevices(out string) []string {
	var devs []string
	wifi := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if port, ok := strings.CutPrefix(line, "Hardware Port: "); ok {
			wifi = port == "Wi-Fi" || port == "AirPort"
			continue
		}
		if dev, ok := strings.CutPrefix(line, "Device: "); ok && wifi {
			devs = append(devs, dev)
			wifi = false
		}
	}
	return devs
}

const redacted = "<redacted>"

// parseIPConfigSSID 从 ipconfig getsummary 取 SSID。macOS 15 起没有权限时显示 <redacted>。
func parseIPConfigSSID(out string) (ssid string, hidden bool) {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, " : ")
		if !ok || strings.TrimSpace(k) != "SSID" {
			continue
		}
		v = strings.TrimRight(v, "\r")
		if v == redacted {
			return "", true
		}
		return v, false
	}
	return "", false
}

// parseAirportNetwork 解析 networksetup -getairportnetwork 的输出。
func parseAirportNetwork(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, p := range []string{"Current Wi-Fi Network: ", "Current AirPort Network: "} {
			if v, ok := strings.CutPrefix(line, p); ok && v != redacted {
				return v
			}
		}
	}
	return ""
}

// wlanInterface 是 netsh wlan show interfaces 里的一块。
type wlanInterface struct {
	Name, SSID string
}

// parseNetshInterfaces 解析 netsh wlan show interfaces。键名随系统语言变化，SSID 不翻译。
func parseNetshInterfaces(out string) []wlanInterface {
	var list []wlanInterface
	var cur *wlanInterface
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "Name", "名称", "名稱":
			list = append(list, wlanInterface{Name: v})
			cur = &list[len(list)-1]
		case "SSID":
			if cur != nil {
				cur.SSID = v
			}
		}
	}
	return list
}
