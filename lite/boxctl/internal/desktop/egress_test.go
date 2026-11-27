package desktop

import (
	"slices"
	"testing"
)

func TestParseTrace(t *testing.T) {
	ip, c, colo, ok := parseTrace("fl=1\nip=::ffff:1.2.3.4\ncolo=HKG\nloc=us\n")
	if !ok || ip != "1.2.3.4" || c != "US" || colo != "HKG" {
		t.Fatalf("got %q %q %q %v", ip, c, colo, ok)
	}
	if _, _, _, ok := parseTrace("nothing"); ok {
		t.Fatal("没有 IP 不该成功")
	}
}

func TestJudgeEgress(t *testing.T) {
	us := EgressResult{Site: "Cloudflare", OK: true, IP: "1.1.1.1", Country: "US"}
	cl := EgressResult{Site: "Claude", OK: true, IP: "1.1.1.1", Country: "US"}
	if p := judgeEgress([]EgressResult{us, cl}, nil); len(p) != 0 {
		t.Fatalf("正常出口不该有问题：%v", p)
	}
	if p := judgeEgress([]EgressResult{us, cl}, []string{"JP"}); len(p) != 2 {
		t.Fatalf("国家不在允许列表：%v", p)
	}
	if p := judgeEgress([]EgressResult{us, {Site: "Claude", Error: "timeout"}}, nil); len(p) != 1 {
		t.Fatalf("探测失败：%v", p)
	}
	jp := EgressResult{Site: "Claude", OK: true, IP: "2.2.2.2", Country: "JP"}
	if p := judgeEgress([]EgressResult{us, jp}, nil); len(p) != 1 {
		t.Fatalf("出口不一致：%v", p)
	}
}

func TestNormalizeCountries(t *testing.T) {
	got := normalizeCountries([]string{" us", "US", "jp", "USA", "1a", ""})
	if !slices.Equal(got, []string{"US", "JP"}) {
		t.Fatalf("got %v", got)
	}
}
