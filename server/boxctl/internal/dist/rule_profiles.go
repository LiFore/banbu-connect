package dist

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"boxctl/internal/gen"
	"boxctl/internal/groups"
)

// ---- 方案（ADR-062，ADR-064 扩展）----
//
// 节点、代理组在「节点」「代理组」页维护，是整个系统的池子；方案决定一组客户端拿到池子里的哪些节点、代理组，
// 以及线路选择、规则（五段）和远程规则集。「默认」也是一个方案（is_default），下发给没有分配方案的客户端兜底，
// 不能改名、删除，也不分配客户端。方案和分配随「发布」快照进版本，客户端拉配置时按自己的 ID 取对应的那份。
//
// 每个方案有唯一的随机代码；内容（不含名称和分配）每次变化记一个版本「代码-时间戳」，可以回滚到任一版本。

const (
	maxRuleProfiles   = 50
	maxProfileHistory = 50 // 每个方案保留的版本数
	defaultName       = "默认"
)

// ProfileContent 是方案的内容，也是版本快照的内容。Nodes、Groups 为 nil 表示全部；RuleSets 为 nil 表示客户端用内置默认。
type ProfileContent struct {
	Nodes      []string          `json:"nodes"`
	Groups     []string          `json:"groups"`
	Selections map[string]string `json:"selections"`
	Rules      []string          `json:"rules"` // 带分段标记
	RuleSets   []gen.RuleSet     `json:"rule_sets"`
}

// RuleProfile 是一个方案。
type RuleProfile struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Code    string `json:"code"`    // 方案唯一的随机代码
	Version string `json:"version"` // 当前版本：代码-时间戳
	Default bool   `json:"default"`
	ProfileContent
	Devices   []int64 `json:"devices"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// RuleVariant 是发布进版本里的方案（已按方案筛好节点和代理组，规则已去掉分段标记）。
type RuleVariant struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Version    string            `json:"version,omitempty"`
	Devices    []int64           `json:"devices"`
	Nodes      []map[string]any  `json:"nodes,omitempty"`
	Groups     []groups.Export   `json:"groups,omitempty"`
	Selections map[string]string `json:"selections,omitempty"`
	Rules      []string          `json:"rules"`
	RuleSets   []gen.RuleSet     `json:"rule_sets"`
	// Scoped 为 false 的旧版本快照没有节点、代理组和线路选择，沿用默认的。
	Scoped bool `json:"scoped,omitempty"`
}

const selectRuleProfile = `SELECT id, name, code, version, is_default, nodes, proxy_groups, selections, rules, rule_sets, devices, created_at, updated_at FROM rule_profiles`

func scanRuleProfile(sc interface{ Scan(...any) error }) (RuleProfile, error) {
	var p RuleProfile
	var nodes, gs, sel, rules, sets, devices string
	if err := sc.Scan(&p.ID, &p.Name, &p.Code, &p.Version, &p.Default, &nodes, &gs, &sel, &rules, &sets, &devices, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	_ = json.Unmarshal([]byte(nodes), &p.Nodes)
	_ = json.Unmarshal([]byte(gs), &p.Groups)
	_ = json.Unmarshal([]byte(sel), &p.Selections)
	_ = json.Unmarshal([]byte(rules), &p.Rules)
	_ = json.Unmarshal([]byte(sets), &p.RuleSets)
	_ = json.Unmarshal([]byte(devices), &p.Devices)
	p.ProfileContent = p.ProfileContent.filled()
	if p.Devices == nil {
		p.Devices = []int64{}
	}
	return p, nil
}

// filled 把 nil 的 map 和规则列表换成空值（Nodes、Groups、RuleSets 的 nil 有含义，不动）。
func (c ProfileContent) filled() ProfileContent {
	if c.Selections == nil {
		c.Selections = map[string]string{}
	}
	if c.Rules == nil {
		c.Rules = []string{}
	}
	return c
}

// RuleProfiles 返回所有方案，「默认」排第一。
func (s *Server) RuleProfiles(ctx context.Context) ([]RuleProfile, error) {
	rows, err := s.DB.QueryContext(ctx, selectRuleProfile+` ORDER BY is_default DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuleProfile{}
	for rows.Next() {
		p, err := scanRuleProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) ruleProfile(ctx context.Context, id int64) (RuleProfile, error) {
	p, err := scanRuleProfile(s.DB.QueryRowContext(ctx, selectRuleProfile+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// defaultProfile 返回「默认」方案；迁移时已建好，万一没有就补一行。
func (s *Server) defaultProfile(ctx context.Context) (RuleProfile, error) {
	p, err := scanRuleProfile(s.DB.QueryRowContext(ctx, selectRuleProfile+` WHERE is_default = 1`))
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	code, err := s.newProfileCode(ctx)
	if err != nil {
		return p, err
	}
	ver := s.nextVersion(ctx, 0, code)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO rule_profiles (name, code, version, is_default) VALUES (?, ?, ?, 1)`, defaultName, code, ver)
	if err != nil {
		return p, err
	}
	id, _ := res.LastInsertId()
	if err := s.addVersion(ctx, id, ver, ProfileContent{}.filled(), "初始版本"); err != nil {
		return p, err
	}
	return s.ruleProfile(ctx, id)
}

const codeAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newProfileCode 生成方案唯一的 6 位随机代码。
func (s *Server) newProfileCode(ctx context.Context) (string, error) {
	for range 20 {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		for i := range b {
			b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
		}
		var n int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM rule_profiles WHERE code = ?`, string(b)).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return string(b), nil
		}
	}
	return "", errors.New("生成方案代码失败")
}

