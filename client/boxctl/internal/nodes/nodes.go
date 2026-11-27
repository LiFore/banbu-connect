// Package nodes 管理代理节点（M3）：列表、手工/ISP 节点增删改、导入、前置代理与连通性测试。
// 订阅节点由 internal/subscriptions 写入，这里只允许修改其启用状态、标签与前置代理。
package nodes

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"boxctl/internal/refs"
	"boxctl/internal/secret"
	"boxctl/internal/sub"
)

const (
	KindSubscription = "subscription"
	KindManual       = "manual"
	KindISP          = "isp"
	TagISP           = "isp"

	maxChainDepth = 8
	timeLayout    = time.RFC3339
)

var (
	ErrNotFound = errors.New("节点不存在")
	// 这些名字在 Mihomo 中有特殊含义，不能作为节点名
	reservedNames = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true,
		"COMPATIBLE": true, "GLOBAL": true}
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{fmt.Sprintf(format, a...)} }

// SecretAAD 是节点凭据密文绑定的 AAD；订阅与节点两处读写必须一致。
func SecretAAD(id string) string { return "proxy_nodes.secret:" + id }

type Node struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	SubscriptionID   string   `json:"subscription_id"`
	SubscriptionName string   `json:"subscription_name"`
	Protocol         string   `json:"protocol"`
	DisplayName      string   `json:"display_name"`
	Region           string   `json:"region"`
	Server           string   `json:"server"`
	Port             int      `json:"port"`
	Enabled          bool     `json:"enabled"`
	Tags             []string `json:"tags"`
	DialerRef        string   `json:"dialer_ref"`
	DialerName       string   `json:"dialer_name"`
	UpdatedAt        string   `json:"updated_at"`
}

// Detail 带完整配置；凭据默认为掩码（M1-08）。
type Detail struct {
	Node
	Mapping map[string]any `json:"mapping"`
}

type Input struct {
	Kind        string         `json:"kind"`
	DisplayName string         `json:"display_name"`
	Region      string         `json:"region"` // 空则按名称自动识别
	Mapping     map[string]any `json:"mapping"`
	DialerRef   string         `json:"dialer_ref"`
	Tags        []string       `json:"tags"`
	Enabled     *bool          `json:"enabled"`
}

// Attrs 是所有类型节点都可修改的属性；nil 表示不修改。
type Attrs struct {
	Enabled   *bool     `json:"enabled"`
	Tags      *[]string `json:"tags"`
	DialerRef *string   `json:"dialer_ref"`
}

// DialerGraph 由代理组服务实现：前置代理可以是代理组，环路检测需要组与节点的整张图。
type DialerGraph interface {
	// CheckNode 以节点保存后的属性做整图校验；返回非空 msg 表示不合法。
	CheckNode(ctx context.Context, n refs.NodeAttrs) (msg string, err error)
	// CheckGraph 校验当前库内整张图（批量修改后使用）。
	CheckGraph(ctx context.Context) (msg string, err error)
	// DialerTarget 返回进程内测试时前置引用实际使用的节点及展示标签。
	DialerTarget(ctx context.Context, ref string) (nodeID, label string, err error)
}

type Service struct {
	db *sql.DB
	// Groups 为空时前置代理只能是节点（单独使用节点服务的测试场景）。
	Groups   DialerGraph
	secrets  *secret.Box
	Validate sub.Validator
	DelayURL string
	ExitURL  string
	now      func() time.Time
}

func New(db *sql.DB, secrets *secret.Box) *Service {
	return &Service{db: db, secrets: secrets, Validate: sub.ValidateWithMihomo,
		DelayURL: DefaultDelayURL, ExitURL: DefaultExitURL, now: time.Now}
}

const selectNode = `SELECT n.id, n.kind, COALESCE(n.subscription_id, ''), COALESCE(s.name, ''), n.protocol,
	n.display_name, n.region, n.server, n.port, n.enabled, n.tags, n.dialer_ref, COALESCE(d.display_name, gd.name, ''),
	n.updated_at, n.spec, n.secret
	FROM proxy_nodes n
	LEFT JOIN subscriptions s ON s.id = n.subscription_id
	LEFT JOIN proxy_nodes d ON d.id = n.dialer_ref
	LEFT JOIN proxy_groups gd ON gd.id = n.dialer_ref`

type row struct {
	Node
	spec   string
	secret []byte
}

