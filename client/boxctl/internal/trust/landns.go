package trust

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// 局域网 DNS 转发口（BXC-D0-002）：内核国内 DNS 组的兜底上游。
//
// 路由盒网络会拦公共 DoH（223.5.5.5、1.12.12.12），内核只能用路由盒自己的 DNS（它本身防泄漏）。
// 不能用 Mihomo 的 system 上游：TUN 起来后 macOS 的默认解析器变成 TUN 里的 198.18.0.2，
// /etc/resolv.conf 里只剩它，又被 Mihomo 当成自己的地址剔除，system 一个上游都没有。
//
// 所以由服务转发：只在受信网络（路由盒）上，把查询交给物理网卡的 DNS 服务器；其他网络上立即拒绝
// （REFUSED），内核取 DoH 的结果，不会把查询明文发给不受信的网络。地址按网络实时读取，换网络不用重载内核。

// LANDNSUpstream 返回物理网卡名和当前应转发到的 DNS 服务器；不该转发（不在受信网络、读不到）时 servers 为空。
// 查询绑定在这块网卡上发出：DNS 不在本网段时按路由会进 TUN，被内核的 DNS 劫持截回来绕圈。
type LANDNSUpstream func() (iface string, servers []netip.AddrPort)

const (
	landnsTimeout = 2 * time.Second
	landnsMaxMsg  = 4096
)

// ServeLANDNS 在 addr（UDP）上提供转发，ctx 结束时停止。端口被占用时返回错误。
// srcPorts 不为零时查询只从这段本机端口（含两端）发出，DNS 防泄漏据此放行（见 killswitch.LocalDNSSrcPorts）。
func ServeLANDNS(ctx context.Context, addr string, upstream LANDNSUpstream, srcPorts [2]int, log *slog.Logger) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	go func() {
		buf := make([]byte, landnsMaxMsg)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			q := append([]byte(nil), buf[:n]...)
			go func() {
				iface, servers := upstream()
				if resp := forwardLANDNS(ctx, q, iface, servers, srcPorts); resp != nil {
					_, _ = pc.WriteTo(resp, from)
				}
			}()
		}
	}()
	if log != nil {
		log.Info("局域网 DNS 转发口已启动", "addr", addr)
	}
	return nil
}

// forwardLANDNS 把查询依次交给上游，返回第一个应答；没有上游或都失败时返回 REFUSED / SERVFAIL。
// 查询太短（不是 DNS 报文）时返回 nil，不作答。
func forwardLANDNS(ctx context.Context, q []byte, iface string, servers []netip.AddrPort, srcPorts [2]int) []byte {
	if len(q) < 12 {
		return nil
	}
	if len(servers) == 0 {
		return dnsError(q, 5) // REFUSED：内核按失败处理，取其他上游的结果
	}
	var nx []byte
	for _, s := range servers {
		resp, err := exchangeUDP(ctx, q, iface, s, srcPorts)
		if err != nil {
			continue
		}
		// NXDOMAIN 时再问下一台：网卡上常有多台 DNS（如路由器的 IPv6 地址和内网 DNS），内网记录可能只在其中一台上。
		if resp[3]&0x0f == 3 {
			nx = resp
			continue
		}
		return resp
	}
	if nx != nil {
		return nx
	}
	return dnsError(q, 2) // SERVFAIL
}

func exchangeUDP(ctx context.Context, q []byte, iface string, server netip.AddrPort, srcPorts [2]int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, landnsTimeout)
	defer cancel()
	c, err := dialFrom(ctx, iface, server, srcPorts)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, landnsMaxMsg)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		// 只认同一个 ID 的应答。
		if n >= 12 && buf[0] == q[0] && buf[1] == q[1] {
			return buf[:n], nil
		}
	}
}

// dialFrom 经网卡 iface（为空时按路由）连到 server；srcPorts 不为零时依次试这段本机端口，
// 都被占用（并发查询太多）时返回错误。
func dialFrom(ctx context.Context, iface string, server netip.AddrPort, srcPorts [2]int) (net.Conn, error) {
	ctl := bindInterface(iface, server.Addr().Is6() && !server.Addr().Is4In6())
	if srcPorts[0] == 0 {
		d := net.Dialer{Control: ctl}
		return d.DialContext(ctx, "udp", server.String())
	}
	var err error
	for p := srcPorts[0]; p <= srcPorts[1]; p++ {
		d := net.Dialer{LocalAddr: &net.UDPAddr{Port: p}, Control: ctl}
		var c net.Conn
		if c, err = d.DialContext(ctx, "udp", server.String()); err == nil {
			return c, nil
		}
	}
	return nil, err
}