// nextVersion 生成版本号「代码-时间戳」（服务器本地时间，精确到秒）；同一秒内再次保存时往后顺延一秒。
func (s *Server) nextVersion(ctx context.Context, id int64, code string) string {
	t := time.Now()
	for {
		v := code + "-" + t.Format("20060102150405")
		var n int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM rule_profile_versions WHERE profile_id = ? AND version = ?`, id, v).Scan(&n); err != nil || n == 0 {
			return v
		}
		t = t.Add(time.Second)
	}
}

func (s *Server) addVersion(ctx context.Context, id int64, version string, c ProfileContent, note string) error {
	body, _ := json.Marshal(c)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO rule_profile_versions (profile_id, version, body, note, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, version, string(body), note, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM rule_profile_versions WHERE profile_id = ? AND id NOT IN
		(SELECT id FROM rule_profile_versions WHERE profile_id = ? ORDER BY id DESC LIMIT ?)`, id, id, maxProfileHistory)
	return err
}

// normalizeRuleSets 整理并检查远程规则集；nil 原样返回（用内置默认）。
func normalizeRuleSets(list []gen.RuleSet) ([]gen.RuleSet, error) {
	if list == nil {
		return nil, nil
	}
	for i := range list {
		list[i].Name = strings.TrimSpace(list[i].Name)
		list[i].URL = strings.TrimSpace(list[i].URL)
		list[i].Target = strings.TrimSpace(list[i].Target)
		if list[i].Type == gen.TypeRuleSet {
			list[i].Type = "" // 默认值不存
		}
	}
	if err := gen.ValidateRuleSets(list); err != nil {
		return nil, &InputError{err.Error()}
	}
	return list, nil
}

