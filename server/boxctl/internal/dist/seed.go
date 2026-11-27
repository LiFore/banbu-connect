package dist

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"boxctl/internal/groups"
	"boxctl/internal/nodes"
)

// seededKey 标记预置配置已经导入过，之后不再导入。
const seededKey = "dist_seeded"

// strictGroups 是客户端 Kill Switch 默认保护的服务所用的组（killswitch.defaultConfig），
// 必须是严格组，否则客户端生成配置会失败。
var strictGroups = map[string]bool{"Claude": true, "ChatGPT": true}

// SeedResult 是一次预置导入或导出的数量。
type SeedResult struct {
	Nodes, Groups, Rules int
}

type seedGroup struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	URL            string `yaml:"url,omitempty"`
	Interval       int    `yaml:"interval,omitempty"`
	Tolerance      int    `yaml:"tolerance,omitempty"`
	Timeout        int    `yaml:"timeout,omitempty"`
	MaxFailedTimes int    `yaml:"max-failed-times,omitempty"`
	Lazy           bool   `yaml:"lazy,omitempty"`
	Strategy       string `yaml:"strategy,omitempty"`
	Hidden         bool   `yaml:"hidden,omitempty"`
	Icon           string `yaml:"icon,omitempty"`
	// Strict 是 boxctl 的扩展字段（Mihomo 忽略），由 ExportSeed 写出。
	Strict  bool     `yaml:"x-strict,omitempty"`
	Proxies []string `yaml:"proxies"`
}

// seedFile 是预置的 Mihomo 配置。x-selections 是 boxctl 的扩展字段：线路（组 → 默认成员），由 ExportSeed 写出。
type seedFile struct {
	Proxies     []map[string]any  `yaml:"proxies"`
	ProxyGroups []seedGroup       `yaml:"proxy-groups"`
	Selections  map[string]string `yaml:"x-selections,omitempty"`
}

type seedData struct {
	proxies    []map[string]any
	groups     []seedGroup
	rules      []string
	selections map[string]string
}

// Seed 在管理服务第一次启动、还没有任何节点和代理组时，导入 dir 下的预置配置：
//   - *.yaml / *.yml：Mihomo 配置，取其中的 proxies（节点，含 dialer-proxy）、proxy-groups（代理组）
//     和扩展字段 x-selections（线路）
//   - *.txt：每行一条规则，作为下发规则
//
// 预置文件含节点凭据，放在数据目录里，不进仓库。同名节点或组以文件名排序靠前的为准。
// 导入失败时清空已导入的内容，下次启动重试。目录不存在或没有文件时什么都不做。
func (s *Server) Seed(ctx context.Context, dir string) (SeedResult, bool, error) {
	var done, existing int
	if err := s.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM settings WHERE key = ?),
		(SELECT COUNT(*) FROM proxy_nodes) + (SELECT COUNT(*) FROM proxy_groups)`, seededKey).Scan(&done, &existing); err != nil {
		return SeedResult{}, false, err
	}
	if done > 0 || existing > 0 {
		return SeedResult{}, false, nil
	}
	data, err := readSeed(dir)
	if err != nil {
		return SeedResult{}, false, err
	}
	if len(data.proxies) == 0 && len(data.groups) == 0 && len(data.rules) == 0 {
		return SeedResult{}, false, nil
	}
	res, err := s.applySeed(ctx, data)
	if err != nil {
		// 导入前两张表都是空的，清空即回到导入前。
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM proxy_groups`)
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM proxy_nodes`)
		_ = s.updateDefault(ctx, "导入预置数据失败，清空", func(c *ProfileContent) { c.Rules, c.Selections = nil, nil })
		return SeedResult{}, false, err
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, '1')`, seededKey); err != nil {
		return SeedResult{}, false, err
	}
	return res, true, nil
}

