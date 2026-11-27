package groups

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
)

// nodeInfo 是成员解析所需的节点字段。
type nodeInfo struct {
	ID, Name, Kind, SubscriptionID, Protocol, Region, DialerRef string
	Enabled                                                     bool
	Tags                                                        []string
}

// graph 是某一时刻全部组与节点的快照；校验时可替换其中一个组或节点的前置后整体检查。
type graph struct {
	groups    map[string]*Group
	order     []string // 组 id，按 sort_order
	nodes     map[string]*nodeInfo
	nodeOrder []string // 节点 id，按显示名
}

func loadGraph(ctx context.Context, db *sql.DB) (*graph, error) {
	g := &graph{groups: map[string]*Group{}, nodes: map[string]*nodeInfo{}}
	rows, err := db.QueryContext(ctx, `SELECT id, name, type, strict, sort_order, members, options, icon,
		created_at, updated_at FROM proxy_groups ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var gr Group
		var members, options string
		if err := rows.Scan(&gr.ID, &gr.Name, &gr.Type, &gr.Strict, &gr.SortOrder, &members, &options, &gr.Icon,
			&gr.CreatedAt, &gr.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(members), &gr.Members); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(options), &gr.Options); err != nil {
			rows.Close()
			return nil, err
		}
		_ = gr.normalize() // 填充默认值；库内数据已在写入时校验过
		g.groups[gr.ID] = &gr
		g.order = append(g.order, gr.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `SELECT id, display_name, kind, COALESCE(subscription_id, ''), protocol, region,
		dialer_ref, enabled, tags FROM proxy_nodes ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n nodeInfo
		var tags string
		if err := rows.Scan(&n.ID, &n.Name, &n.Kind, &n.SubscriptionID, &n.Protocol, &n.Region, &n.DialerRef,
			&n.Enabled, &tags); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &n.Tags)
		g.nodes[n.ID] = &n
		g.nodeOrder = append(g.nodeOrder, n.ID)
	}
	return g, rows.Err()
}

// MemberRef 是解析后的成员。
type MemberRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // node / group / builtin
}

// resolve 计算组的最终成员：显式成员（按顺序）+ 动态筛选的已启用节点；
// 为空时使用兜底成员（M4-04）。warnings 报告悬空引用与被禁用的显式节点。
func (g *graph) resolve(gr *Group) (members []MemberRef, usedFallback bool, warnings []string) {
	seen := map[string]bool{}
	add := func(m MemberRef) {
		if !seen[m.ID] {
			seen[m.ID] = true
			members = append(members, m)
		}
	}
	for _, ref := range gr.Members.Explicit {
		switch {
		case ref == Direct || ref == Reject:
			add(MemberRef{ID: ref, Name: ref, Kind: "builtin"})
		case g.groups[ref] != nil:
			add(MemberRef{ID: ref, Name: g.groups[ref].Name, Kind: "group"})
		case g.nodes[ref] != nil:
			if n := g.nodes[ref]; n.Enabled {
				add(MemberRef{ID: ref, Name: n.Name, Kind: "node"})
			} else {
				warnings = append(warnings, "节点「"+n.Name+"」已禁用，未计入")
			}
		default:
			warnings = append(warnings, "引用 "+ref+" 已不存在（节点或组被删除）")
		}
	}
	if f := gr.Members.Filter; f.Enabled {
		var re *regexp.Regexp
		if f.NameRegex != "" {
			re, _ = regexp.Compile(f.NameRegex)
		}
		for _, id := range g.nodeOrder {
			if n := g.nodes[id]; n.Enabled && f.match(n, re) {
				add(MemberRef{ID: id, Name: n.Name, Kind: "node"})
			}
		}
	}
	if len(members) == 0 {
		fb := gr.Options.EmptyFallback
		switch {
		case fb == Direct || fb == Reject || fb == "":
			members = []MemberRef{{ID: Reject, Name: Reject, Kind: "builtin"}}
			if fb == Direct {
				members[0] = MemberRef{ID: Direct, Name: Direct, Kind: "builtin"}
			}
		case g.groups[fb] != nil:
			members = []MemberRef{{ID: fb, Name: g.groups[fb].Name, Kind: "group"}}
		default:
			members = []MemberRef{{ID: Reject, Name: Reject, Kind: "builtin"}}
			warnings = append(warnings, "兜底组已不存在，按 REJECT 处理")
		}
		usedFallback = true
	}
	return members, usedFallback, warnings
}