// normalizeNames 去空白、去重、排序；nil 原样返回（全部）。
func normalizeNames(list []string) []string {
	if list == nil {
		return nil
	}
	out := []string{}
	for _, n := range list {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// normalizeContent 检查方案内容。
func normalizeContent(c ProfileContent) (ProfileContent, error) {
	var err error
	if c.Rules, err = NormalizeRules(c.Rules); err != nil {
		return c, err
	}
	if c.RuleSets, err = normalizeRuleSets(c.RuleSets); err != nil {
		return c, err
	}
	c.Nodes, c.Groups = normalizeNames(c.Nodes), normalizeNames(c.Groups)
	sel := map[string]string{}
	for g, m := range c.Selections {
		if g, m = strings.TrimSpace(g), strings.TrimSpace(m); g != "" && m != "" {
			sel[g] = m
		}
	}
	c.Selections = sel
	return c.filled(), nil
}

// normalizeProfile 检查名称、内容和分配；分配的客户端不能已在别的方案里。
func (s *Server) normalizeProfile(ctx context.Context, id int64, p *RuleProfile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len([]rune(p.Name)) > 40 {
		return &InputError{"方案名称要填，最多 40 字"}
	}
	if p.Name == defaultName {
		return &InputError{"「默认」是所有客户端共用的方案，换一个名称"}
	}
	var err error
	if p.ProfileContent, err = normalizeContent(p.ProfileContent); err != nil {
		return err
	}
	devices := []int64{}
	for _, d := range p.Devices {
		if d > 0 && !slices.Contains(devices, d) {
			devices = append(devices, d)
		}
	}
	slices.Sort(devices)
	p.Devices = devices
	others, err := s.RuleProfiles(ctx)
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.ID == id {
			continue
		}
		for _, d := range o.Devices {
			if slices.Contains(p.Devices, d) {
				var name string
				_ = s.DB.QueryRowContext(ctx, `SELECT name FROM devices WHERE id = ?`, d).Scan(&name)
				return &InputError{fmt.Sprintf("客户端「%s」已经在方案「%s」里，一台客户端只能用一个方案", name, o.Name)}
			}
		}
	}
	for _, d := range p.Devices {
		var revoked bool
		err := s.DB.QueryRowContext(ctx, `SELECT revoked FROM devices WHERE id = ?`, d).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked) {
			return &InputError{fmt.Sprintf("客户端 %d 不存在或已吊销", d)}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func profileUniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return &InputError{"已经有同名的方案"}
	}
	return err
}

func marshalContent(c ProfileContent) (nodes, gs, sel, rules, sets string) {
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return j(c.Nodes), j(c.Groups), j(c.Selections), j(c.Rules), j(c.RuleSets)
}

func sameContent(a, b ProfileContent) bool {
	x, _ := json.Marshal(a.filled())
	y, _ := json.Marshal(b.filled())
	return string(x) == string(y)
}

// saveContent 保存方案内容；和当前内容不同时记一个新版本。返回是否记了新版本。
func (s *Server) saveContent(ctx context.Context, p RuleProfile, c ProfileContent, note string) (bool, error) {
	if sameContent(p.ProfileContent, c) {
		return false, nil
	}
	ver := s.nextVersion(ctx, p.ID, p.Code)
	nodes, gs, sel, rules, sets := marshalContent(c)
	if _, err := s.DB.ExecContext(ctx, `UPDATE rule_profiles SET nodes = ?, proxy_groups = ?, selections = ?, rules = ?, rule_sets = ?, version = ?, updated_at = ? WHERE id = ?`,
		nodes, gs, sel, rules, sets, ver, time.Now().UTC().Format(time.RFC3339), p.ID); err != nil {
		return false, err
	}
	return true, s.addVersion(ctx, p.ID, ver, c, strings.TrimSpace(note))
}

// CreateRuleProfile 新建方案。copyFrom 不为 0 时内容从那个方案复制一份（含「默认」）。
func (s *Server) CreateRuleProfile(ctx context.Context, p RuleProfile, copyFrom int64) (RuleProfile, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM rule_profiles WHERE is_default = 0`).Scan(&n); err != nil {
		return p, err
	}
	if n >= maxRuleProfiles {
		return p, &InputError{fmt.Sprintf("方案最多 %d 个", maxRuleProfiles)}
	}
	note := "新建"
	if copyFrom != 0 {
		src, err := s.ruleProfile(ctx, copyFrom)
		if err != nil {
			return p, err
		}
		p.ProfileContent = src.ProfileContent
		note = fmt.Sprintf("从「%s」%s 复制", src.Name, src.Version)
	}
	if err := s.normalizeProfile(ctx, 0, &p); err != nil {
		return p, err
	}
	code, err := s.newProfileCode(ctx)
	if err != nil {
		return p, err
	}
	ver := code + "-" + time.Now().Format("20060102150405")
	nodes, gs, sel, rules, sets := marshalContent(p.ProfileContent)
	devices, _ := json.Marshal(p.Devices)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO rule_profiles (name, code, version, nodes, proxy_groups, selections, rules, rule_sets, devices)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.Name, code, ver, nodes, gs, sel, rules, sets, string(devices))
	if err != nil {
		return p, profileUniqueErr(err)
	}
	id, _ := res.LastInsertId()
	if err := s.addVersion(ctx, id, ver, p.ProfileContent, note); err != nil {
		return p, err
	}
	return s.ruleProfile(ctx, id)
}

// UpdateRuleProfile 整体保存方案（名称、内容、分配）；内容变了记新版本，note 是版本说明。
// 「默认」只保存内容，名称和分配忽略。
func (s *Server) UpdateRuleProfile(ctx context.Context, id int64, p RuleProfile, note string) (RuleProfile, error) {
	old, err := s.ruleProfile(ctx, id)
	if err != nil {
		return p, err
	}
	if old.Default {
		c, err := normalizeContent(p.ProfileContent)
		if err != nil {
			return p, err
		}
		if _, err := s.saveContent(ctx, old, c, note); err != nil {
			return p, err
		}
		return s.ruleProfile(ctx, id)
	}
	if err := s.normalizeProfile(ctx, id, &p); err != nil {
		return p, err
	}
	devices, _ := json.Marshal(p.Devices)
	if _, err := s.DB.ExecContext(ctx, `UPDATE rule_profiles SET name = ?, devices = ?, updated_at = ? WHERE id = ?`,
		p.Name, string(devices), time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return p, profileUniqueErr(err)
	}
	if _, err := s.saveContent(ctx, old, p.ProfileContent, note); err != nil {
		return p, err
	}
	return s.ruleProfile(ctx, id)
}

func (s *Server) DeleteRuleProfile(ctx context.Context, id int64) error {
	p, err := s.ruleProfile(ctx, id)
	if err != nil {
		return err
	}
	if p.Default {
		return &InputError{"「默认」方案不能删除"}
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM rule_profile_versions WHERE profile_id = ?`, id); err != nil {
		return err
	}
	return s.execOne(ctx, `DELETE FROM rule_profiles WHERE id = ?`, id)
}

