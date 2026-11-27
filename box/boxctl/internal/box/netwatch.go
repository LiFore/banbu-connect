package box

import (
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"boxctl/internal/guard"
)

// 网络变化时先阻断再放通：WAN 重拨、换了上联、默认路由或 WAN 地址变了，严格线路的出口
// 就不能再算已确认。每秒看一次默认路由和出口网卡的地址，一变就作废严格线路的探测结果：
// 管理服务下发了允许的出口时，重新探测成功之前按「正在探测」阻断（内核里和 guard 都阻断），
// 探测失败也继续阻断（trust.StrictChecker.Invalidate 之后只有成功的探测才能解除）。

const netwatchPeriod = time.Second

// 网络刚变化时可能还没通：作废后隔几秒再探测几次，网络一恢复就尽快放通，不等阻断时的探测间隔。
var netwatchFollowUps = []time.Duration{3 * time.Second, 6 * time.Second, 10 * time.Second}

func (b *Box) watchNetwork() {
	skip := func(dev string) bool {
		return dev == "lo" || dev == guard.TUNDevice || slices.Contains(guard.LANDevices(b.opts.LANDevice), dev)
	}
	last := currentFingerprint(skip)
	b.netMu.Lock()
	b.netFP = last
	b.netMu.Unlock()
	t := time.NewTicker(netwatchPeriod)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		fp := currentFingerprint(skip)
		if fp == last {
			continue
		}
		b.log.Warn("网络变化，严格线路重新确认出口前先阻断", "before", last, "after", fp)
		last = fp
		b.netMu.Lock()
		b.netFP, b.netAt = fp, time.Now().UTC()
		b.netChanges++
		b.netMu.Unlock()
		b.strict.Invalidate()
		for _, d := range netwatchFollowUps {
			time.AfterFunc(d, b.strict.Kick)
		}
	}
}

func currentFingerprint(skip func(string) bool) string {
	r4, _ := os.ReadFile("/proc/net/route")
	r6, _ := os.ReadFile("/proc/net/ipv6_route")
	return netFingerprint(string(r4), string(r6), interfaceAddrs, skip)
}

// netFingerprint 是默认路由（网卡、网关）和这些网卡上全局地址的摘要。读不到时为空（非 Linux）。
// 只看主路由表：内核的 TUN 用自己的路由表，局域网网桥和 TUN 跳过，不会因为内核启停误判。
func netFingerprint(route4, route6 string, addrs func(string) []string, skip func(string) bool) string {
	var parts []string
	devs := map[string]bool{}
	for i, line := range strings.Split(route4, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 || skip(f[0]) {
			continue
		}
		if f[1] == "00000000" && f[7] == "00000000" {
			parts = append(parts, "v4:"+f[0]+":"+f[2])
			devs[f[0]] = true
		}
	}
	for _, line := range strings.Split(route6, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || skip(f[9]) {
			continue
		}
		if strings.Trim(f[0], "0") == "" && f[1] == "00" {
			parts = append(parts, "v6:"+f[9]+":"+f[4])
			devs[f[9]] = true
		}
	}
	for dev := range devs {
		for _, a := range addrs(dev) {
			parts = append(parts, "addr:"+dev+":"+a)
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}

func interfaceAddrs(dev string) []string {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return nil
	}
	list, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range list {
		ipn, ok := a.(*net.IPNet)
		if ok && ipn.IP.IsGlobalUnicast() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
