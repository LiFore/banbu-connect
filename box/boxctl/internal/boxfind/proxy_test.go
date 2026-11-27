package boxfind

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
)

// fakeSOCKS 是只做用户名密码认证的 SOCKS5 服务，监听在 ProxyPort（被占用时跳过测试）。
func fakeSOCKS(t *testing.T, pass string) {
	t.Helper()
	ln, err := net.Listen("tcp", ProxyListenAddr("127.0.0.1"))
	if err != nil {
		t.Skipf("端口 %d 被占用：%v", ProxyPort, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				hello := make([]byte, 3)
				if _, err := io.ReadFull(c, hello); err != nil {
					return
				}
				_, _ = c.Write([]byte{5, 2})
				head := make([]byte, 2)
				_, _ = io.ReadFull(c, head)
				user := make([]byte, head[1])
				_, _ = io.ReadFull(c, user)
				n := make([]byte, 1)
				_, _ = io.ReadFull(c, n)
				p := make([]byte, n[0])
				_, _ = io.ReadFull(c, p)
				status := byte(1)
				if string(user) == ProxyUser && string(p) == pass {
					status = 0
				}
				_, _ = c.Write([]byte{1, status})
			}()
		}
	}()
}

func TestVerifyProxy(t *testing.T) {
	fakeSOCKS(t, ProxyPassword("s3cret"))
	lo := netip.MustParseAddr("127.0.0.1")
	if err := VerifyProxy(context.Background(), lo, "s3cret"); err != nil {
		t.Fatalf("口令一致应通过：%v", err)
	}
	if err := VerifyProxy(context.Background(), lo, "other"); err == nil {
		t.Fatal("口令不一致应失败")
	}
	if ProxyPassword("") != "" || ProxyPassword("a") == ProxyPassword("b") || len(ProxyPassword("a")) != 32 {
		t.Fatal("密码派生不对")
	}
}
