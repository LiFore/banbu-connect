package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"boxctl/internal/gen"
	"boxctl/internal/guard"
)

func TestParseUpstreamDNS(t *testing.T) {
	got := parseUpstreamDNS(`# Interface wan
nameserver 192.168.1.1
nameserver 223.5.5.5
nameserver 192.168.1.1
search lan
nameserver fd00::1
nameserver fe80::1%eth1
nameserver 10.0.0.53
`)
	want := []string{"udp://192.168.1.1:53", "udp://[fd00::1]:53", "udp://10.0.0.53:53"}
	if !slices.Equal(got, want) {
		t.Fatalf("只取私有地址并去重：%v", got)
	}
}

func TestUpstreamDNSPaths(t *testing.T) {
	dir := t.TempDir()
	newer, older := filepath.Join(dir, "resolv.conf.d", "resolv.conf.auto"), filepath.Join(dir, "resolv.conf.auto")
	if err := os.WriteFile(older, []byte("nameserver 192.168.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := upstreamDNS([]string{newer, older}); !slices.Equal(got, []string{"udp://192.168.2.1:53"}) {
		t.Fatalf("新位置没有时读旧位置：%v", got)
	}
	if got := upstreamDNS([]string{newer}); got != nil {
		t.Fatalf("读不到时为空：%v", got)
	}
}

// dnsmasq 放行私有地址答案的域名要覆盖内核交给上级本地 DNS 的全部域名，否则答案会被 rebind 保护丢掉。
func TestLocalDomainsMatchGen(t *testing.T) {
	var want []string
	for _, s := range gen.LocalSuffixes {
		if d := strings.TrimPrefix(s, "+."); !strings.HasSuffix(d, ".in-addr.arpa") {
			want = append(want, d)
		}
	}
	if !slices.Equal(want, guard.LocalDomains) {
		t.Fatalf("guard.LocalDomains=%v，gen.LocalSuffixes 的域名部分=%v", guard.LocalDomains, want)
	}
}