func readSeed(dir string) (seedData, error) {
	var data seedData
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return data, nil
	}
	if err != nil {
		return data, err
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	seenProxy, seenGroup := map[string]bool{}, map[string]bool{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		switch strings.ToLower(filepath.Ext(name)) {
		case ".yaml", ".yml":
			b, err := os.ReadFile(path)
			if err != nil {
				return data, err
			}
			var doc seedFile
			if err := yaml.Unmarshal(b, &doc); err != nil {
				return data, fmt.Errorf("%s：%w", name, err)
			}
			for _, p := range doc.Proxies {
				n, _ := p["name"].(string)
				if n = strings.TrimSpace(n); n == "" || seenProxy[n] {
					continue
				}
				seenProxy[n] = true
				for k := range p {
					if strings.HasPrefix(k, "_") { // 订阅工具附加的字段，如 _subName
						delete(p, k)
					}
				}
				data.proxies = append(data.proxies, p)
			}
			for _, g := range doc.ProxyGroups {
				if g.Name = strings.TrimSpace(g.Name); g.Name == "" || seenGroup[g.Name] {
					continue
				}
				seenGroup[g.Name] = true
				data.groups = append(data.groups, g)
			}
			for g, m := range doc.Selections {
				if data.selections == nil {
					data.selections = map[string]string{}
				}
				if _, ok := data.selections[g]; !ok {
					data.selections[g] = m
				}
			}
		case ".txt":
			f, err := os.Open(path)
			if err != nil {
				return data, err
			}
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if r := strings.TrimSpace(sc.Text()); r != "" && !strings.HasPrefix(r, "#") {
					data.rules = append(data.rules, r)
				}
			}
			f.Close()
			if err := sc.Err(); err != nil {
				return data, err
			}
		}
	}
	return data, nil
}

func (s *Server) applySeed(ctx context.Context, data seedData) (SeedResult, error) {
	// 节点：dialer-proxy 指向的组要等组建好后再设置。
	dialers := map[string]string{}
	for _, p := range data.proxies {
		name, _ := p["name"].(string)
		if d, _ := p["dialer-proxy"].(string); d != "" {
			dialers[name] = d
		}
		delete(p, "dialer-proxy")
	}
	ids := map[string]string{groups.Direct: groups.Direct, groups.Reject: groups.Reject}
	if len(data.proxies) > 0 {
		text, err := yaml.Marshal(map[string]any{"proxies": data.proxies})
		if err != nil {
			return SeedResult{}, err
		}
		for _, c := range s.Nodes.Import(string(text)) {
			if c.Error != "" {
				return SeedResult{}, fmt.Errorf("预置节点「%s」：%s", c.DisplayName, c.Error)
			}
			d, err := s.Nodes.Create(ctx, nodes.Input{Kind: c.Kind, DisplayName: c.DisplayName, Region: c.Region, Mapping: c.Mapping})
			if err != nil {
				return SeedResult{}, fmt.Errorf("预置节点「%s」：%w", c.DisplayName, err)
			}
			ids[c.DisplayName] = d.ID
		}
	}

	// 代理组：先建空组，再填成员，组之间可以互相引用而不受顺序影响。
	members := strictMembers(data.groups)
	for _, g := range data.groups {
		v, err := s.Groups.Create(ctx, groups.Group{Name: g.Name, Type: g.Type, Strict: isStrict(g), Icon: g.Icon,
			Options: groups.Options{URL: g.URL, Interval: g.Interval, Tolerance: g.Tolerance, Timeout: g.Timeout,
				MaxFailedTimes: g.MaxFailedTimes, Lazy: g.Lazy, Strategy: g.Strategy, Hidden: g.Hidden}})
		if err != nil {
			return SeedResult{}, fmt.Errorf("预置代理组「%s」：%w", g.Name, err)
		}
		ids[g.Name] = v.ID
	}
	for _, g := range data.groups {
		v, err := s.Groups.Get(ctx, ids[g.Name])
		if err != nil {
			return SeedResult{}, err
		}
		in := v.Group
		in.Members.Explicit = nil
		for _, m := range members[g.Name] {
			id, ok := ids[m]
			if !ok {
				return SeedResult{}, fmt.Errorf("预置代理组「%s」的成员「%s」不存在", g.Name, m)
			}
			in.Members.Explicit = append(in.Members.Explicit, id)
		}
		if _, err := s.Groups.Update(ctx, in.ID, in); err != nil {
			return SeedResult{}, fmt.Errorf("预置代理组「%s」：%w", g.Name, err)
		}
	}

	for node, dialer := range dialers {
		ref, ok := ids[dialer]
		if !ok || ref == groups.Direct || ref == groups.Reject {
			return SeedResult{}, fmt.Errorf("预置节点「%s」的前置代理「%s」不存在", node, dialer)
		}
		if _, err := s.Nodes.Patch(ctx, ids[node], nodes.Attrs{DialerRef: &ref}); err != nil {
			return SeedResult{}, fmt.Errorf("预置节点「%s」的前置代理：%w", node, err)
		}
	}

	if err := s.SetRules(ctx, data.rules); err != nil {
		return SeedResult{}, err
	}
	if len(data.selections) > 0 {
		if err := s.SetSelections(ctx, data.selections); err != nil {
			return SeedResult{}, err
		}
	}
	return SeedResult{Nodes: len(data.proxies), Groups: len(data.groups), Rules: len(data.rules)}, nil
}

