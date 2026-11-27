//go:build darwin || windows

package trust

import (
	"net"
	"slices"
	"strings"

	tun "github.com/metacubex/sing-tun"
	"github.com/metacubex/sing/common/control"
	"github.com/metacubex/sing/common/logger"
)

// Watcher 订阅系统的路由变化（macOS 路由套接字、Windows NotifyRouteChange2），
// 换网络后立即通知，不等下一次定时检查。它还给出 0.0.0.0/0 默认路由所在的物理网卡：
// 本机内核的 TUN 用拆分路由（0/1、128/1）接管流量，默认路由仍在物理网卡上。
type Watcher struct {
	network tun.NetworkUpdateMonitor
	iface   tun.DefaultInterfaceMonitor
}

// NewWatcher 创建监听，Start 之后才会回调；onChange 在任何路由变化时调用
// （可能很频繁，调用方自己判断是否真的换了网络）。
func NewWatcher(onChange func()) (*Watcher, error) {
	nm, err := tun.NewNetworkUpdateMonitor(logger.NOP())
	if err != nil {
		return nil, err
	}
	im, err := tun.NewDefaultInterfaceMonitor(nm, logger.NOP(), tun.DefaultInterfaceMonitorOptions{
		InterfaceFinder: control.NewDefaultInterfaceFinder(),
	})
	if err != nil {
		return nil, err
	}
	nm.RegisterCallback(onChange)
	// 默认网卡的判断有 1 秒防抖，判断出来后再通知一次，让调用方拿到新网卡。
	im.RegisterCallback(func(*control.Interface, int) { onChange() })
	return &Watcher{network: nm, iface: im}, nil
}

func (w *Watcher) Start() error {
	if err := w.network.Start(); err != nil {
		return err
	}
	if err := w.iface.Start(); err != nil {
		_ = w.network.Close()
		return err
	}
	return nil
}

// Physical 返回默认路由所在的物理网卡，没有时为空。
func (w *Watcher) Physical() string {
	if w == nil {
		return ""
	}
	if i := w.iface.DefaultInterface(); i != nil {
		return i.Name
	}
	return ""
}

func (w *Watcher) Close() {
	if w == nil {
		return
	}
	_ = w.iface.Close()
	_ = w.network.Close()
}

// quickFingerprint 不起子进程、不读 SSID，只看网卡和它的地址，在路由变化的回调里同步计算（微秒级）。
// 换 Wi-Fi、插网线、断网、重新拿到地址都会改变它；内核 TUN 增删路由不会。
func quickFingerprint(physical string) string {
	name := physical
	if name == "" {
		name = defaultInterface()
	}
	if name == "" {
		return ""
	}
	i, err := net.InterfaceByName(name)
	if err != nil {
		return name
	}
	addrs, _ := i.Addrs()
	list := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() {
			list = append(list, n.String())
		}
	}
	slices.Sort(list)
	return name + "|" + strings.Join(list, ",")
}
