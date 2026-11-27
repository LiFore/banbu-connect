package groups

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxctl/internal/nodes"
	"boxctl/internal/refs"
	"boxctl/internal/secret"
	"boxctl/internal/store"
	"boxctl/internal/sub"
	"boxctl/internal/subscriptions"
)

type fixture struct {
	db    *sql.DB
	svc   *Service
	nodes *nodes.Service
	subID string
}

// newFixture 导入脱敏样本订阅（107 个节点）。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "boxctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, _ := secret.LoadOrCreateKey(filepath.Join(dir, "device.key"))
	box, _ := secret.New(key)

	sample, err := os.ReadFile("../sub/testdata/sample_sub.json")
	if err != nil {
		t.Fatal(err)
	}
	subs := subscriptions.New(db, box, "")
	subs.Fetch = func(context.Context, sub.FetchRequest) (sub.FetchResult, error) {
		return sub.FetchResult{Body: sample}, nil
	}
	sb, err := subs.Create(ctx, subscriptions.Input{Name: "jsyun", DisplayName: "极速云", URL: "https://sub.example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := subs.Preview(ctx, sb.ID)
	if _, err := subs.Apply(ctx, sb.ID, p.ID, false); err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, svc: New(db), nodes: nodes.New(db, box), subID: sb.ID}
	f.nodes.Groups = f.svc
	return f
}

func (f *fixture) byName(t *testing.T, name string) View {
	t.Helper()
	list, err := f.svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("group %q not found", name)
	return View{}
}

func (f *fixture) create(t *testing.T, g Group) View {
	t.Helper()
	v, err := f.svc.Create(context.Background(), g)
	if err != nil {
		t.Fatalf("create %s: %v", g.Name, err)
	}
	return v
}

func wantInvalid(t *testing.T, err error, contains string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, contains) {
		t.Fatalf("want validation error containing %q, got %v", contains, err)
	}
}