func scanRow(sc interface{ Scan(...any) error }) (row, error) {
	var r row
	var tags string
	err := sc.Scan(&r.ID, &r.Kind, &r.SubscriptionID, &r.SubscriptionName, &r.Protocol, &r.DisplayName,
		&r.Region, &r.Server, &r.Port, &r.Enabled, &tags, &r.DialerRef, &r.DialerName, &r.UpdatedAt,
		&r.spec, &r.secret)
	if err != nil {
		return row{}, err
	}
	if err := json.Unmarshal([]byte(tags), &r.Tags); err != nil || r.Tags == nil {
		r.Tags = []string{}
	}
	return r, nil
}

// List 返回全部节点摘要（不含配置与凭据）。前端分页与筛选。
func (s *Service) List(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, selectNode+` ORDER BY n.kind = 'subscription', s.name, n.region, n.display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r.Node)
	}
	return out, rows.Err()
}

func (s *Service) get(ctx context.Context, id string) (row, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, selectNode+` WHERE n.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, ErrNotFound
	}
	return r, err
}

// mappingOf 还原完整配置（含凭据）。
func (s *Service) mappingOf(r row) (map[string]any, error) {
	public := map[string]any{}
	if err := json.Unmarshal([]byte(r.spec), &public); err != nil {
		return nil, err
	}
	secrets := map[string]any{}
	if len(r.secret) > 0 {
		plain, err := s.secrets.Open(r.secret, SecretAAD(r.ID))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(plain, &secrets); err != nil {
			return nil, err
		}
	}
	return sub.MergeSecrets(public, secrets), nil
}

// Export 返回已启用节点的完整 Mihomo 配置（含凭据），供配置生成器使用：
// 名称用显示名，前置代理转换成名称。
func (s *Service) Export(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, selectNode+` WHERE n.enabled = 1 ORDER BY n.display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		m, err := s.mappingOf(r)
		if err != nil {
			return nil, err
		}
		m["name"] = r.DisplayName
		delete(m, "dialer-proxy")
		if r.DialerName != "" {
			m["dialer-proxy"] = r.DialerName
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, id string, reveal bool) (Detail, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	m, err := s.mappingOf(r)
	if err != nil {
		return Detail{}, err
	}
	if !reveal {
		m = maskSecrets(m)
	}
	return Detail{Node: r.Node, Mapping: m}, nil
}

func maskSecrets(m map[string]any) map[string]any {
	public, secrets := sub.SplitSecrets(m)
	masked := map[string]any{}
	for k, v := range secrets {
		if s, ok := v.(string); ok && s == "" {
			masked[k] = ""
		} else {
			masked[k] = secretMaskStr
		}
	}
	return sub.MergeSecrets(public, masked)
}

// unmask 把输入中仍为掩码的凭据替换为已保存的值（编辑时未改动密码）。
func unmask(in, stored map[string]any) map[string]any {
	public, secrets := sub.SplitSecrets(in)
	_, old := sub.SplitSecrets(stored)
	for k, v := range secrets {
		if v == secretMaskStr {
			if ov, ok := old[k]; ok {
				secrets[k] = ov
			} else {
				delete(secrets, k)
			}
		}
	}
	return sub.MergeSecrets(public, secrets)
}

func (s *Service) Create(ctx context.Context, in Input) (Detail, error) {
	id := "m_" + randomHex(8)
	if err := s.prepare(ctx, id, &in, nil); err != nil {
		return Detail{}, err
	}
	public, secrets := sub.SplitSecrets(in.Mapping)
	spec, _ := json.Marshal(public)
	secJSON, _ := json.Marshal(secrets)
	ct, err := s.secrets.Seal(secJSON, SecretAAD(id))
	if err != nil {
		return Detail{}, err
	}
	tags, _ := json.Marshal(in.Tags)
	enabled := in.Enabled == nil || *in.Enabled
	now := s.stamp()
	_, err = s.db.ExecContext(ctx, `INSERT INTO proxy_nodes
		(id, kind, protocol, display_name, region, server, port, enabled, spec, secret, dialer_ref, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Kind, in.Mapping["type"], in.DisplayName, in.Region, in.Mapping["server"], sub.PortOf(in.Mapping),
		enabled, string(spec), ct, in.DialerRef, string(tags), now, now)
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, id, false)
}

