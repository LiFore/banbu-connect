package trust

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// 开着 TUN 时的真实输出：默认解析器指向 TUN，scoped 部分才是各网卡自己的 DNS。
const scutilSample = `DNS configuration

resolver #1
  search domain[0] : lan
  nameserver[0] : 198.18.0.2
  flags    : Supplemental, Request A records
  order    : 1

resolver #2
  nameserver[0] : 10.200.11.1
  if_index : 11 (en0)
  flags    : Request A records

DNS configuration (for scoped queries)

resolver #1
  search domain[0] : lan
  nameserver[0] : 10.200.11.1
  nameserver[1] : fe80::1%en7
  if_index : 13 (en7)
  flags    : Scoped, Request A records

resolver #2
  search domain[0] : lan
  nameserver[0] : 114.114.114.114
  if_index : 15 (en0)
  flags    : Scoped, Request A records
`

func TestParseScopedDNS(t *testing.T) {
	if got := parseScopedDNS(scutilSample, "en0"); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("114.114.114.114")}) {
		t.Fatalf("en0：%v", got)
	}
	if got := parseScopedDNS(scutilSample, "en7"); len(got) != 2 || got[0] != netip.MustParseAddr("10.200.11.1") {
		t.Fatalf("en7：%v", got)
	}
	if got := parseScopedDNS(scutilSample, "en1"); got != nil {
		t.Fatalf("没有的网卡应为空：%v", got)
	}
	if got := parseScopedDNS("DNS configuration\n", "en0"); got != nil {
		t.Fatalf("没有 scoped 部分应为空：%v", got)
	}
}