// ProfileVersion 是方案的一个历史版本。
type ProfileVersion struct {
	Version   string          `json:"version"`
	Note      string          `json:"note"`
	CreatedAt string          `json:"created_at"`
	Current   bool            `json:"current"`
	Content   *ProfileContent `json:"content,omitempty"`
	// 摘要：nil 的节点、代理组记为 -1（全部）；规则不算分段标记；规则集 -1 表示内置默认。
	Nodes    int `json:"nodes"`
	Groups   int `json:"groups"`
	Rules    int `json:"rules"`
	RuleSets int `json:"rule_sets"`
}

func countOrAll[T any](list []T) int {
	if list == nil {
		return -1
	}
	return len(list)
}

// ProfileVersions 返回方案的版本，最新的在前。
func (s *Server) ProfileVersions(ctx context.Context, id int64) ([]ProfileVersion, error) {
	p, err := s.ruleProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT version, note, created_at, body FROM rule_profile_versions WHERE profile_id = ? ORDER BY id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileVersion{}
	for rows.Next() {
		var v ProfileVersion
		var body string
		if err := rows.Scan(&v.Version, &v.Note, &v.CreatedAt, &body); err != nil {
			return nil, err
		}
		var c ProfileContent
		_ = json.Unmarshal([]byte(body), &c)
		c = c.filled()
		v.Current = v.Version == p.Version
		v.Nodes, v.Groups, v.Rules, v.RuleSets = countOrAll(c.Nodes), countOrAll(c.Groups), len(stripSegments(c.Rules)), countOrAll(c.RuleSets)
		v.Content = &c
		out = append(out, v)
	}
	return out, rows.Err()
}

// RollbackProfile 把方案内容恢复成某个版本，记为一个新版本（客户端分配和名称不变）。
func (s *Server) RollbackProfile(ctx context.Context, id int64, version string) (RuleProfile, error) {
	p, err := s.ruleProfile(ctx, id)
	if err != nil {
		return p, err
	}
	var body string
	err = s.DB.QueryRowContext(ctx, `SELECT body FROM rule_profile_versions WHERE profile_id = ? AND version = ?`, id, version).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return p, &InputError{"版本不存在：" + version}
	}
	if err != nil {
		return p, err
	}
	var c ProfileContent
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return p, err
	}
	if c, err = normalizeContent(c); err != nil {
		return p, err
	}
	if sameContent(p.ProfileContent, c) {
		return p, &InputError{"当前内容和这个版本相同，不需要回滚"}
	}
	if _, err := s.saveContent(ctx, p, c, "回滚到 "+version); err != nil {
		return p, err
	}
	return s.ruleProfile(ctx, id)
}

// ---- 默认方案的内容（旧接口和导入预置数据用）----

func (s *Server) Selections(ctx context.Context) (map[string]string, error) {
	p, err := s.defaultProfile(ctx)
	return p.Selections, err
}

func (s *Server) Rules(ctx context.Context) ([]string, error) {
	p, err := s.defaultProfile(ctx)
	return p.Rules, err
}

// RuleSets 返回默认方案的规则集；没配置过时返回 nil（客户端用内置默认）。
func (s *Server) RuleSets(ctx context.Context) ([]gen.RuleSet, error) {
	p, err := s.defaultProfile(ctx)
	return p.RuleSets, err
}

