package subscriptions

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	nodesvc "boxctl/internal/nodes"
	"boxctl/internal/refs"
	"boxctl/internal/sub"
)

// NodeRef 是差异列表中的节点摘要（不含任何凭据）。
type NodeRef struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Protocol    string `json:"protocol"`
	Region      string `json:"region"`
}

type ChangedNode struct {
	NodeRef
	Fields []string `json:"fields"` // 变化的字段名；凭据字段只报名称不报值
}

type Diff struct {
	Added     []NodeRef     `json:"added"`
	Removed   []NodeRef     `json:"removed"`
	Changed   []ChangedNode `json:"changed"`
	Unchanged int           `json:"unchanged"`
}

// Impact 描述一个被删除节点的引用方（M2-10）。
type Impact = refs.Impact

// ConflictNode 是与其它订阅中节点 ID 相同而被跳过的节点（跨订阅去重，先到先得）。
type ConflictNode struct {
	NodeRef
	OwnerSubscription string `json:"owner_subscription"`
}

type Preview struct {
	ID        string         `json:"preview_id"`
	ExpiresAt string         `json:"expires_at"`
	Format    sub.Format     `json:"format"`
	Report    sub.Report     `json:"report"`
	UserInfo  *sub.UserInfo  `json:"userinfo"`
	Diff      Diff           `json:"diff"`
	Conflicts []ConflictNode `json:"conflicts"`
	Impacts   []Impact       `json:"impacts"`
}

type preview struct {
	subID    string
	expires  time.Time
	nodes    []sub.Node
	userInfo *sub.UserInfo
}

