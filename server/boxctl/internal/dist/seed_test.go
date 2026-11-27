package dist

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"boxctl/internal/gen"
	"boxctl/internal/killswitch"
)

const seedYAML = `proxies:
  - {name: 美国 - 01, type: socks5, server: 203.0.113.1, port: 1080, _subName: x}
  - {name: 香港 - 01, type: socks5, server: 203.0.113.2, port: 1080}
  - {name: isp-us, type: http, server: 203.0.113.3, port: 3128, username: u, password: p, dialer-proxy: 链式US}
proxy-groups:
  - {name: 代理模式, type: select, proxies: [自动选择, DIRECT]}
  - {name: 自动选择, type: url-test, url: 'http://www.gstatic.com/generate_204', interval: 300, tolerance: 50, hidden: true, proxies: [美国 - 01, 香港 - 01]}
  - {name: 链式US, type: select, proxies: [自动选择]}
  - {name: ISP US, type: select, proxies: [isp-us]}
  - {name: Claude, type: select, proxies: [ISP US, 代理模式]}
  - {name: ChatGPT, type: select, proxies: [代理模式]}
  - {name: 漏网之鱼, type: select, proxies: [DIRECT, 代理模式]}
`

func writeSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"a.yaml":    seedYAML,
		"b.yaml":    "proxies:\n  - {name: 香港 - 01, type: socks5, server: 198.51.100.9, port: 1080}\n", // 同名以 a.yaml 为准
		"rules.txt": "# 注释\nDOMAIN-SUFFIX,stripe.com,代理模式\n\nDOMAIN-KEYWORD,claude,Claude\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSeed(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	res, ok, err := s.Seed(ctx, writeSeed(t))
	if err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	if res != (SeedResult{Nodes: 3, Groups: 7, Rules: 2}) {
		t.Fatalf("result = %+v", res)
	}

	p, err := s.Draft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members := map[string][]string{}
	strict := map[string]bool{}
	for _, g := range p.Groups {
		members[g.Name], strict[g.Name] = g.Members, g.Strict
	}
	// 严格组去掉能走到 DIRECT 的「代理模式」，换成它除 DIRECT 以外的成员。
	if !strict["Claude"] || !slices.Equal(members["Claude"], []string{"ISP US", "自动选择"}) {
		t.Fatalf("Claude = %v strict=%v", members["Claude"], strict["Claude"])
	}
	if !strict["ChatGPT"] || !slices.Equal(members["ChatGPT"], []string{"自动选择"}) {
		t.Fatalf("ChatGPT = %v", members["ChatGPT"])
	}
	if !slices.Equal(members["漏网之鱼"], []string{"DIRECT", "代理模式"}) {
		t.Fatalf("漏网之鱼 = %v", members["漏网之鱼"])
	}
	for _, n := range p.Nodes {
		switch n["name"] {
		case "isp-us":
			if n["dialer-proxy"] != "链式US" {
				t.Fatalf("isp-us dialer = %v", n["dialer-proxy"])
			}
		case "香港 - 01":
			if n["server"] != "203.0.113.2" {
				t.Fatalf("香港 - 01 应以排序靠前的文件为准，得到 %v", n["server"])
			}
		}
		if _, ok := n["_subName"]; ok {
			t.Fatal("附加字段 _subName 应被去掉")
		}
	}
	if !slices.Equal(p.Rules, []string{"DOMAIN-SUFFIX,stripe.com,代理模式", "DOMAIN-KEYWORD,claude,Claude"}) {
		t.Fatalf("rules = %v", p.Rules)
	}

	// 只导入一次。
	if _, ok, err := s.Seed(ctx, writeSeed(t)); ok || err != nil {
		t.Fatalf("second seed: ok=%v err=%v", ok, err)
	}
}

// 导出再导入到新的服务，得到的配置与原来一致（含严格标记、前置代理、线路、规则）。
func TestExportSeedRoundTrip(t *testing.T) {
	ctx := context.Background()
	a := newServer(t)
	if _, _, err := a.Seed(ctx, writeSeed(t)); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSelections(ctx, map[string]string{"Claude": "自动选择"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := a.ExportSeed(ctx, dir); err != nil {
		t.Fatal(err)
	}
	b := newServer(t)
	if _, ok, err := b.Seed(ctx, dir); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	pa, _ := a.Draft(ctx)
	pb, _ := b.Draft(ctx)
	pa.ProfileVersion, pb.ProfileVersion = "", "" // 方案代码是随机的
	ja, _ := json.Marshal(pa)
	jb, _ := json.Marshal(pb)
	if string(ja) != string(jb) {
		t.Fatalf("导入后不一致：\n%s\n%s", ja, jb)
	}
}

func TestSeedSkipsWhenDataExists(t *testing.T) {
	s := newServer(t)
	seed(t, s)
	if _, ok, err := s.Seed(context.Background(), writeSeed(t)); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestSeedRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	dir := t.TempDir()
	bad := "proxies:\n  - {name: a, type: socks5, server: 203.0.113.1, port: 1080}\nproxy-groups:\n  - {name: G, type: select, proxies: [不存在]}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Seed(ctx, dir); err == nil {
		t.Fatal("want error")
	}
	var n int
	_ = s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM proxy_nodes) + (SELECT COUNT(*) FROM proxy_groups)`).Scan(&n)
	if n != 0 {
		t.Fatalf("应清空已导入的内容，剩 %d 条", n)
	}
}

// TestSeedRealFiles 用真实的预置文件检查导入结果能生成客户端配置（含 Kill Switch 严格组）。
// 文件含凭据不进仓库：BOXCTL_SEED_DIR=<目录> go test ./internal/dist -run TestSeedRealFiles
func TestSeedRealFiles(t *testing.T) {
	dir := os.Getenv("BOXCTL_SEED_DIR")
	if dir == "" {
		t.Skip("未设置 BOXCTL_SEED_DIR")
	}
	ctx := context.Background()
	s := newServer(t)
	res, ok, err := s.Seed(ctx, dir)
	if err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	t.Logf("导入 %+v", res)
	p, err := s.Draft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gen.Build(gen.Input{Nodes: p.Nodes, Groups: p.Groups, CustomRules: p.Rules, Mode: "rule", MixedPort: 7890,
		DNSListen: "127.0.0.1:1053", Services: []killswitch.Service{
			{Name: "Claude", Enabled: true, Group: "Claude", Suffixes: []string{"claude.ai"}},
			{Name: "ChatGPT", Enabled: true, Group: "ChatGPT", Suffixes: []string{"chatgpt.com"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range out.Warnings {
		t.Log("warning:", w)
	}
	for _, g := range p.Groups {
		if g.Name == "Claude" || g.Name == "ChatGPT" {
			t.Logf("%s strict=%v members=%v", g.Name, g.Strict, g.Members)
		}
	}
}