// query 构造一个 A 记录查询（带一个 EDNS OPT 记录，模拟真实客户端）。
func query(id uint16, name string) []byte {
	m := make([]byte, 12)
	binary.BigEndian.PutUint16(m[0:2], id)
	binary.BigEndian.PutUint16(m[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(m[4:6], 1)
	binary.BigEndian.PutUint16(m[10:12], 1)
	for _, l := range []string{name, "com"} {
		m = append(m, byte(len(l)))
		m = append(m, l...)
	}
	m = append(m, 0, 0, 1, 0, 1)                      // A IN
	m = append(m, 0, 0, 41, 4, 208, 0, 0, 0, 0, 0, 0) // OPT
	return m
}

func TestForwardLANDNSRefusesWithoutUpstream(t *testing.T) {
	q := query(0xbeef, "baidu")
	resp := forwardLANDNS(context.Background(), q, "", nil, [2]int{})
	if resp == nil || resp[0] != 0xbe || resp[1] != 0xef {
		t.Fatalf("应回同一个 ID：%x", resp)
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	if flags&0x8000 == 0 || flags&0x000f != 5 {
		t.Fatalf("应为 REFUSED 应答：%04x", flags)
	}
	if binary.BigEndian.Uint16(resp[10:12]) != 0 || len(resp) != len(q)-11 {
		t.Fatalf("应去掉附加记录：%x", resp)
	}
	if forwardLANDNS(context.Background(), []byte{1, 2, 3}, "", nil, [2]int{}) != nil {
		t.Fatal("不是 DNS 报文时不作答")
	}
}

func TestForwardLANDNSRelays(t *testing.T) {
	// 假的局域网 DNS：原样回一个标了 QR 的应答。
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := append([]byte(nil), buf[:n]...)
			resp[2] |= 0x80
			_, _ = pc.WriteTo(resp, from)
		}
	}()
	up := netip.MustParseAddrPort(pc.LocalAddr().String())
	dead := netip.MustParseAddrPort("127.0.0.1:1") // 没人监听：失败后试下一个
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp := forwardLANDNS(ctx, query(7, "baidu"), "", []netip.AddrPort{dead, up}, [2]int{})
	if resp == nil || resp[2]&0x80 == 0 || binary.BigEndian.Uint16(resp[0:2]) != 7 {
		t.Fatalf("应转发上游的应答：%x", resp)
	}

	// 指定源端口段：DNS 防泄漏只放行从这段端口发出的查询。占住第一个端口，应换用下一个。
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	first := busy.LocalAddr().(*net.UDPAddr).Port
	c, err := dialFrom(ctx, "", up, [2]int{first, first + 1})
	if err != nil {
		t.Skipf("端口 %d 不可用：%v", first+1, err)
	}
	defer c.Close()
	if got := c.LocalAddr().(*net.UDPAddr).Port; got != first+1 {
		t.Fatalf("应从端口段里没被占用的端口发出：%d", got)
	}
}

func TestTrustedLANDNS(t *testing.T) {
	h := newHarness(t, nil)
	calls := 0
	up := h.e.TrustedLANDNS(func(iface string) []netip.Addr {
		calls++
		if iface != "en0" {
			t.Fatalf("应读物理网卡：%s", iface)
		}
		return []netip.Addr{netip.MustParseAddr("10.200.11.1"), netip.MustParseAddr("198.18.0.2")}
	})
	if _, got := up(); got != nil {
		t.Fatalf("还没确认网络时不转发：%v", got)
	}
	h.detector.set(home)
	h.e.Tick(context.Background(), true)
	iface, got := up()
	if iface != "en0" {
		t.Fatalf("应带上物理网卡名：%q", iface)
	}
	if len(got) != 1 || got[0] != netip.MustParseAddrPort("10.200.11.1:53") {
		t.Fatalf("受信网络上应转给路由盒，排除 TUN 地址：%v", got)
	}
	up()
	if calls != 1 {
		t.Fatalf("应缓存：读了 %d 次", calls)
	}
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	h.e.Tick(context.Background(), true)
	if _, got := up(); got != nil {
		t.Fatalf("不受信网络上不转发：%v", got)
	}
}

func TestLocalLANDNS(t *testing.T) {
	h := newHarness(t, nil)
	old := ifacePrefixes
	ifacePrefixes = func(string) []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("240e:b8f:392c:e201::/64"), netip.MustParsePrefix("192.168.5.0/24")}
	}
	t.Cleanup(func() { ifacePrefixes = old })
	up := h.e.LocalLANDNS(func(iface string) []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("192.168.5.1"), netip.MustParseAddr("114.114.114.114"), netip.MustParseAddr("198.18.0.2"),
			netip.MustParseAddr("240e:b8f:392c:e201:face:21ff:fee5:4cb9"), netip.MustParseAddr("240e:1::1")}
	})
	if _, got := up(); got != nil {
		t.Fatalf("还没识别网络时不转发：%v", got)
	}
	h.detector.set(Network{Interface: "en0", WiFi: true, SSID: "Cafe"})
	h.e.Tick(context.Background(), true)
	iface, got := up()
	if iface != "en0" {
		t.Fatalf("应带上物理网卡名：%q", iface)
	}
	if len(got) != 2 || got[0] != netip.MustParseAddrPort("192.168.5.1:53") || got[1] != netip.MustParseAddrPort("[240e:b8f:392c:e201:face:21ff:fee5:4cb9]:53") {
		t.Fatalf("不受信网络上也转给私有地址和同网段的 DNS，排除其他公网和 TUN 地址：%v", got)
	}
}

// fakeDNS 起一个本机 UDP DNS，应答的 rcode 固定为 rcode。
func fakeDNS(t *testing.T, rcode byte) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := append([]byte(nil), buf[:n]...)
			resp[2] |= 0x80
			resp[3] = resp[3]&0xf0 | rcode
			_, _ = pc.WriteTo(resp, from)
		}
	}()
	return netip.MustParseAddrPort(pc.LocalAddr().String())
}

func TestForwardLANDNSNXDomainTriesNext(t *testing.T) {
	nx, ok := fakeDNS(t, 3), fakeDNS(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp := forwardLANDNS(ctx, query(9, "gitlab"), "", []netip.AddrPort{nx, ok}, [2]int{}); resp == nil || resp[3]&0x0f != 0 {
		t.Fatalf("NXDOMAIN 时应再问下一台：%x", resp)
	}
	if resp := forwardLANDNS(ctx, query(9, "gitlab"), "", []netip.AddrPort{nx}, [2]int{}); resp == nil || resp[3]&0x0f != 3 {
		t.Fatalf("都没有时返回 NXDOMAIN：%x", resp)
	}
}
