package dist

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"boxctl/internal/gen"
	"boxctl/internal/groups"
	"boxctl/internal/nodes"
)

var versionRe = regexp.MustCompile(`^[a-z0-9]{6}-\d{14}$`)

func TestRuleProfiles(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	d1, d2, d3 := newDevice(t, s), newDevice(t, s), newDevice(t, s)
	if err := s.SetRules(ctx, []string{"#@segment:custom", "DOMAIN,a.com,DIRECT", "#@segment:fallback", "MATCH,DIRECT"}); err != nil {
		t.Fatal(err)
	}
	def, err := s.defaultProfile(ctx)
	if err != nil || !def.Default || def.Name != defaultName || !versionRe.MatchString(def.Version) {
		t.Fatalf("default = %+v, err = %v", def, err)
	}

	// 从默认复制：规则一样，远程规则集没配置过（nil，客户端内置默认）。
	p, err := s.CreateRuleProfile(ctx, RuleProfile{Name: " 办公 ", Devices: []int64{d1.ID, d1.ID}}, def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "办公" || len(p.Rules) != 4 || p.RuleSets != nil || len(p.Devices) != 1 || p.Code == def.Code || !versionRe.MatchString(p.Version) {
		t.Fatalf("p = %+v", p)
	}
	if _, err := s.CreateRuleProfile(ctx, RuleProfile{Name: "办公"}, 0); err == nil {
		t.Fatal("duplicate name should fail")
	}
	if _, err := s.CreateRuleProfile(ctx, RuleProfile{Name: "默认"}, 0); err == nil {
		t.Fatal("reserved name should fail")
	}
	if err := s.DeleteRuleProfile(ctx, def.ID); err == nil {
		t.Fatal("default should not be deletable")
	}
	// 一台客户端只能属于一个方案。
	if _, err := s.CreateRuleProfile(ctx, RuleProfile{Name: "测试", Devices: []int64{d1.ID}}, 0); err == nil {
		t.Fatal("device in two profiles should fail")
	}
	// 规则照默认的规矩检查。
	if _, err := s.UpdateRuleProfile(ctx, p.ID, RuleProfile{Name: "办公", ProfileContent: ProfileContent{Rules: []string{"MATCH,DIRECT", "MATCH,PROXY"}}}, ""); err == nil {
		t.Fatal("two MATCH should fail")
	}
	v1 := p.Version
	p, err = s.UpdateRuleProfile(ctx, p.ID, RuleProfile{
		Name:           "办公",
		ProfileContent: ProfileContent{Rules: []string{"#@segment:custom", "DOMAIN,b.com,REJECT"}, RuleSets: []gen.RuleSet{}},
		Devices:        []int64{d1.ID, d2.ID},
	}, "改规则")
	if err != nil || p.RuleSets == nil || len(p.RuleSets) != 0 || p.Version == v1 || p.Version[:6] != p.Code {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
	// 只改分配不记新版本。
	v2 := p.Version
	p.Devices = []int64{d1.ID, d2.ID}
	if p, err = s.UpdateRuleProfile(ctx, p.ID, p, ""); err != nil || p.Version != v2 {
		t.Fatalf("assignment-only change should keep version: %v %v", p.Version, err)
	}
	vs, err := s.ProfileVersions(ctx, p.ID)
	if err != nil || len(vs) != 2 || !vs[0].Current || vs[0].Note != "改规则" || vs[1].Version != v1 {
		t.Fatalf("versions = %+v, err = %v", vs, err)
	}
	// 没有分配客户端的方案不进草稿。
	if _, err := s.CreateRuleProfile(ctx, RuleProfile{Name: "空方案"}, def.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(ctx, d2.ID); err != nil {
		t.Fatal(err)
	}

	draft, err := s.Draft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Variants) != 1 || len(draft.Variants[0].Devices) != 1 || draft.Variants[0].Rules[0] != "DOMAIN,b.com,REJECT" {
		t.Fatalf("variants = %+v", draft.Variants)
	}
	got := draft.ForDevice(d1.ID)
	if got.RuleProfile != "办公" || got.ProfileVersion != v2 || len(got.Rules) != 1 || got.RuleSets == nil || got.Variants != nil {
		t.Fatalf("d1 profile = %+v", got)
	}
	dp := draft.ForDevice(d3.ID)
	if dp.RuleProfile != "" || len(dp.Rules) != 2 || dp.RuleSets != nil || dp.Variants != nil {
		t.Fatalf("d3 profile = %+v", dp)
	}
	if sum := summarize(draft); len(sum.Variants) != 1 || sum.Variants[0].RuleSets != 0 {
		t.Fatalf("summary = %+v", sum.Variants)
	}

	// 回滚到第一版：规则回到从默认复制的那份，记为新版本，分配不变。
	p, err = s.RollbackProfile(ctx, p.ID, v1)
	if err != nil || len(p.Rules) != 4 || p.Version == v2 || len(p.Devices) != 2 {
		t.Fatalf("rollback: %+v %v", p, err)
	}
	if _, err := s.RollbackProfile(ctx, p.ID, p.Version); err == nil {
		t.Fatal("rollback to identical content should fail")
	}
	if vs, _ = s.ProfileVersions(ctx, p.ID); len(vs) != 3 || vs[0].Note != "回滚到 "+v1 {
		t.Fatalf("versions after rollback = %+v", vs)
	}

	if err := s.DeleteRuleProfile(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if draft, _ = s.Draft(ctx); len(draft.Variants) != 0 {
		t.Fatal("deleted profile should be gone")
	}
}

// 方案拥有的节点和代理组：成员按方案筛选，前置代理自动带上，指向方案外代理组的规则和规则集去掉。
func TestProfileScope(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	mk := func(name string) string {
		n, err := s.Nodes.Create(ctx, nodes.Input{Kind: nodes.KindManual, DisplayName: name,
			Mapping: map[string]any{"type": "socks5", "server": "203.0.113.9", "port": 1080}})
		if err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	us, jp, hop := mk("US"), mk("JP"), mk("Hop")
	if _, err := s.Nodes.Patch(ctx, us, nodes.Attrs{DialerRef: &hop}); err != nil {
		t.Fatal(err)
	}
	mkGroup := func(name string, members ...string) {
		if _, err := s.Groups.Create(ctx, groups.Group{Name: name, Type: groups.TypeSelect, Members: groups.Members{Explicit: members}}); err != nil {
			t.Fatal(err)
		}
	}
	mkGroup("Claude", us, jp)
	mkGroup("Japan", jp)
	d := newDevice(t, s)

	p, err := s.CreateRuleProfile(ctx, RuleProfile{Name: "美国", Devices: []int64{d.ID}, ProfileContent: ProfileContent{
		Nodes:      []string{"US"},
		Groups:     []string{"Claude", "Japan"},
		Selections: map[string]string{"Claude": "JP", "Japan": "JP"},
		Rules:      []string{"DOMAIN,a.com,Claude", "DOMAIN,b.com,Japan", "DOMAIN,c.com,DIRECT"},
		RuleSets:   []gen.RuleSet{{Name: "jp", Behavior: "domain", URL: "https://e/jp.yaml", Target: "Japan"}},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s.Draft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := draft.ForDevice(d.ID)
	var names []string
	for _, n := range got.Nodes {
		names = append(names, n["name"].(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Hop", "US"}) {
		t.Fatalf("nodes = %v（前置代理应自动带上）", names)
	}
	if len(got.Groups) != 1 || got.Groups[0].Name != "Claude" || !slices.Equal(got.Groups[0].Members, []string{"US"}) {
		t.Fatalf("groups = %+v（Japan 筛空应去掉）", got.Groups)
	}
	if len(got.Selections) != 0 {
		t.Fatalf("selections = %v（JP 不在方案里）", got.Selections)
	}
	if !slices.Equal(got.Rules, []string{"DOMAIN,a.com,Claude", "DOMAIN,c.com,DIRECT"}) || len(got.RuleSets) != 0 {
		t.Fatalf("rules = %v, sets = %v", got.Rules, got.RuleSets)
	}
	// 默认方案仍是全部。
	if len(draft.Nodes) != 3 || len(draft.Groups) != 2 {
		t.Fatalf("default: %d nodes, %d groups", len(draft.Nodes), len(draft.Groups))
	}
	_ = p
}