func (s *Server) updateDefault(ctx context.Context, note string, edit func(*ProfileContent)) error {
	p, err := s.defaultProfile(ctx)
	if err != nil {
		return err
	}
	c := p.ProfileContent
	c.Selections = maps(c.Selections)
	edit(&c)
	if c, err = normalizeContent(c); err != nil {
		return err
	}
	_, err = s.saveContent(ctx, p, c, note)
	return err
}

func maps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Server) SetSelections(ctx context.Context, sel map[string]string) error {
	return s.updateDefault(ctx, "修改线路选择", func(c *ProfileContent) { c.Selections = sel })
}

func (s *Server) SetRules(ctx context.Context, rules []string) error {
	return s.updateDefault(ctx, "修改规则", func(c *ProfileContent) { c.Rules = rules })
}

// SetRuleSets 保存默认方案的规则集；list 为 nil 表示恢复内置默认。
func (s *Server) SetRuleSets(ctx context.Context, list []gen.RuleSet) error {
	return s.updateDefault(ctx, "修改远程规则集", func(c *ProfileContent) { c.RuleSets = list })
}

// ---- 按方案筛选节点和代理组 ----

// builtinTargets 是内核自带的出站，代理组成员里可以直接引用。
var builtinTargets = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true}

// Scoped 是按方案筛选后的节点、代理组和线路选择。
type Scoped struct {
	Nodes      []map[string]any
	Groups     []groups.Export
	Selections map[string]string
	// Excluded 是系统里有、方案里没有的代理组和节点名：指向它们的规则下发前去掉。
	Excluded map[string]bool
}

// scope 按方案内容筛选节点和代理组：
//   - 选中节点的前置代理（dialer-proxy）自动带上，否则节点不可用；
//   - 代理组成员只保留方案里有的节点、代理组和内置出站；筛选后没有成员的组去掉（本来就为空的不动）；
//   - 线路选择只保留还存在的组和成员。
func scope(allNodes []map[string]any, allGroups []groups.Export, c ProfileContent) Scoped {
	nodeByName := map[string]map[string]any{}
	for _, n := range allNodes {
		name, _ := n["name"].(string)
		nodeByName[name] = n
	}
	groupByName := map[string]groups.Export{}
	for _, g := range allGroups {
		groupByName[g.Name] = g
	}
	inNodes, inGroups := map[string]bool{}, map[string]bool{}
	for name := range nodeByName {
		if c.Nodes == nil || slices.Contains(c.Nodes, name) {
			inNodes[name] = true
		}
	}
	for name := range groupByName {
		if c.Groups == nil || slices.Contains(c.Groups, name) {
			inGroups[name] = true
		}
	}
	// 前置代理闭包。
	for changed := true; changed; {
		changed = false
		for name := range inNodes {
			d, _ := nodeByName[name]["dialer-proxy"].(string)
			if d == "" {
				continue
			}
			if _, ok := nodeByName[d]; ok && !inNodes[d] {
				inNodes[d], changed = true, true
			}
			if _, ok := groupByName[d]; ok && !inGroups[d] {
				inGroups[d], changed = true, true
			}
		}
	}
	// 成员筛选；筛空的组去掉后，引用它的组还要再筛一轮。
	members := map[string][]string{}
	for changed := true; changed; {
		changed = false
		for name := range inGroups {
			g := groupByName[name]
			kept := []string{}
			for _, m := range g.Members {
				if inNodes[m] || inGroups[m] || builtinTargets[m] {
					kept = append(kept, m)
				}
			}
			if len(kept) == 0 && len(g.Members) > 0 {
				delete(inGroups, name)
				changed = true
				continue
			}
			members[name] = kept
		}
	}
	out := Scoped{Nodes: []map[string]any{}, Groups: []groups.Export{}, Selections: map[string]string{}, Excluded: map[string]bool{}}
	for _, n := range allNodes {
		name, _ := n["name"].(string)
		if inNodes[name] {
			out.Nodes = append(out.Nodes, n)
		} else {
			out.Excluded[name] = true
		}
	}
	for _, g := range allGroups {
		if !inGroups[g.Name] {
			out.Excluded[g.Name] = true
			continue
		}
		g.Members = members[g.Name]
		out.Groups = append(out.Groups, g)
		if want, ok := c.Selections[g.Name]; ok && slices.Contains(g.Members, want) {
			out.Selections[g.Name] = want
		}
	}
	return out
}