func (f Filter) match(n *nodeInfo, re *regexp.Regexp) bool {
	in := func(list []string, v string) bool {
		if len(list) == 0 {
			return true
		}
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	hasAny := func(list, tags []string) bool {
		for _, t := range tags {
			for _, x := range list {
				if t == x {
					return true
				}
			}
		}
		return false
	}
	if !in(f.Regions, n.Region) || !in(f.Subscriptions, n.SubscriptionID) ||
		!in(f.Protocols, n.Protocol) || !in(f.Kinds, n.Kind) {
		return false
	}
	if len(f.Tags) > 0 && !hasAny(f.Tags, n.Tags) {
		return false
	}
	if hasAny(f.ExcludeTags, n.Tags) {
		return false
	}
	return re == nil || re.MatchString(n.Name)
}

// groupEdges 返回组在“组图”中的出边：显式成员中的组与内置项，以及兜底项。
// 兜底总计入：它决定了成员消失后的去向，严格组必须对此保守。
func (g *graph) groupEdges(gr *Group) []string {
	var out []string
	for _, ref := range gr.Members.Explicit {
		if ref == Direct || ref == Reject || g.groups[ref] != nil {
			out = append(out, ref)
		}
	}
	if fb := gr.Options.EmptyFallback; fb != "" {
		out = append(out, fb)
	}
	return out
}

func (g *graph) name(id string) string {
	if gr := g.groups[id]; gr != nil {
		return gr.Name
	}
	if n := g.nodes[id]; n != nil {
		return n.Name
	}
	return id
}

// validate 检查整张图（M4-05、M6-02）：
//  1. 组之间无环；
//  2. 严格组经任何路径都到不了 DIRECT；
//  3. 前置代理（节点或组）不形成回到自身的环。
func (g *graph) validate() error {
	// 1. 组环
	state := map[string]int{} // 0 未访问 1 访问中 2 完成
	var stack []string
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			i := indexOf(stack, id)
			return invalid("代理组形成环路：%s", g.path(append(stack[i:], id)))
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		for _, e := range g.groupEdges(g.groups[id]) {
			if g.groups[e] != nil {
				if err := visit(e); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	for _, id := range g.order {
		if err := visit(id); err != nil {
			return err
		}
	}

	// 2. 严格组不可达 DIRECT
	for _, id := range g.order {
		if !g.groups[id].Strict {
			continue
		}
		if p := g.findDirect(id, map[string]bool{}); p != nil {
			return invalid("严格组「%s」存在通往 DIRECT 的路径：%s", g.groups[id].Name, g.path(p))
		}
	}

	// 3. 前置代理环
	for _, id := range g.nodeOrder {
		if g.nodes[id].DialerRef == "" {
			continue
		}
		if p := g.dialerLoop(id, id, map[string]bool{}, []string{id}); p != nil {
			return invalid("前置代理形成环路：%s", g.path(p))
		}
	}
	return nil
}

func (g *graph) findDirect(id string, seen map[string]bool) []string {
	if id == Direct {
		return []string{Direct}
	}
	gr := g.groups[id]
	if gr == nil || seen[id] {
		return nil
	}
	seen[id] = true
	for _, e := range g.groupEdges(gr) {
		if p := g.findDirect(e, seen); p != nil {
			return append([]string{id}, p...)
		}
	}
	return nil
}

// dialerLoop 从节点 cur 沿前置代理展开（组展开为其节点成员），若回到 start 返回路径。
func (g *graph) dialerLoop(start, cur string, seen map[string]bool, path []string) []string {
	n := g.nodes[cur]
	if n == nil || n.DialerRef == "" {
		return nil
	}
	for _, next := range g.expandDialer(n.DialerRef, map[string]bool{}) {
		p := append(append([]string{}, path...), next...)
		last := next[len(next)-1]
		if last == start {
			return p
		}
		if seen[last] {
			continue
		}
		seen[last] = true
		if r := g.dialerLoop(start, last, seen, p); r != nil {
			return r
		}
	}
	return nil
}

// expandDialer 把前置引用展开成若干条到达节点的路径（组 → … → 节点）。
func (g *graph) expandDialer(ref string, seen map[string]bool) [][]string {
	if g.nodes[ref] != nil {
		return [][]string{{ref}}
	}
	gr := g.groups[ref]
	if gr == nil || seen[ref] {
		return nil
	}
	seen[ref] = true
	members, _, _ := g.resolve(gr)
	var out [][]string
	for _, m := range members {
		for _, p := range g.expandDialer(m.ID, seen) {
			out = append(out, append([]string{ref}, p...))
		}
	}
	return out
}

func (g *graph) path(ids []string) string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = g.name(id)
	}
	return strings.Join(names, " → ")
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}
