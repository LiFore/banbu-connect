// Package refs 查找对节点或代理组的引用（代理组显式成员与空组兜底、节点的前置代理、规则目标），
// 供删除节点前的影响分析使用（M2-10、M3、M4-08）。
package refs

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Impact 描述一个被删除节点的引用方。
type Impact struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	Kind     string `json:"kind"` // group / node / rule
	RefID    string `json:"ref_id"`
	RefName  string `json:"ref_name"`
}

// Error 表示操作会删除仍被引用的节点，需要用户确认。
type Error struct{ Impacts []Impact }

func (e *Error) Error() string {
	return fmt.Sprintf("%d 处引用会受影响，需要确认", len(e.Impacts))
}

// NodeAttrs 是节点在“组—节点—前置代理”图中的属性。节点保存前用它描述保存后的状态，
// 交给代理组服务做整图校验（新建节点、改标签/启停都可能改变组成员进而形成前置环路）。
type NodeAttrs struct {
	ID, Name, Kind, SubscriptionID, Protocol, Region, DialerRef string
	Tags                                                        []string
	Enabled                                                     bool
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Find 返回 removed（id→显示名）中节点或组的全部引用方；removed 内部的节点互相作为前置不计。
func Find(ctx context.Context, db querier, removed map[string]string) ([]Impact, error) {
	impacts := []Impact{}
	if len(removed) == 0 {
		return impacts, nil
	}
	ids := make([]any, 0, len(removed))
	for id := range removed {
		ids = append(ids, id)
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"

	queries := []struct {
		kind, q string
		args    []any
	}{
		{"group", `SELECT e.value, g.id, g.name FROM proxy_groups g, json_each(g.members, '$.explicit') e
			WHERE e.value IN ` + in, ids},
		{"group", `SELECT json_extract(options, '$.empty_fallback'), id, name || '（空组兜底）' FROM proxy_groups
			WHERE json_extract(options, '$.empty_fallback') IN ` + in, ids},
		{"node", `SELECT dialer_ref, id, display_name FROM proxy_nodes WHERE dialer_ref IN ` + in +
			` AND id NOT IN ` + in, append(append([]any{}, ids...), ids...)},
		{"rule", `SELECT target, id, note FROM rules WHERE target IN ` + in, ids},
	}
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q.q, q.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			im := Impact{Kind: q.kind}
			if err := rows.Scan(&im.NodeID, &im.RefID, &im.RefName); err != nil {
				rows.Close()
				return nil, err
			}
			im.NodeName = removed[im.NodeID]
			impacts = append(impacts, im)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return impacts, nil
}
