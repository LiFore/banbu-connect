package trust

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// 绑定网卡（IP_BOUND_IF）后仍能收发：绑回环网卡问本机的假 DNS。绑到别的网卡则到不了 127.0.0.1。
func TestForwardLANDNSBindsInterface(t *testing.T) {
	up := fakeDNS(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp := forwardLANDNS(ctx, query(5, "gitlab"), "lo0", []netip.AddrPort{up}, [2]int{}); resp == nil || resp[3]&0x0f != 0 {
		t.Fatalf("绑定 lo0 应能收到应答：%x", resp)
	}
	if _, err := dialFrom(ctx, "no-such-if0", up, [2]int{}); err == nil {
		t.Fatal("网卡不存在时应失败，不能退回按路由发出")
	}
}