// dnsError 用查询的头部和问题部分构造一个错误应答。
func dnsError(q []byte, rcode byte) []byte {
	resp := append([]byte(nil), q...)
	flags := binary.BigEndian.Uint16(resp[2:4])
	flags |= 0x8000  // QR：应答
	flags &^= 0x0080 // RA 清零
	flags = flags&^0x000f | uint16(rcode)
	binary.BigEndian.PutUint16(resp[2:4], flags)
	// 只保留问题部分：应答、授权、附加记录数清零（EDNS 的 OPT 记录也去掉）。
	binary.BigEndian.PutUint16(resp[6:8], 0)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	if end := questionEnd(resp); end > 0 {
		resp = resp[:end]
	}
	return resp
}

// questionEnd 返回问题部分结束的位置；解析不了时返回 0。
func questionEnd(m []byte) int {
	qd := int(binary.BigEndian.Uint16(m[4:6]))
	i := 12
	for range qd {
		for {
			if i >= len(m) {
				return 0
			}
			l := int(m[i])
			if l == 0 {
				i++
				break
			}
			if l&0xc0 == 0xc0 { // 压缩指针（问题里一般不会有）
				i += 2
				break
			}
			i += 1 + l
		}
		i += 4 // QTYPE + QCLASS
		if i > len(m) {
			return 0
		}
	}
	return i
}

// TrustedLANDNS 返回受信网络上物理网卡的 DNS 服务器。servers 按网卡名读取（各平台实现），结果缓存一会儿。
func (e *Evaluator) TrustedLANDNS(servers func(iface string) []netip.Addr) LANDNSUpstream {
	return cachedLANDNS(e.TrustedInterface, servers, func(string, netip.Addr) bool { return true })
}

// LocalLANDNS 返回当前网络（受信与否都算）物理网卡上局域网 DNS 服务器，给内网域名用（ADR-080）：
// 私有地址，或与网卡地址同网段（路由器用公网 IPv6 地址下发 DNS 时很常见，如 240e:...）。
// 不开代理时系统本来就问它们；只取私有地址，不把内网域名发给运营商或公共 DNS。
func (e *Evaluator) LocalLANDNS(servers func(iface string) []netip.Addr) LANDNSUpstream {
	return cachedLANDNS(e.CurrentInterface, servers, func(iface string, a netip.Addr) bool {
		if a.IsPrivate() {
			return true
		}
		if !a.IsGlobalUnicast() { // 链路本地地址要带网卡 zone，这里不处理
			return false
		}
		for _, p := range ifacePrefixes(iface) {
			if p.Contains(a) {
				return true
			}
		}
		return false
	})
}

// ifacePrefixes 返回网卡上地址所在的网段；测试里替换。
var ifacePrefixes = func(iface string) []netip.Prefix {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				ones, _ := n.Mask.Size()
				out = append(out, netip.PrefixFrom(ip.Unmap(), ones).Masked())
			}
		}
	}
	return out
}

func cachedLANDNS(current func() (string, bool), servers func(iface string) []netip.Addr, keep func(iface string, a netip.Addr) bool) LANDNSUpstream {
	var mu sync.Mutex
	var cachedIface string
	var cached []netip.AddrPort
	var at time.Time
	return func() (string, []netip.AddrPort) {
		iface, ok := current()
		if !ok {
			return "", nil
		}
		mu.Lock()
		defer mu.Unlock()
		if iface == cachedIface && time.Since(at) < 10*time.Second {
			return iface, cached
		}
		var out []netip.AddrPort
		for _, a := range servers(iface) {
			a = a.Unmap()
			// TUN 里的地址（198.18.0.0/15）不是局域网 DNS，转过去会绕回内核。
			if a.IsValid() && !tunRange.Contains(a) && keep(iface, a) {
				out = append(out, netip.AddrPortFrom(a, 53))
			}
		}
		cachedIface, cached, at = iface, out, time.Now()
		return iface, out
	}
}

var tunRange = netip.MustParsePrefix("198.18.0.0/15")

// parseScopedDNS 从 scutil --dns 的输出里取 iface 的 scoped 解析器的 nameserver。
// 只看「DNS configuration (for scoped queries)」之后的部分：前面的默认解析器在 TUN 起来后指向 TUN。
func parseScopedDNS(out, iface string) []netip.Addr {
	_, scoped, ok := strings.Cut(out, "(for scoped queries)")
	if !ok {
		return nil
	}
	var res []netip.Addr
	for _, block := range strings.Split(scoped, "resolver #")[1:] {
		var servers []netip.Addr
		match := false
		for _, line := range strings.Split(block, "\n") {
			key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			switch {
			case strings.HasPrefix(key, "nameserver["):
				if a, err := netip.ParseAddr(val); err == nil {
					servers = append(servers, a)
				}
			case key == "if_index":
				match = strings.HasSuffix(val, "("+iface+")")
			}
		}
		if match {
			res = append(res, servers...)
		}
	}
	return res
}
