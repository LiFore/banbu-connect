package box

import (
	"context"
	"time"

	"boxctl/internal/dist"
)

// decorateConns 给要上报的一批代理连接统计补上来源 IP 对应的局域网设备（BXC-F0-027）：
// DHCP 租约 / 邻居表里的 MAC，备注名优先、没有时用主机名。管理服务按 MAC 识别设备，
// 并把 MAC 与桌面客户端上报的局域网地址相同的流量（闸门直连）归到那台客户端。
func (b *Box) decorateConns(batch *dist.ConnBatch) {
	ips := map[string]bool{}
	for _, m := range batch.Minutes {
		if m.Source != "" {
			ips[m.Source] = true
		}
	}
	for _, r := range batch.Records {
		if r.Source != "" {
			ips[r.Source] = true
		}
	}
	if len(ips) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	devices, err := b.listDevices(ctx)
	if err != nil {
		b.log.Warn("读取局域网设备失败，统计不带设备信息", "err", err)
		return
	}
	for _, d := range devices {
		if d.IP == "" || !ips[d.IP] || d.MAC == "" {
			continue
		}
		name := d.Name
		if name == "" {
			name = d.Hostname
		}
		batch.Sources = append(batch.Sources, dist.ConnSource{IP: d.IP, MAC: d.MAC, Name: name})
		delete(ips, d.IP)
	}
	for ip := range ips {
		batch.Sources = append(batch.Sources, dist.ConnSource{IP: ip})
	}
}