// scopeRules 去掉分段标记，以及指向方案外代理组、节点的规则（客户端生成配置时会因目标不存在而失败）。
// 规则集同理：目标在方案外的去掉。
func scopeRules(rules []string, sets []gen.RuleSet, excluded map[string]bool) ([]string, []gen.RuleSet) {
	out := []string{}
	for _, r := range stripSegments(rules) {
		if t := ruleTarget(r); t != "" && excluded[t] {
			continue
		}
		out = append(out, r)
	}
	if sets == nil {
		return out, nil
	}
	keptSets := []gen.RuleSet{}
	for _, rs := range sets {
		if !excluded[rs.Target] {
			keptSets = append(keptSets, rs)
		}
	}
	return out, keptSets
}

// ruleTarget 取规则的出口：最后一段，带 no-resolve 等参数时取倒数第二段。
func ruleTarget(r string) string {
	parts := strings.Split(r, ",")
	for i := len(parts) - 1; i >= 1; i-- {
		p := strings.TrimSpace(parts[i])
		switch strings.ToLower(p) {
		case "no-resolve", "src":
			continue
		}
		if strings.HasSuffix(p, ")") {
			return "" // 逻辑规则的子条件，不是出口
		}
		return p
	}
	return ""
}

// ruleVariants 是草稿里的方案：没有分配客户端的方案不下发。已吊销的客户端不算。
func (s *Server) ruleVariants(ctx context.Context, allNodes []map[string]any, allGroups []groups.Export) ([]RuleVariant, error) {
	list, err := s.RuleProfiles(ctx)
	if err != nil {
		return nil, err
	}
	revoked := map[int64]bool{}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM devices WHERE revoked = 1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		revoked[id] = true
	}
	rows.Close()
	var out []RuleVariant
	for _, p := range list {
		if p.Default {
			continue
		}
		devices := slices.DeleteFunc(slices.Clone(p.Devices), func(d int64) bool { return revoked[d] })
		if len(devices) == 0 {
			continue
		}
		sc := scope(allNodes, allGroups, p.ProfileContent)
		rules, sets := scopeRules(p.Rules, p.RuleSets, sc.Excluded)
		out = append(out, RuleVariant{ID: p.ID, Name: p.Name, Version: p.Version, Devices: devices, Scoped: true,
			Nodes: sc.Nodes, Groups: sc.Groups, Selections: sc.Selections, Rules: rules, RuleSets: sets})
	}
	return out, nil
}

// ForDevice 返回下发给这台客户端的配置：分配了方案时整份替换节点、代理组、线路选择、规则和远程规则集；不带任何方案的内容。
func (p Profile) ForDevice(deviceID int64) Profile {
	out := p
	out.Variants = nil
	for _, v := range p.Variants {
		if slices.Contains(v.Devices, deviceID) {
			out.Rules, out.RuleSets, out.RuleProfile, out.ProfileVersion = v.Rules, v.RuleSets, v.Name, v.Version
			if v.Scoped {
				out.Nodes, out.Groups, out.Selections = v.Nodes, v.Groups, v.Selections
			}
			break
		}
	}
	return out
}

// ruleProfileRoutes：管理页面的方案。
func (s *Server) ruleProfileRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	pathID := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return 0, false
		}
		return id, true
	}
	mux.Handle("GET /api/v1/dist/rule-profiles", authed(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.defaultProfile(r.Context()); err != nil {
			fail(w, err)
			return
		}
		list, err := s.RuleProfiles(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/dist/rule-profiles", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RuleProfile
			CopyFrom    int64 `json:"copy_from"`
			CopyDefault bool  `json:"copy_default"` // 旧参数
		}
		if !decode(w, r, &in) {
			return
		}
		if in.CopyDefault && in.CopyFrom == 0 {
			d, err := s.defaultProfile(r.Context())
			if err != nil {
				fail(w, err)
				return
			}
			in.CopyFrom = d.ID
		}
		p, err := s.CreateRuleProfile(r.Context(), in.RuleProfile, in.CopyFrom)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.Handle("PUT /api/v1/dist/rule-profiles/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			RuleProfile
			Note string `json:"note"`
		}
		if !decode(w, r, &in) {
			return
		}
		p, err := s.UpdateRuleProfile(r.Context(), id, in.RuleProfile, in.Note)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.Handle("DELETE /api/v1/dist/rule-profiles/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := s.DeleteRuleProfile(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("GET /api/v1/dist/rule-profiles/{id}/versions", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		list, err := s.ProfileVersions(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/dist/rule-profiles/{id}/rollback", authed(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Version string `json:"version"`
		}
		if !decode(w, r, &in) {
			return
		}
		p, err := s.RollbackProfile(r.Context(), id, in.Version)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
}
