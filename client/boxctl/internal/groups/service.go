package groups

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"boxctl/internal/refs"
)

var ErrNotFound = errors.New("代理组不存在")

const timeLayout = time.RFC3339

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func New(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

// View 是列表/详情返回的组，附带解析后的成员。
type View struct {
	Group
	Resolved     []MemberRef `json:"resolved"`
	UsedFallback bool        `json:"used_fallback"`
	Warnings     []string    `json:"warnings"`
}

func (g *graph) view(gr *Group) View {
	members, fb, warns := g.resolve(gr)
	if warns == nil {
		warns = []string{}
	}
	return View{Group: *gr, Resolved: members, UsedFallback: fb, Warnings: warns}
}

func (s *Service) List(ctx context.Context) ([]View, error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, g.view(g.groups[id]))
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, id string) (View, error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return View{}, err
	}
	gr := g.groups[id]
	if gr == nil {
		return View{}, ErrNotFound
	}
	return g.view(gr), nil
}

// Resolve 预览未保存的组配置会得到哪些成员（表单实时预览）。
func (s *Service) Resolve(ctx context.Context, in Group) (View, error) {
	if err := in.normalize(); err != nil {
		return View{}, err
	}
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return View{}, err
	}
	return g.view(&in), nil
}

func (s *Service) Create(ctx context.Context, in Group) (View, error) {
	in.ID = "g_" + randomHex(8)
	return s.save(ctx, in, true)
}

func (s *Service) Update(ctx context.Context, id string, in Group) (View, error) {
	in.ID = id
	return s.save(ctx, in, false)
}

func (s *Service) save(ctx context.Context, in Group, creating bool) (View, error) {
	if err := in.normalize(); err != nil {
		return View{}, err
	}
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return View{}, err
	}
	old := g.groups[in.ID]
	if !creating && old == nil {
		return View{}, ErrNotFound
	}
	if err := s.checkName(ctx, in.ID, in.Name); err != nil {
		return View{}, err
	}
	for _, ref := range in.Members.Explicit {
		if ref != Direct && ref != Reject && g.groups[ref] == nil && g.nodes[ref] == nil {
			return View{}, invalid("成员 %s 不存在", ref)
		}
	}
	if fb := in.Options.EmptyFallback; fb != Direct && fb != Reject && g.groups[fb] == nil {
		return View{}, invalid("兜底组不存在")
	}
	if fb := in.Options.EmptyFallback; fb == in.ID {
		return View{}, invalid("兜底不能是组自身")
	}

	if creating {
		in.SortOrder = len(g.order)
		g.order = append(g.order, in.ID)
	} else {
		in.SortOrder = old.SortOrder
	}
	g.groups[in.ID] = &in
	if err := g.validate(); err != nil {
		return View{}, err
	}

	members, _ := json.Marshal(in.Members)
	options, _ := json.Marshal(in.Options)
	now := s.now().UTC().Format(timeLayout)
	if creating {
		_, err = s.db.ExecContext(ctx, `INSERT INTO proxy_groups
			(id, name, type, strict, sort_order, members, options, icon, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, in.Name, in.Type, in.Strict, in.SortOrder, string(members), string(options), in.Icon, now, now)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE proxy_groups SET name = ?, type = ?, strict = ?, members = ?,
			options = ?, icon = ?, updated_at = ? WHERE id = ?`,
			in.Name, in.Type, in.Strict, string(members), string(options), in.Icon, now, in.ID)
	}
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, in.ID)
}

