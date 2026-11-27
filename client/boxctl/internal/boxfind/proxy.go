package boxfind

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
)

// 路由盒给客户端的代理入口（ADR-084）：客户端在路由盒网络上把非直连的流量经它交给路由盒的内核，
// 按路由盒的策略走。不要求路由盒是本机的网关，旁路由、跨网段都可以。
//
// 入口是 Mihomo 的 mixed 监听（SOCKS5 / HTTP），只监听 LAN 地址，用户名固定、密码由发现口令派生：
// 口令本身不在网络上传输，没有口令的设备用不了。带独立用户的监听不受 allow-lan 限制。
const (
	// ProxyPort 是代理入口的端口（TCP / UDP）。
	ProxyPort = 19444
	// ProxyUser 是代理入口的用户名。
	ProxyUser = "banbu"
)

// ProxyPassword 由发现口令派生代理入口的密码。口令为空时返回空（不开入口）。
func ProxyPassword(secret string) string {
	if secret == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(proto + "|proxy"))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// ProxyListenAddr 拼出代理入口的监听地址。
func ProxyListenAddr(ip string) string { return net.JoinHostPort(ip, strconv.Itoa(ProxyPort)) }

// VerifyProxy 连路由盒 addr 的代理入口完成一次 SOCKS5 用户名密码认证，确认入口开着、口令对得上。
// 旧版路由盒没有这个入口，客户端不能把流量交给它。
func VerifyProxy(ctx context.Context, addr netip.Addr, secret string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := netip.AddrPortFrom(addr, ProxyPort).String()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("连不上路由盒的代理入口 %s（路由盒版本太旧？）：%w", target, err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	// 问候：版本 5，一种方法：用户名密码（2）。
	if _, err := c.Write([]byte{5, 1, 2}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil || resp[0] != 5 || resp[1] != 2 {
		return fmt.Errorf("路由盒的代理入口 %s 不接受用户名密码认证", target)
	}
	pass := ProxyPassword(secret)
	msg := append([]byte{1, byte(len(ProxyUser))}, ProxyUser...)
	msg = append(append(msg, byte(len(pass))), pass...)
	if _, err := c.Write(msg); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, resp); err != nil || resp[1] != 0 {
		return fmt.Errorf("路由盒的代理入口 %s 认证不通过（口令不一致）", target)
	}
	return nil
}
