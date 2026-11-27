package box

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"boxctl/internal/core"
	"boxctl/internal/guard"
)

func TestRecordVersions(t *testing.T) {
	b, h, _ := setup(t)
	ctx := context.Background()
	yaml1 := []byte("proxies:\n  - name: a\n    password: hunter2\n    uuid: 1111\nrules:\n  - MATCH,DIRECT\n")
	p := guard.Policy{Suffixes: []string{"claude.ai"}, DNSUpstream: DNSListen}

	if err := b.recordVersion(ctx, yaml1, p); err != nil {
		t.Fatal(err)
	}
	// 只有运行状态变化（阻断、内核停止）不算新版本。
	held := p
	held.Hold, held.Reason, held.ForwardAll = true, "出口不符", false
	if err := b.recordVersion(ctx, yaml1, held); err != nil {
		t.Fatal(err)
	}
	list, err := b.listVersions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("运行状态变化不记版本：%v %v", list, err)
	}

	// 只改策略：新版本，内核配置摘要不变。
	p2 := p
	p2.Suffixes = append(p2.Suffixes, "openai.com")
	if err := b.recordVersion(ctx, yaml1, p2); err != nil {
		t.Fatal(err)
	}
	yaml2 := []byte(strings.Replace(string(yaml1), "MATCH,DIRECT", "MATCH,Proxy", 1))
	if err := b.recordVersion(ctx, yaml2, p2); err != nil {
		t.Fatal(err)
	}
	list, _ = b.listVersions(ctx)
	if len(list) != 3 || list[0].ConfigHash != core.ConfigHash(yaml2) || list[1].ConfigHash != core.ConfigHash(yaml1) {
		t.Fatalf("versions = %+v", list)
	}

	// 确认结果写到最新的同摘要版本上。
	b.onConfirm(core.ConfirmEvent{Hash: core.ConfigHash(yaml2), State: core.ConfirmRolledBack, Reason: "出口不通"})
	list, _ = b.listVersions(ctx)
	if list[0].Status != "rolled_back" || list[0].Reason != "出口不通" || list[1].Status != "pending" {
		t.Fatalf("confirm = %+v", list)
	}

	// 查看：凭据打码，三种产物都在。
	w := call(t, h, http.MethodGet, "/api/v1/box/versions/1", "")
	var d VersionDetail
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &d) != nil {
		t.Fatalf("detail %d %s", w.Code, w.Body)
	}
	if strings.Contains(d.Mihomo, "hunter2") || strings.Contains(d.Mihomo, "1111") || !strings.Contains(d.Mihomo, "'******'") {
		t.Fatalf("凭据应打码：%s", d.Mihomo)
	}
	if !strings.Contains(d.GuardPolicy, "claude.ai") || !strings.Contains(d.Dnsmasq, "nftset=/claude.ai/") {
		t.Fatalf("guard 产物：%s %s", d.GuardPolicy, d.Dnsmasq)
	}

	// 对比：默认与上一个版本比。
	w = call(t, h, http.MethodGet, "/api/v1/box/versions/2/diff", "")
	var diff VersionDiff
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &diff) != nil {
		t.Fatalf("diff %d %s", w.Code, w.Body)
	}
	if diff.From != 1 || diff.Mihomo != "" || !strings.Contains(diff.GuardPolicy, `+    "openai.com"`) || !strings.Contains(diff.Dnsmasq, "+nftset=/openai.com/") {
		t.Fatalf("diff = %+v", diff)
	}
	w = call(t, h, http.MethodGet, "/api/v1/box/versions/3/diff?base=1", "")
	_ = json.Unmarshal(w.Body.Bytes(), &diff)
	if !strings.Contains(diff.Mihomo, "-  - MATCH,DIRECT") || !strings.Contains(diff.Mihomo, "+  - MATCH,Proxy") {
		t.Fatalf("mihomo diff = %q", diff.Mihomo)
	}
	if w := call(t, h, http.MethodGet, "/api/v1/box/versions/99", ""); w.Code != http.StatusNotFound {
		t.Fatalf("不存在的版本：%d", w.Code)
	}
}

func TestPruneVersions(t *testing.T) {
	b, _, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < keepVersions+5; i++ {
		y := []byte("rules:\n  - MATCH,DIRECT\n# " + strings.Repeat("x", i) + "\n")
		if err := b.recordVersion(ctx, y, guard.Policy{DNSUpstream: DNSListen}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := b.listVersions(ctx)
	if len(list) != keepVersions || list[len(list)-1].ID != 6 {
		t.Fatalf("应只保留最近 %d 个：%d，最旧 %d", keepVersions, len(list), list[len(list)-1].ID)
	}
}

func TestMaskSecrets(t *testing.T) {
	in := "secret: abc\nproxies:\n  - {name: a, password: inl, port: 1}\n  - name: b\n    password: \"p w\"\n    private-key: k\n    server: 1.2.3.4\n"
	out := MaskSecrets(in)
	for _, leak := range []string{"abc", "p w", ": k", "inl"} {
		if strings.Contains(out, leak) {
			t.Fatalf("泄漏 %q：%s", leak, out)
		}
	}
	if !strings.Contains(out, "server: 1.2.3.4") {
		t.Fatal("非凭据字段应保留")
	}
}

func mustPolicy(t *testing.T, b *Box) guard.Policy {
	t.Helper()
	p, err := b.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