// checkName：组名与节点名共用 Mihomo 的命名空间。
func (s *Service) checkName(ctx context.Context, id, name string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM proxy_groups WHERE name = ? AND id != ?) +
		(SELECT COUNT(*) FROM proxy_nodes WHERE display_name = ?)`, name, id, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return invalid("名称「%s」已被其它组或节点使用", name)
	}
	return nil
}

// Delete 删除组；被其它组、兜底、节点前置或规则引用且未确认时返回 *refs.Error（M4-08）。
// 确认删除后，其它组显式成员中的引用会一并移除，兜底改回 REJECT，节点前置清空；
// 规则的目标不自动改写（改变分流行为须由用户决定），由 M8 静态校验报出。
func (s *Service) Delete(ctx context.Context, id string, confirm bool) error {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return err
	}
	gr := g.groups[id]
	if gr == nil {
		return ErrNotFound
	}
	impacts, err := refs.Find(ctx, s.db, map[string]string{id: gr.Name})
	if err != nil {
		return err
	}
	if len(impacts) > 0 && !confirm {
		return &refs.Error{Impacts: impacts}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(timeLayout)
	for _, oid := range g.order {
		o := g.groups[oid]
		if oid == id {
			continue
		}
		changed := false
		kept := o.Members.Explicit[:0:0]
		for _, ref := range o.Members.Explicit {
			if ref == id {
				changed = true
				continue
			}
			kept = append(kept, ref)
		}
		o.Members.Explicit = kept
		if o.Options.EmptyFallback == id {
			o.Options.EmptyFallback = Reject
			changed = true
		}
		if !changed {
			continue
		}
		members, _ := json.Marshal(o.Members)
		options, _ := json.Marshal(o.Options)
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_groups SET members = ?, options = ?, updated_at = ? WHERE id = ?`,
			string(members), string(options), now, oid); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proxy_nodes SET dialer_ref = '' WHERE dialer_ref = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_groups WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Reorder 按给定顺序重排（M4-07）；未列出的组排在后面。
func (s *Service) Reorder(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_groups SET sort_order = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proxy_groups SET sort_order = sort_order + ? WHERE id NOT IN (`+
		placeholders(len(ids))+`)`, append([]any{len(ids)}, toAny(ids)...)...); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- 供节点服务使用（前置代理可以是组） ----

// CheckNode 以节点保存后的属性 n 替换图中该节点（新建则加入）后做整图校验。
// 返回非空 msg 表示校验失败（由调用方转为其自身的校验错误）。
func (s *Service) CheckNode(ctx context.Context, n refs.NodeAttrs) (msg string, err error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return "", err
	}
	if ref := n.DialerRef; ref != "" {
		if ref == n.ID {
			return "不能把节点自身设为前置代理", nil
		}
		if g.nodes[ref] == nil && g.groups[ref] == nil {
			return "前置代理不存在", nil
		}
	}
	if g.nodes[n.ID] == nil {
		g.nodeOrder = append(g.nodeOrder, n.ID)
	}
	name := n.Name
	if name == "" {
		name = "本节点"
	}
	g.nodes[n.ID] = &nodeInfo{ID: n.ID, Name: name, Kind: n.Kind, SubscriptionID: n.SubscriptionID,
		Protocol: n.Protocol, Region: n.Region, DialerRef: n.DialerRef, Enabled: n.Enabled, Tags: n.Tags}
	return s.validateGraph(g)
}

// CheckGraph 校验当前库内整张图。
func (s *Service) CheckGraph(ctx context.Context) (msg string, err error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return "", err
	}
	return s.validateGraph(g)
}

func (s *Service) validateGraph(g *graph) (string, error) {
	var ve *ValidationError
	if err := g.validate(); errors.As(err, &ve) {
		return ve.Msg, nil
	} else if err != nil {
		return "", err
	}
	return "", nil
}

// DialerTarget 返回前置引用在进程内测试时实际使用的节点：节点直接返回；
// 组取解析后的第一个节点成员（运行时的实际选择需由 Mihomo 决定）。
func (s *Service) DialerTarget(ctx context.Context, ref string) (nodeID, label string, err error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return "", "", err
	}
	if n := g.nodes[ref]; n != nil {
		return ref, n.Name, nil
	}
	gr := g.groups[ref]
	if gr == nil {
		return "", "", invalid("前置代理 %s 不存在", ref)
	}
	paths := g.expandDialer(ref, map[string]bool{})
	if len(paths) == 0 {
		return "", "", invalid("前置代理组「%s」没有可用节点", gr.Name)
	}
	first := paths[0][len(paths[0])-1]
	return first, gr.Name + "（按首个成员 " + g.nodes[first].Name + " 测试）", nil
}

// Export 是生成配置用的组：成员已解析成名称，顺序与界面一致。
type Export struct {
	Name    string
	Type    string
	Strict  bool
	Options Options
	Members []string
}

// Export 导出全部组，供配置生成器使用。
func (s *Service) Export(ctx context.Context) ([]Export, error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]Export, 0, len(g.order))
	for _, id := range g.order {
		gr := g.groups[id]
		members, _, _ := g.resolve(gr)
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = m.Name
		}
		out = append(out, Export{Name: gr.Name, Type: gr.Type, Strict: gr.Strict, Options: gr.Options, Members: names})
	}
	return out, nil
}

// ---- 模板（M4-06） ----

type TemplateRequest struct {
	Template string   `json:"template"` // region_auto / region_manual / subscription_auto
	Regions  []string `json:"regions"`  // 空表示所有有节点的地区
	// ExcludeISP：地区组默认排除 isp 标签节点（对应原脚本的 proxiesWithoutISP）
	ExcludeISP bool `json:"exclude_isp"`
}

type TemplateResult struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"` // 同名组已存在
}