// Preview 拉取并清洗订阅，与当前已入库节点比对，不修改节点数据（M2-10）。
// 拉取失败会记录到 last_error。
func (s *Service) Preview(ctx context.Context, id string) (Preview, error) {
	sb, err := s.Get(ctx, id, true)
	if err != nil {
		return Preview{}, err
	}
	req := sub.FetchRequest{
		URL: sb.URL, UserAgent: sb.UserAgent,
		Timeout: time.Duration(sb.Options.TimeoutSec) * time.Second, Retries: sb.Options.Retries,
	}
	if sb.FetchVia == FetchProxy {
		req.ProxyURL = s.FetchProxy
	}
	var p Preview
	res, err := s.Fetch(ctx, req)
	if err == nil {
		p, err = s.preparePreview(ctx, sb, res)
	}
	if err != nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE subscriptions SET last_error = ? WHERE id = ?`, err.Error(), id)
		return Preview{}, err
	}
	return p, nil
}

func (s *Service) preparePreview(ctx context.Context, sb Subscription, res sub.FetchResult) (Preview, error) {
	raw, format, err := sub.Parse(res.Body)
	if err != nil {
		return Preview{}, err
	}
	nodes, rep := sub.Process(raw, sb.Name, sb.Options, s.Validate)

	nodes, conflicts, err := s.dropForeignDuplicates(ctx, sb.ID, nodes)
	if err != nil {
		return Preview{}, err
	}
	diff, removed, err := s.diff(ctx, sb.ID, nodes)
	if err != nil {
		return Preview{}, err
	}
	impacts, err := refs.Find(ctx, s.db, removed)
	if err != nil {
		return Preview{}, err
	}

	pv := &preview{subID: sb.ID, expires: s.now().Add(previewTTL), nodes: nodes, userInfo: res.UserInfo}
	pid := "p_" + randomHex(8)
	s.mu.Lock()
	for k, old := range s.previews { // 同一订阅只保留最新预览，并顺带清理过期项
		if old.subID == sb.ID || s.now().After(old.expires) {
			delete(s.previews, k)
		}
	}
	s.previews[pid] = pv
	s.mu.Unlock()

	return Preview{
		ID: pid, ExpiresAt: pv.expires.UTC().Format(timeLayout), Format: format,
		Report: rep, UserInfo: res.UserInfo, Diff: diff, Conflicts: conflicts, Impacts: impacts,
	}, nil
}

// Apply 把预览结果写入节点表。基于当前库内状态重新计算差异与影响，
// 存在引用影响且未确认时返回 *ReferencesError。保留节点的启用状态、标签与前置代理。
func (s *Service) Apply(ctx context.Context, id, previewID string, confirm bool) (Diff, error) {
	s.mu.Lock()
	pv, ok := s.previews[previewID]
	if ok && (pv.subID != id || s.now().After(pv.expires)) {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		return Diff{}, ErrPreviewExpired
	}

	nodes, _, err := s.dropForeignDuplicates(ctx, id, pv.nodes)
	if err != nil {
		return Diff{}, err
	}
	diff, removed, err := s.diff(ctx, id, nodes)
	if err != nil {
		return Diff{}, err
	}
	impacts, err := refs.Find(ctx, s.db, removed)
	if err != nil {
		return Diff{}, err
	}
	if len(impacts) > 0 && !confirm {
		return Diff{}, &ReferencesError{Impacts: impacts}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Diff{}, err
	}
	defer tx.Rollback()

	for nid := range removed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_nodes WHERE id = ?`, nid); err != nil {
			return Diff{}, err
		}
	}
	now := s.stamp()
	for _, n := range nodes {
		public, secrets := sub.SplitSecrets(n.Mapping)
		spec, _ := json.Marshal(public)
		secJSON, _ := json.Marshal(secrets)
		ct, err := s.secrets.Seal(secJSON, nodesvc.SecretAAD(n.ID))
		if err != nil {
			return Diff{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_nodes
			(id, subscription_id, kind, protocol, display_name, region, server, port, spec, secret, created_at, updated_at)
			VALUES (?, ?, 'subscription', ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET protocol = excluded.protocol, display_name = excluded.display_name,
				region = excluded.region, server = excluded.server, port = excluded.port,
				spec = excluded.spec, secret = excluded.secret, updated_at = excluded.updated_at`,
			n.ID, id, n.Protocol, n.DisplayName, n.Region, n.Server, n.Port, string(spec), ct, now, now); err != nil {
			return Diff{}, err
		}
	}
	ui := []byte("{}")
	if pv.userInfo != nil {
		ui, _ = json.Marshal(pv.userInfo)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET userinfo = ?, last_fetch_at = ?, last_error = '',
		updated_at = ? WHERE id = ?`, string(ui), now, now, id); err != nil {
		return Diff{}, err
	}
	if err := tx.Commit(); err != nil {
		return Diff{}, err
	}

	s.mu.Lock()
	delete(s.previews, previewID)
	s.mu.Unlock()
	return diff, nil
}

// dropForeignDuplicates 剔除 ID 已属于其它订阅（或手工节点）的节点。
func (s *Service) dropForeignDuplicates(ctx context.Context, subID string, nodes []sub.Node) ([]sub.Node, []ConflictNode, error) {
	owners := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, COALESCE(s.name, '手工节点') FROM proxy_nodes n
		LEFT JOIN subscriptions s ON s.id = n.subscription_id
		WHERE n.subscription_id IS NULL OR n.subscription_id != ?`, subID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, nil, err
		}
		owners[id] = owner
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	kept := make([]sub.Node, 0, len(nodes))
	conflicts := []ConflictNode{}
	for _, n := range nodes {
		if owner, ok := owners[n.ID]; ok {
			conflicts = append(conflicts, ConflictNode{NodeRef: ref(n), OwnerSubscription: owner})
			continue
		}
		kept = append(kept, n)
	}
	return kept, conflicts, nil
}

// diff 比较新节点与库内该订阅节点；返回差异与将被删除的节点（id→显示名）。
func (s *Service) diff(ctx context.Context, subID string, nodes []sub.Node) (Diff, map[string]string, error) {
	type existing struct {
		NodeRef
		mapping map[string]any
	}
	cur := map[string]existing{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name, protocol, region, spec, secret
		FROM proxy_nodes WHERE subscription_id = ?`, subID)
	if err != nil {
		return Diff{}, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e existing
		var spec string
		var ct []byte
		if err := rows.Scan(&e.ID, &e.DisplayName, &e.Protocol, &e.Region, &spec, &ct); err != nil {
			return Diff{}, nil, err
		}
		public := map[string]any{}
		if err := json.Unmarshal([]byte(spec), &public); err != nil {
			return Diff{}, nil, err
		}
		secrets := map[string]any{}
		if len(ct) > 0 {
			plain, err := s.secrets.Open(ct, nodesvc.SecretAAD(e.ID))
			if err != nil {
				return Diff{}, nil, err
			}
			if err := json.Unmarshal(plain, &secrets); err != nil {
				return Diff{}, nil, err
			}
		}
		e.mapping = sub.MergeSecrets(public, secrets)
		cur[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		return Diff{}, nil, err
	}

	d := Diff{Added: []NodeRef{}, Removed: []NodeRef{}, Changed: []ChangedNode{}}
	for _, n := range nodes {
		e, ok := cur[n.ID]
		if !ok {
			d.Added = append(d.Added, ref(n))
			continue
		}
		delete(cur, n.ID)
		fields := changedFields(e.mapping, n.Mapping)
		if e.DisplayName != n.DisplayName {
			fields = append(fields, "display_name")
		}
		if e.Region != n.Region {
			fields = append(fields, "region")
		}
		if len(fields) == 0 {
			d.Unchanged++
		} else {
			d.Changed = append(d.Changed, ChangedNode{NodeRef: ref(n), Fields: fields})
		}
	}
	removed := map[string]string{}
	for id, e := range cur {
		d.Removed = append(d.Removed, e.NodeRef)
		removed[id] = e.DisplayName
	}
	sort.Slice(d.Removed, func(i, j int) bool { return d.Removed[i].DisplayName < d.Removed[j].DisplayName })
	return d, removed, nil
}

// changedFields 比较两份配置的顶层字段（JSON 规范化后比较，消除 int/float 差异）。
// 节点原始名称 name 不算变化：显示名变化单独报告。
func changedFields(a, b map[string]any) []string {
	norm := func(m map[string]any) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if k == "name" {
				continue
			}
			j, _ := json.Marshal(v)
			out[k] = string(j)
		}
		return out
	}
	na, nb := norm(a), norm(b)
	var fields []string
	for k, v := range na {
		if nb[k] != v {
			fields = append(fields, k)
		}
	}
	for k := range nb {
		if _, ok := na[k]; !ok {
			fields = append(fields, k)
		}
	}
	sort.Strings(fields)
	return fields
}

func ref(n sub.Node) NodeRef {
	return NodeRef{ID: n.ID, DisplayName: n.DisplayName, Protocol: n.Protocol, Region: n.Region}
}