// Update 全量修改手工/ISP 节点。订阅节点返回校验错误（其配置随订阅更新覆盖）。
func (s *Service) Update(ctx context.Context, id string, in Input) (Detail, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	if r.Kind == KindSubscription {
		return Detail{}, invalid("订阅节点的配置随订阅更新，只能修改启用状态、标签与前置代理")
	}
	stored, err := s.mappingOf(r)
	if err != nil {
		return Detail{}, err
	}
	if in.Mapping != nil {
		in.Mapping = unmask(in.Mapping, stored)
	}
	if err := s.prepare(ctx, id, &in, &r); err != nil {
		return Detail{}, err
	}
	public, secrets := sub.SplitSecrets(in.Mapping)
	spec, _ := json.Marshal(public)
	secJSON, _ := json.Marshal(secrets)
	ct, err := s.secrets.Seal(secJSON, SecretAAD(id))
	if err != nil {
		return Detail{}, err
	}
	tags, _ := json.Marshal(in.Tags)
	enabled := r.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	_, err = s.db.ExecContext(ctx, `UPDATE proxy_nodes SET kind = ?, protocol = ?, display_name = ?, region = ?,
		server = ?, port = ?, enabled = ?, spec = ?, secret = ?, dialer_ref = ?, tags = ?, updated_at = ? WHERE id = ?`,
		in.Kind, in.Mapping["type"], in.DisplayName, in.Region, in.Mapping["server"], sub.PortOf(in.Mapping), enabled,
		string(spec), ct, in.DialerRef, string(tags), s.stamp(), id)
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, id, false)
}

