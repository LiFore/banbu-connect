package box

import (
	"strings"
	"testing"
)

const route4 = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
pppoe-wan	00000000	0100000A	0003	0	0	0	00000000	0	0	0
br-lan	0064A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`

const route6 = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 pppoe-wan
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200 lo
fd000000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001 br-lan
`

func TestNetFingerprint(t *testing.T) {
	skip := func(d string) bool { return d == "lo" || d == "br-lan" || d == "boxtun" }
	addrs := func(ip string) func(string) []string {
		return func(dev string) []string {
			if dev == "pppoe-wan" {
				return []string{ip}
			}
			return nil
		}
	}
	a := netFingerprint(route4, route6, addrs("100.64.1.2"), skip)
	if !strings.Contains(a, "v4:pppoe-wan:0100000A") || !strings.Contains(a, "v6:pppoe-wan:") || !strings.Contains(a, "addr:pppoe-wan:100.64.1.2") {
		t.Fatalf("%q", a)
	}
	if strings.Contains(a, "lo") || strings.Contains(a, "br-lan") {
		t.Fatalf("局域网网桥和 lo 不应计入：%q", a)
	}
	if a != netFingerprint(route4, route6, addrs("100.64.1.2"), skip) {
		t.Fatal("相同输入应得到相同结果")
	}
	// PPPoE 重拨换了地址、默认路由消失：都算变化。
	if netFingerprint(route4, route6, addrs("100.64.9.9"), skip) == a {
		t.Fatal("WAN 地址变了应算变化")
	}
	if netFingerprint("Iface\n", "", addrs("100.64.1.2"), skip) == a {
		t.Fatal("默认路由消失应算变化")
	}
	if netFingerprint("", "", addrs(""), skip) != "" {
		t.Fatal("读不到路由表时应为空")
	}
}
