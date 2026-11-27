package boxfind

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRecorder struct {
	mu     sync.Mutex
	seen   []netip.Addr
	hellos []Hello
}

func (r *fakeRecorder) Seen(ip netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, ip)
}

func (r *fakeRecorder) Hello(_ netip.Addr, h Hello) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hellos = append(r.hellos, h)
}

func (r *fakeRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen), len(r.hellos)
}

// 挑战应答记为来过；签名对的自报记下名字、系统和版本，控制字符去掉、过长的截断；口令不对的不记。
func TestHello(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &fakeRecorder{}
	if err := Serve(ctx, addr.String(), "box", func() string { return "s3cret" }, slog.New(slog.NewTextHandler(io.Discard, nil)), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, addr, "s3cret"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("长", 100)
	if err := SendHello(ctx, addr, "s3cret", Hello{Name: "Mac\x1b[31m 小王", OS: "macOS 15.2", Version: long}); err != nil {
		t.Fatal(err)
	}
	if err := SendHello(ctx, addr, "wrong", Hello{Name: "fake"}); err == nil {
		t.Fatal("wrong secret should be rejected")
	}
	// 记录在应答之后：稍等一下。
	deadline := time.Now().Add(time.Second)
	for {
		s, h := rec.counts()
		if s == 1 && h == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seen = %d, hellos = %d", s, h)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := rec.hellos[0]
	if h.Name != "Mac[31m 小王" || h.OS != "macOS 15.2" || len([]rune(h.Version)) != maxHelloText {
		t.Fatalf("hello = %+v", h)
	}
	if rec.seen[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("seen = %v", rec.seen)
	}
}

// 旧版路由盒不认识自报：客户端拿到错误，不影响别的。
func TestHelloOldBox(t *testing.T) {
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
		_, _ = c.Read(make([]byte, 256))
		c.Close()
	}()
	if err := SendHello(context.Background(), netip.MustParseAddrPort(ln.Addr().String()), "s3cret", Hello{Name: "x"}); err == nil {
		t.Fatal("old box should fail")
	}
}