// prepare 规范化并校验输入；existing 为 nil 表示新建。
func (s *Service) prepare(ctx context.Context, id string, in *Input, existing *row) error {
	if in.Kind != KindManual && in.Kind != KindISP {
		return invalid("节点类型须为 manual 或 isp")
	}
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if n := len([]rune(in.DisplayName)); n == 0 || n > 64 {
		return invalid("节点名称须为 1–64 字")
	}
	if reservedNames[strings.ToUpper(in.DisplayName)] {
		return invalid("%s 是保留名称", in.DisplayName)
	}
	var dup int
	// 节点名与代理组名共用 Mihomo 的命名空间
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM proxy_nodes WHERE display_name = ? AND id != ?) +
		(SELECT COUNT(*) FROM proxy_groups WHERE name = ?)`, in.DisplayName, id, in.DisplayName).Scan(&dup); err != nil {
		return err
	}
	if dup > 0 {
		return invalid("名称「%s」已被其它节点或代理组使用", in.DisplayName)
	}

	if in.Mapping == nil {
		return invalid("缺少节点配置")
	}
	m := make(map[string]any, len(in.Mapping))
	for k, v := range in.Mapping {
		m[k] = v
	}
	delete(m, "name")
	delete(m, "dialer-proxy") // 前置代理由 dialer_ref 管理
	if p := sub.PortOf(m); p > 0 {
		m["port"] = p
	}
	if t, _ := m["type"].(string); t == "" {
		return invalid("缺少协议类型")
	}
	if srv, _ := m["server"].(string); srv == "" || sub.PortOf(m) < 1 || sub.PortOf(m) > 65535 {
		return invalid("服务器与端口必填，端口须在 1–65535")
	}
	if s.Validate != nil {
		if err := s.Validate(withName(m, in.DisplayName)); err != nil {
			return invalid("配置校验失败：%v", err)
		}
	}
	in.Mapping = m

	in.Region = strings.TrimSpace(in.Region)
	if in.Region == "" {
		if in.Region = sub.MatchRegion(in.DisplayName); in.Region == "" {
			in.Region = sub.RegionOther
		}
	}
	in.Tags = normalizeTags(in.Tags, in.Kind == KindISP)
	enabled := in.Enabled == nil || *in.Enabled
	if in.Enabled == nil && existing != nil {
		enabled = existing.Enabled
	}
	return s.checkDialer(ctx, refs.NodeAttrs{ID: id, Name: in.DisplayName, Kind: in.Kind, Protocol: m["type"].(string),
		Region: in.Region, Tags: in.Tags, Enabled: enabled, DialerRef: in.DialerRef})
}

func normalizeTags(tags []string, isp bool) []string {
	seen := map[string]bool{}
	out := []string{}
	if isp {
		tags = append(tags, TagISP) // M3-09：ISP 节点自动带 isp 标签
	}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] || len([]rune(t)) > 32 {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// checkDialer 校验前置代理存在且不形成环（M3-06、M6-02）。有代理组服务时做整图校验，
// 即使没有设置前置：节点的标签/启停变化可能让它进入别的节点的前置组。
func (s *Service) checkDialer(ctx context.Context, n refs.NodeAttrs) error {
	selfID, ref := n.ID, n.DialerRef
	if ref != "" && ref == selfID {
		return invalid("不能把节点自身设为前置代理")
	}
	if s.Groups != nil {
		msg, err := s.Groups.CheckNode(ctx, n)
		if err != nil {
			return err
		}
		if msg != "" {
			return invalid("%s", msg)
		}
		return nil
	}
	if ref == "" {
		return nil
	}
	cur := ref
	path := []string{}
	for depth := 0; cur != ""; depth++ {
		if depth >= maxChainDepth {
			return invalid("前置代理链超过 %d 跳", maxChainDepth)
		}
		var name, next string
		err := s.db.QueryRowContext(ctx, `SELECT display_name, dialer_ref FROM proxy_nodes WHERE id = ?`, cur).Scan(&name, &next)
		if errors.Is(err, sql.ErrNoRows) {
			if cur == ref {
				return invalid("前置代理不存在")
			}
			return invalid("前置代理链中的节点 %s 不存在", cur)
		}
		if err != nil {
			return err
		}
		path = append(path, name)
		if next == selfID {
			return invalid("前置代理形成环路：%s → 本节点", strings.Join(path, " → "))
		}
		cur = next
	}
	return nil
}

// Patch 修改任意节点的启用状态、标签、前置代理。
func (s *Service) Patch(ctx context.Context, id string, a Attrs) (Node, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if a.Enabled != nil {
		r.Enabled = *a.Enabled
	}
	if a.Tags != nil {
		r.Tags = normalizeTags(*a.Tags, r.Kind == KindISP)
	}
	if a.DialerRef != nil {
		r.DialerRef = *a.DialerRef
	}
	if err := s.checkDialer(ctx, refs.NodeAttrs{ID: id, Name: r.DisplayName, Kind: r.Kind,
		SubscriptionID: r.SubscriptionID, Protocol: r.Protocol, Region: r.Region, Tags: r.Tags,
		Enabled: r.Enabled, DialerRef: r.DialerRef}); err != nil {
		return Node{}, err
	}
	tags, _ := json.Marshal(r.Tags)
	if _, err := s.db.ExecContext(ctx, `UPDATE proxy_nodes SET enabled = ?, tags = ?, dialer_ref = ?, updated_at = ?
		WHERE id = ?`, r.Enabled, string(tags), r.DialerRef, s.stamp(), id); err != nil {
		return Node{}, err
	}
	d, err := s.Get(ctx, id, false)
	return d.Node, err
}

// SetEnabled 批量启停（M3-01）。
func (s *Service) SetEnabled(ctx context.Context, ids []string, enabled bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	in := `(` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}
	// 启用可能让节点进入别的节点的前置组而形成环路：记下原先禁用的节点，校验失败时回退
	var turnedOn []any
	if enabled && s.Groups != nil {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM proxy_nodes WHERE enabled = 0 AND id IN `+in, idArgs...)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			turnedOn = append(turnedOn, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE proxy_nodes SET enabled = ?, updated_at = ? WHERE id IN `+in,
		append([]any{enabled, s.stamp()}, idArgs...)...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if len(turnedOn) > 0 {
		msg, err := s.Groups.CheckGraph(ctx)
		if err == nil && msg == "" {
			return int(n), nil
		}
		if _, rerr := s.db.ExecContext(ctx, `UPDATE proxy_nodes SET enabled = 0 WHERE id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(turnedOn)), ",")+`)`, turnedOn...); rerr != nil {
			return 0, rerr
		}
		if err != nil {
			return 0, err
		}
		return 0, invalid("%s", msg)
	}
	return int(n), nil
}

