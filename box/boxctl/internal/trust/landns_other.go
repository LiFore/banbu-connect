//go:build !darwin

package trust

import (
	"net/netip"
	"syscall"
)

// InterfaceDNS 目前只在 macOS 上实现；其他平台返回空，局域网 DNS 转发口一律拒绝，内核照常用 DoH。
func InterfaceDNS(string) []netip.Addr { return nil }

// bindInterface 只在 macOS 上实现（其他平台没有上游，不会发查询）。
func bindInterface(string, bool) func(network, address string, c syscall.RawConn) error { return nil }
