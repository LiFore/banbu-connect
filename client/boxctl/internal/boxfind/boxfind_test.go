package boxfind

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func serve(t *testing.T, id, secret string) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := Serve(ctx, addr, id, func() string { return secret }, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err != nil {
		t.Fatal(err)
	}
	return netip.MustParseAddrPort(addr)
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	addr := serve(t, "home-box", "s3cret")
	b, err := Verify(ctx, addr, "s3cret")
	if err != nil || b.ID != "home-box" || b.Addr != addr {
		t.Fatalf("verify = %+v, %v", b, err)
	}
	if _, err := Verify(ctx, addr, "wrong"); err == nil || !strings.Contains(err.Error(), "验证不通过") {
		t.Fatalf("wrong secret: %v", err)
	}
	// 路由盒没设置口令：不应答。
	silent := serve(t, "box2", "")
	if _, err := Verify(ctx, silent, "s3cret"); err == nil {
		t.Fatal("box without secret should not verify")
	}
}

// 冒充的服务器回放别的随机数的签名，验证不过。
func TestVerifyRejectsReplay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Read(make([]byte, 64))
		io.WriteString(c, proto+" box "+sign("s3cret", strings.Repeat("0", 32), "box")+"\n")
	}()
	if _, err := Verify(context.Background(), netip.MustParseAddrPort(ln.Addr().String()), "s3cret"); err == nil {
		t.Fatal("replayed signature accepted")
	}
}

func TestDefaultID(t *testing.T) {
	for in, want := range map[string]string{"iStoreOS": "iStoreOS", "路由器": "box", "my box!": "mybox"} {
		if got := DefaultID(in); got != want || !ValidID(got) {
			t.Errorf("DefaultID(%q) = %q", in, got)
		}
	}
}
