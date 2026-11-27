package trust

import (
	"context"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// macOS 15 起，没有定位权限的进程读到的 SSID 是 <redacted>。服务以 root 运行，
// 启动时执行 ipconfig setverbose 1 让 getsummary 显示真实名称。
//
// TODO(verify:V24) macOS 26 上 root 服务执行 setverbose 1 后能否读到 SSID；不行的话 Wi-Fi 永远不受信，始终阻断。
type systemDetector struct {
	mu      sync.Mutex
	devs    []string
	devsAt  time.Time
	verbose sync.Once
}

func NewSystemDetector() Detector { return &systemDetector{} }

func output(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	return string(out)
}

// wifiDevices 缓存一分钟：网卡列表很少变，networksetup 又比较慢。
func (d *systemDetector) wifiDevices(ctx context.Context) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.devs == nil || time.Since(d.devsAt) > time.Minute {
		d.devs = parseWiFiDevices(output(ctx, "/usr/sbin/networksetup", "-listallhardwareports"))
		d.devsAt = time.Now()
	}
	return d.devs
}

func (d *systemDetector) Detect(ctx context.Context) Network {
	return d.DetectOn(ctx, defaultInterface())
}

// DetectOn 读指定网卡的网络信息：本机内核的 TUN 接管时，出口网卡是 TUN，要看它底下的物理网卡。
func (d *systemDetector) DetectOn(ctx context.Context, iface string) Network {
	d.verbose.Do(func() { _ = exec.CommandContext(ctx, "/usr/sbin/ipconfig", "setverbose", "1").Run() })
	n := Network{Interface: iface}
	if n.Interface == "" || !slices.Contains(d.wifiDevices(ctx), n.Interface) {
		return n
	}
	n.WiFi = true
	ssid, hidden := parseIPConfigSSID(output(ctx, "/usr/sbin/ipconfig", "getsummary", n.Interface))
	if ssid == "" {
		ssid = parseAirportNetwork(output(ctx, "/usr/sbin/networksetup", "-getairportnetwork", n.Interface))
	}
	n.SSID = ssid
	if ssid == "" {
		if hidden {
			n.Hint = "系统隐藏了 Wi-Fi 名称（需要 root 执行 ipconfig setverbose 1）"
		} else {
			n.Hint = "读不到 Wi-Fi 名称"
		}
	}
	return n
}