func (s *Service) ApplyTemplate(ctx context.Context, req TemplateRequest) (TemplateResult, error) {
	g, err := loadGraph(ctx, s.db)
	if err != nil {
		return TemplateResult{}, err
	}
	exclude := []string{}
	if req.ExcludeISP {
		exclude = []string{"isp"}
	}
	var plans []Group
	switch req.Template {
	case "region_auto", "region_manual":
		regions := req.Regions
		if len(regions) == 0 {
			counts := map[string]int{}
			for _, id := range g.nodeOrder {
				if n := g.nodes[id]; n.Enabled && !(req.ExcludeISP && contains(n.Tags, "isp")) {
					counts[n.Region]++
				}
			}
			for r := range counts {
				regions = append(regions, r)
			}
			sort.Slice(regions, func(i, j int) bool {
				if counts[regions[i]] != counts[regions[j]] {
					return counts[regions[i]] > counts[regions[j]]
				}
				return regions[i] < regions[j]
			})
		}
		for _, r := range regions {
			f := Filter{Enabled: true, Regions: []string{r}, ExcludeTags: exclude}
			if req.Template == "region_auto" {
				plans = append(plans, Group{Name: r + " - 自动选择", Type: TypeURLTest,
					Members: Members{Filter: f}, Options: Options{Tolerance: 50, Hidden: true}})
			} else {
				plans = append(plans, Group{Name: r + " - 手工选择", Type: TypeSelect, Members: Members{Filter: f}})
			}
		}
	case "subscription_auto":
		rows, err := s.db.QueryContext(ctx, `SELECT id, CASE WHEN display_name != '' THEN display_name ELSE name END
			FROM subscriptions ORDER BY created_at`)
		if err != nil {
			return TemplateResult{}, err
		}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return TemplateResult{}, err
			}
			plans = append(plans, Group{Name: name + " - 自动选择", Type: TypeURLTest,
				Members: Members{Filter: Filter{Enabled: true, Subscriptions: []string{id}, ExcludeTags: exclude}},
				Options: Options{Tolerance: 50, Hidden: true}})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return TemplateResult{}, err
		}
	default:
		return TemplateResult{}, invalid("未知模板 %s", req.Template)
	}

	res := TemplateResult{Created: []string{}, Skipped: []string{}}
	for _, p := range plans {
		if _, err := s.Create(ctx, p); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				res.Skipped = append(res.Skipped, p.Name)
				continue
			}
			return res, err
		}
		res.Created = append(res.Created, p.Name)
	}
	return res, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func placeholders(n int) string {
	if n == 0 {
		return "''"
	}
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