// M4-06 + M4-03：模板生成地区组；新节点自动进入组。
func TestTemplatesAndDynamicMembers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res, err := f.svc.ApplyTemplate(ctx, TemplateRequest{Template: "region_auto", ExcludeISP: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 24 || res.Created[0] != "香港 - 自动选择" { // 样本 24 个地区，香港节点最多
		t.Fatalf("created %d: %v", len(res.Created), res.Created)
	}
	hk := f.byName(t, "香港 - 自动选择")
	if len(hk.Resolved) != 24 || hk.Type != TypeURLTest || hk.Options.URL != DefaultTestURL || hk.Options.Interval != 300 {
		t.Fatalf("hk group: %d members %+v", len(hk.Resolved), hk.Options)
	}
	// 再次执行：同名跳过
	res, _ = f.svc.ApplyTemplate(ctx, TemplateRequest{Template: "region_auto", Regions: []string{"香港"}})
	if len(res.Created) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("rerun %+v", res)
	}
	res, _ = f.svc.ApplyTemplate(ctx, TemplateRequest{Template: "subscription_auto"})
	if len(res.Created) != 1 || res.Created[0] != "极速云 - 自动选择" || len(f.byName(t, "极速云 - 自动选择").Resolved) != 107 {
		t.Fatalf("sub template %+v", res)
	}

	// 新增香港 ISP 节点：自动组排除 isp，手动加一个自建香港节点则自动进入
	if _, err := f.nodes.Create(ctx, nodes.Input{Kind: nodes.KindISP, DisplayName: "香港 ISP",
		Mapping: map[string]any{"type": "http", "server": "isp.example.com", "port": 8080}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.nodes.Create(ctx, nodes.Input{Kind: nodes.KindManual, DisplayName: "香港 自建",
		Mapping: map[string]any{"type": "trojan", "server": "t.example.com", "port": 443, "password": "p"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.byName(t, "香港 - 自动选择").Resolved); n != 25 {
		t.Fatalf("hk after new nodes: %d", n)
	}
}

// M4-04：空组兜底。
func TestEmptyFallback(t *testing.T) {
	f := newFixture(t)
	empty := f.create(t, Group{Name: "火星", Type: TypeSelect, Members: Members{Filter: Filter{Enabled: true, Regions: []string{"火星"}}}})
	if !empty.UsedFallback || len(empty.Resolved) != 1 || empty.Resolved[0].ID != Reject {
		t.Fatalf("default fallback %+v", empty.Resolved)
	}
	hk := f.create(t, Group{Name: "HK", Type: TypeSelect, Members: Members{Filter: Filter{Enabled: true, Regions: []string{"香港"}}}})
	empty.Options.EmptyFallback = hk.ID
	v, err := f.svc.Update(context.Background(), empty.ID, empty.Group)
	if err != nil || v.Resolved[0].ID != hk.ID {
		t.Fatalf("group fallback %+v %v", v.Resolved, err)
	}
}

// M4-05：严格组不可达 DIRECT，包括事后修改被引用的非严格组。
func TestStrictReachability(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hk := f.create(t, Group{Name: "HK", Type: TypeURLTest, Members: Members{Filter: Filter{Enabled: true, Regions: []string{"香港"}}}})
	mode := f.create(t, Group{Name: "代理模式", Type: TypeSelect, Members: Members{Explicit: []string{hk.ID, Direct}}})

	_, err := f.svc.Create(ctx, Group{Name: "Claude", Type: TypeSelect, Strict: true, Members: Members{Explicit: []string{mode.ID}}})
	wantInvalid(t, err, "Claude → 代理模式 → DIRECT")

	_, err = f.svc.Create(ctx, Group{Name: "Claude", Type: TypeSelect, Strict: true,
		Members: Members{Explicit: []string{hk.ID}}, Options: Options{EmptyFallback: Direct}})
	wantInvalid(t, err, "兜底不能是 DIRECT")

	safe := f.create(t, Group{Name: "安全", Type: TypeSelect, Members: Members{Explicit: []string{hk.ID}}})
	claude := f.create(t, Group{Name: "Claude", Type: TypeSelect, Strict: true, Members: Members{Explicit: []string{safe.ID}}})
	if !claude.Strict {
		t.Fatal("strict not saved")
	}
	// 事后把 DIRECT 加进被引用的非严格组 → 拒绝
	safe.Members.Explicit = append(safe.Members.Explicit, Direct)
	_, err = f.svc.Update(ctx, safe.ID, safe.Group)
	wantInvalid(t, err, "Claude → 安全 → DIRECT")
}

func TestGroupCycleAndNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.create(t, Group{Name: "A", Type: TypeSelect, Members: Members{Explicit: []string{Reject}}})
	b := f.create(t, Group{Name: "B", Type: TypeSelect, Members: Members{Explicit: []string{a.ID}}})
	a.Members.Explicit = []string{b.ID}
	_, err := f.svc.Update(ctx, a.ID, a.Group)
	wantInvalid(t, err, "A → B → A")

	_, err = f.svc.Create(ctx, Group{Name: "香港 - 01 - jsyun", Type: TypeSelect}) // 与节点同名
	wantInvalid(t, err, "已被其它组或节点使用")
	_, err = f.svc.Create(ctx, Group{Name: "direct", Type: TypeSelect})
	wantInvalid(t, err, "保留名称")
	_, err = f.svc.Create(ctx, Group{Name: "LB", Type: TypeLoadBalance, Options: Options{Strategy: "random"}})
	wantInvalid(t, err, "负载均衡策略")
	_, err = f.svc.Create(ctx, Group{Name: "X", Type: TypeSelect, Members: Members{Explicit: []string{"n_missing"}}})
	wantInvalid(t, err, "不存在")
	// 节点不能与组同名
	_, err = f.nodes.Create(ctx, nodes.Input{Kind: nodes.KindManual, DisplayName: "A",
		Mapping: map[string]any{"type": "trojan", "server": "t.example.com", "port": 443, "password": "p"}})
	var nve *nodes.ValidationError
	if !errors.As(err, &nve) {
		t.Fatalf("node named as group: %v", err)
	}
}

// 代理组作为前置代理；组内包含节点自身时形成环路。
func TestGroupAsDialer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	chain := f.create(t, Group{Name: "链式拨号代理组", Type: TypeSelect,
		Members: Members{Filter: Filter{Enabled: true, Regions: []string{"香港"}, ExcludeTags: []string{"isp"}}}})
	isp, err := f.nodes.Create(ctx, nodes.Input{Kind: nodes.KindISP, DisplayName: "ISP 美国",
		Mapping: map[string]any{"type": "http", "server": "isp.example.com", "port": 8080}, DialerRef: chain.ID})
	if err != nil {
		t.Fatal(err)
	}
	if isp.DialerName != "链式拨号代理组" {
		t.Fatalf("dialer name %q", isp.DialerName)
	}
	// 把 isp 标签排除去掉 → ISP 节点进入自己的前置组 → 环路（香港节点里没有它，用全量筛选）
	chain.Members.Filter = Filter{Enabled: true}
	_, err = f.svc.Update(ctx, chain.ID, chain.Group)
	wantInvalid(t, err, "前置代理形成环路")

	// 节点侧：把 HK 节点的前置设为包含它自己的组
	var hkNode string
	_ = f.db.QueryRow(`SELECT id FROM proxy_nodes WHERE display_name = '香港 - 01 - jsyun'`).Scan(&hkNode)
	ref := chain.ID
	_, err = f.nodes.Patch(ctx, hkNode, nodes.Attrs{DialerRef: &ref})
	var nve *nodes.ValidationError
	if !errors.As(err, &nve) || !strings.Contains(nve.Msg, "环路") {
		t.Fatalf("self-containing dialer group: %v", err)
	}

	target, label, err := f.svc.DialerTarget(ctx, chain.ID)
	if err != nil || target == "" || !strings.Contains(label, "按首个成员") {
		t.Fatalf("dialer target %q %q %v", target, label, err)
	}
}

// M4-08：删除被引用的组需确认；确认后清理组成员、兜底与节点前置。
func TestDeleteReferencedGroup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hk := f.create(t, Group{Name: "HK", Type: TypeSelect, Members: Members{Filter: Filter{Enabled: true, Regions: []string{"香港"}}}})
	parent := f.create(t, Group{Name: "代理模式", Type: TypeSelect, Members: Members{Explicit: []string{hk.ID, Direct}}})
	other := f.create(t, Group{Name: "空", Type: TypeSelect, Options: Options{EmptyFallback: hk.ID}})
	isp, _ := f.nodes.Create(ctx, nodes.Input{Kind: nodes.KindISP, DisplayName: "ISP",
		Mapping: map[string]any{"type": "http", "server": "isp.example.com", "port": 8080}, DialerRef: hk.ID})

	var re *refs.Error
	if err := f.svc.Delete(ctx, hk.ID, false); !errors.As(err, &re) || len(re.Impacts) != 3 {
		t.Fatalf("impacts: %v", err)
	}
	if err := f.svc.Delete(ctx, hk.ID, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.svc.Get(ctx, parent.ID); len(p.Members.Explicit) != 1 || p.Members.Explicit[0] != Direct {
		t.Fatalf("parent members %v", p.Members.Explicit)
	}
	if o, _ := f.svc.Get(ctx, other.ID); o.Options.EmptyFallback != Reject {
		t.Fatalf("fallback %q", o.Options.EmptyFallback)
	}
	if n, _ := f.nodes.Get(ctx, isp.ID, false); n.DialerRef != "" {
		t.Fatalf("dialer %q", n.DialerRef)
	}
}

func TestReorderAndResolvePreview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.create(t, Group{Name: "A", Type: TypeSelect})
	b := f.create(t, Group{Name: "B", Type: TypeSelect})
	c := f.create(t, Group{Name: "C", Type: TypeSelect})
	if err := f.svc.Reorder(ctx, []string{c.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	list, _ := f.svc.List(ctx)
	if list[0].Name != "C" || list[1].Name != "A" || list[2].Name != "B" {
		t.Fatalf("order %s %s %s", list[0].Name, list[1].Name, list[2].Name)
	}
	_ = b

	v, err := f.svc.Resolve(ctx, Group{Name: "预览", Type: TypeSelect,
		Members: Members{Explicit: []string{a.ID, Direct}, Filter: Filter{Enabled: true, Regions: []string{"日本"}, Protocols: []string{"vless"}}}})
	if err != nil || len(v.Resolved) != 2+4 { // 4 个去重后的日本 vless
		t.Fatalf("resolve %d %v", len(v.Resolved), err)
	}
}

// 启用节点可能使其进入自身的前置组：批量启用被拒绝并回退；新建节点时按其真实属性校验。
func TestEnableCreatesDialerLoop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	chain := f.create(t, Group{Name: "香港链", Type: TypeSelect,
		Members: Members{Filter: Filter{Enabled: true, Regions: []string{"香港"}}}})
	off := false
	// 香港 ISP 节点以“香港链”为前置：禁用时不在组内，合法
	isp, err := f.nodes.Create(ctx, nodes.Input{Kind: nodes.KindISP, DisplayName: "香港 ISP", Enabled: &off,
		Mapping: map[string]any{"type": "http", "server": "isp.example.com", "port": 8080}, DialerRef: chain.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.nodes.SetEnabled(ctx, []string{isp.ID}, true)
	var nve *nodes.ValidationError
	if !errors.As(err, &nve) || !strings.Contains(nve.Msg, "环路") {
		t.Fatalf("enable: %v", err)
	}
	if n, _ := f.nodes.Get(ctx, isp.ID, false); n.Enabled {
		t.Fatal("not reverted")
	}
	on := true
	if _, err := f.nodes.Patch(ctx, isp.ID, nodes.Attrs{Enabled: &on}); !errors.As(err, &nve) {
		t.Fatalf("patch enable: %v", err)
	}
}