// Delete 删除手工/ISP 节点；仍被引用且未确认时返回 *refs.Error。
func (s *Service) Delete(ctx context.Context, id string, confirm bool) error {
	r, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if r.Kind == KindSubscription {
		return invalid("订阅节点会在下次更新时重新出现，请改为禁用，或在订阅的过滤规则中排除")
	}
	impacts, err := refs.Find(ctx, s.db, map[string]string{id: r.DisplayName})
	if err != nil {
		return err
	}
	if len(impacts) > 0 && !confirm {
		return &refs.Error{Impacts: impacts}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM proxy_nodes WHERE id = ?`, id)
	return err
}

// DeleteMany 批量删除节点，返回删除的数量和跳过的订阅节点数量（订阅节点下次更新会重新出现，只能禁用）。
// 被代理组、其他节点的前置或规则引用时，未确认（confirm=false）返回 *refs.Error，什么都不删；
// 选中的节点之间互相引用不计。
func (s *Service) DeleteMany(ctx context.Context, ids []string, confirm bool) (deleted, skipped int, err error) {
	removed := map[string]string{}
	for _, id := range ids {
		r, err := s.get(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue // 已经不在了（比如别处刚删过）
		}
		if err != nil {
			return 0, 0, err
		}
		if r.Kind == KindSubscription {
			skipped++
			continue
		}
		removed[id] = r.DisplayName
	}
	if len(removed) == 0 {
		return 0, skipped, nil
	}
	impacts, err := refs.Find(ctx, s.db, removed)
	if err != nil {
		return 0, 0, err
	}
	if len(impacts) > 0 && !confirm {
		return 0, skipped, &refs.Error{Impacts: impacts}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	for id := range removed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_nodes WHERE id = ?`, id); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(removed), skipped, nil
}

// Import 解析粘贴内容，返回候选节点供用户确认后逐条 Create。
func (s *Service) Import(text string) []Candidate {
	return ParseImport(text, s.Validate)
}

type TestRequest struct {
	Kind      string         `json:"kind"` // delay / exit
	ID        string         `json:"id"`   // 已保存节点；与 Mapping 同时给出时用于还原掩码凭据
	Mapping   map[string]any `json:"mapping"`
	DialerRef *string        `json:"dialer_ref"`
}

type TestResult struct {
	DelayMS int       `json:"delay_ms,omitempty"`
	Exit    *ExitInfo `json:"exit,omitempty"`
	Chain   []string  `json:"chain"` // 从本节点到最外层前置的显示名
}

// Test 在进程内测试节点（含前置代理链）的延迟或出口。
func (s *Service) Test(ctx context.Context, req TestRequest) (TestResult, error) {
	var head map[string]any
	dialer := ""
	selfName := "（未保存）"
	if req.ID != "" {
		r, err := s.get(ctx, req.ID)
		if err != nil {
			return TestResult{}, err
		}
		stored, err := s.mappingOf(r)
		if err != nil {
			return TestResult{}, err
		}
		head, dialer, selfName = stored, r.DialerRef, r.DisplayName
		if req.Mapping != nil {
			head = unmask(req.Mapping, stored)
		}
	} else if req.Mapping != nil {
		head = req.Mapping
	} else {
		return TestResult{}, invalid("需要 id 或 mapping")
	}
	if req.DialerRef != nil {
		dialer = *req.DialerRef
	}

	chain := []ChainNode{{Name: "self", Mapping: head, Dialer: dialer}}
	names := []string{selfName}
	for cur := dialer; cur != ""; {
		if len(chain) > maxChainDepth {
			return TestResult{}, invalid("前置代理链超过 %d 跳或存在环路", maxChainDepth)
		}
		label := ""
		if s.Groups != nil { // 前置为组时取其首个节点成员
			id, l, err := s.Groups.DialerTarget(ctx, cur)
			if err != nil {
				return TestResult{}, err
			}
			cur, label = id, l
		}
		r, err := s.get(ctx, cur)
		if errors.Is(err, ErrNotFound) {
			return TestResult{}, invalid("前置代理 %s 不存在", cur)
		}
		if err != nil {
			return TestResult{}, err
		}
		m, err := s.mappingOf(r)
		if err != nil {
			return TestResult{}, err
		}
		chain[len(chain)-1].Dialer = cur
		chain = append(chain, ChainNode{Name: cur, Mapping: m, Dialer: r.DialerRef})
		if label == "" {
			label = r.DisplayName
		}
		names = append(names, label)
		cur = r.DialerRef
	}

	res := TestResult{Chain: names}
	switch req.Kind {
	case "delay":
		ms, err := Delay(ctx, chain, s.DelayURL)
		if err != nil {
			return res, err
		}
		res.DelayMS = ms
	case "exit":
		info, err := Exit(ctx, chain, s.ExitURL)
		if err != nil {
			return res, err
		}
		res.Exit = &info
	default:
		return res, invalid("测试类型须为 delay 或 exit")
	}
	return res, nil
}

func (s *Service) stamp() string { return s.now().UTC().Format(timeLayout) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