func isStrict(g seedGroup) bool { return g.Strict || strictGroups[g.Name] }

// strictMembers 返回每个组的成员名。严格组不能走到 DIRECT：成员里能走到 DIRECT 的组
// （如含 DIRECT 的「代理模式」）换成它自己除 DIRECT 以外的成员，直到不再能走到 DIRECT。
func strictMembers(grps []seedGroup) map[string][]string {
	byName := map[string]seedGroup{}
	for _, g := range grps {
		byName[g.Name] = g
	}
	reach := map[string]bool{}
	var reachesDirect func(name string, path map[string]bool) bool
	reachesDirect = func(name string, path map[string]bool) bool {
		if name == groups.Direct {
			return true
		}
		g, ok := byName[name]
		if !ok || path[name] {
			return false
		}
		if v, ok := reach[name]; ok {
			return v
		}
		path[name] = true
		defer delete(path, name)
		for _, m := range g.Proxies {
			if reachesDirect(m, path) {
				reach[name] = true
				return true
			}
		}
		reach[name] = false
		return false
	}

	out := map[string][]string{}
	for _, g := range grps {
		if !isStrict(g) {
			out[g.Name] = g.Proxies
			continue
		}
		seen := map[string]bool{}
		var list []string
		var add func(m string, depth int)
		add = func(m string, depth int) {
			if m == groups.Direct || seen[m] || depth > 8 {
				return
			}
			seen[m] = true
			if reachesDirect(m, map[string]bool{}) {
				for _, sub := range byName[m].Proxies {
					add(sub, depth+1)
				}
				return
			}
			list = append(list, m)
		}
		for _, m := range g.Proxies {
			add(m, 0)
		}
		out[g.Name] = list
	}
	return out
}

// ExportSeed 把服务端当前的节点、代理组、线路和规则写成预置文件（config.yaml、rules.txt），
// 新部署的管理服务把它们放进数据目录的 seed/ 即可得到同样的初始配置。
// 代理组的动态成员按当前解析结果写成固定成员；停用的节点不导出。文件含凭据，权限 0600。
func (s *Server) ExportSeed(ctx context.Context, dir string) (SeedResult, error) {
	p, err := s.Draft(ctx)
	if err != nil {
		return SeedResult{}, err
	}
	views, err := s.Groups.List(ctx)
	if err != nil {
		return SeedResult{}, err
	}
	icons := map[string]string{}
	for _, v := range views {
		icons[v.Name] = v.Icon
	}
	doc := seedFile{Proxies: p.Nodes, Selections: p.Selections}
	for _, g := range p.Groups {
		o := g.Options
		sg := seedGroup{Name: g.Name, Type: g.Type, Hidden: o.Hidden, Icon: icons[g.Name], Strict: g.Strict, Proxies: g.Members}
		if g.Type != groups.TypeSelect {
			sg.URL, sg.Interval, sg.Tolerance, sg.Timeout = o.URL, o.Interval, o.Tolerance, o.Timeout
			sg.MaxFailedTimes, sg.Lazy, sg.Strategy = o.MaxFailedTimes, o.Lazy, o.Strategy
		}
		if sg.Proxies == nil {
			sg.Proxies = []string{}
		}
		doc.ProxyGroups = append(doc.ProxyGroups, sg)
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return SeedResult{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return SeedResult{}, err
	}
	header := "# boxctl server 预置配置，由 `boxctl server export-seed` 生成。含节点凭据，不要放进仓库。\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), append([]byte(header), body...), 0o600); err != nil {
		return SeedResult{}, err
	}
	rules := "# 下发规则，每行一条，放在客户端自定义规则之前。\n" + strings.Join(p.Rules, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rules.txt"), []byte(rules), 0o600); err != nil {
		return SeedResult{}, err
	}
	return SeedResult{Nodes: len(p.Nodes), Groups: len(p.Groups), Rules: len(p.Rules)}, nil
}
