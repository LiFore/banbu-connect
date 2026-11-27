package box

import (
	"bufio"
	"context"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"
)

// 上级本地 DNS：内核运行时 dnsmasq 用 no-resolv 把查询全部交给内核，不再问 WAN 口拿到的 DNS，
// 上级路由器或公司 DNS 里的内网记录（xxx.lan、xxx.internal 等）就查不到了。
// 内核的内网域名（gen.LocalSuffixes）改问 WAN 口的 DNS，只取私有地址：公网 DNS 上不会有内网记录，
// 也不把内网域名发给运营商。WAN 口的 DNS 由 netifd 写在 resolv.conf.auto 里，每 syncPeriod 检查一次，变了就重建配置。

// resolvAutoPaths 依次尝试：OpenWrt 21.02 起在 resolv.conf.d 下，更早的版本在 /tmp 下。
var resolvAutoPaths = []string{"/tmp/resolv.conf.d/resolv.conf.auto", "/tmp/resolv.conf.auto"}

// parseUpstreamDNS 从 resolv.conf 格式的内容里取私有地址的 nameserver，转成内核的上游写法（去重、保持顺序）。
func parseUpstreamDNS(content string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			continue
		}
		a = a.Unmap()
		if !a.IsPrivate() { // RFC 1918 和 IPv6 ULA
			continue
		}
		up := "udp://" + netip.AddrPortFrom(a, 53).String()
		if !slices.Contains(out, up) {
			out = append(out, up)
		}
	}
	return out
}

// upstreamDNS 读 WAN 口拿到的私有地址 DNS；读不到时为空（内网域名按普通域名处理）。
func upstreamDNS(paths []string) []string {
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			return parseUpstreamDNS(string(b))
		}
	}
	return nil
}

// watchUpstreamDNS 每 syncPeriod 检查一次 WAN 口的 DNS，变了就让内核重建配置。
func (b *Box) watchUpstreamDNS(ctx context.Context) {
	last := b.localDNS()
	t := time.NewTicker(syncPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if cur := upstreamDNS(b.resolvPaths()); !slices.Equal(cur, last) {
			last = cur
			b.log.Info("上级 DNS 变化，重建内核配置", "dns", cur)
			b.sup.Trigger()
		}
	}
}

func (b *Box) resolvPaths() []string {
	if b.opts.ResolvPaths != nil {
		return b.opts.ResolvPaths
	}
	return resolvAutoPaths
}

func (b *Box) localDNS() []string { return upstreamDNS(b.resolvPaths()) }
