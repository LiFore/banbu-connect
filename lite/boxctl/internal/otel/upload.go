package otel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// 上报：客户端按 rowid 增量取出自己的数据（device_id = 0），发给管理服务；成功后记下进度。
// 提示词和助手回复的原文在入库时就已删掉，上报的数据里没有。

const (
	// 一批的大致大小上限（属性 JSON 的字节数）。
	batchBudget = 4 << 20
	// 单行属性超过这个大小时，span 去掉事件（工具输出），其他行照常。
	maxRowAttrs = 1 << 20
	// 管理服务一次最多接受的行数。
	maxImportRows = 20000
)

// Batch 是一次上报的数据。
type Batch struct {
	Logs    []LogRow    `json:"logs"`
	Metrics []MetricRow `json:"metrics"`
	Spans   []SpanRow   `json:"spans"`
}

// Len 是总行数。
func (b Batch) Len() int { return len(b.Logs) + len(b.Metrics) + len(b.Spans) }

// Cursor 是各表已取到的 rowid。
type Cursor map[string]int64

var uploadTables = []string{"otel_logs", "otel_metrics", "otel_spans"}

func (s *Store) cursor(ctx context.Context) (Cursor, error) {
	c := Cursor{}
	rows, err := s.db.QueryContext(ctx, `select tbl, last_rowid from otel_upload`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var id int64
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		c[t] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 表被清空后 rowid 从 1 重新开始，进度要跟着归零，否则新数据永远不会上报。
	for _, t := range uploadTables {
		var max sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `select max(rowid) from `+t).Scan(&max); err != nil {
			return nil, err
		}
		if max.Int64 < c[t] {
			c[t] = 0
		}
	}
	return c, nil
}

// Ack 记下已上报到的位置。
func (s *Store) Ack(ctx context.Context, c Cursor) error {
	for t, id := range c {
		if _, err := s.db.ExecContext(ctx, `insert into otel_upload (tbl, last_rowid) values (?, ?)
			on conflict(tbl) do update set last_rowid = excluded.last_rowid`, t, id); err != nil {
			return err
		}
	}
	return nil
}

// PendingCount 是还没上报的行数。
func (s *Store) PendingCount(ctx context.Context) (int64, error) {
	c, err := s.cursor(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, t := range uploadTables {
		var n int64
		if err := s.db.QueryRowContext(ctx, `select count(*) from `+t+` where device_id = 0 and rowid > ?`, c[t]).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// Pending 取出下一批要上报的数据和上报成功后的进度。没有数据时 Batch 为空。
func (s *Store) Pending(ctx context.Context) (Batch, Cursor, error) {
	c, err := s.cursor(ctx)
	if err != nil {
		return Batch{}, nil, err
	}
	b := Batch{Logs: []LogRow{}, Metrics: []MetricRow{}, Spans: []SpanRow{}}
	budget := batchBudget
	next := Cursor{}
	for k, v := range c {
		next[k] = v
	}
	// 每张表取一部分，预算用完就停。
	scan := func(table, cols string, each func(*sql.Rows) (int, error)) error {
		if budget <= 0 {
			return nil
		}
		rows, err := s.db.QueryContext(ctx, `select rowid, `+cols+` from `+table+` where device_id = 0 and rowid > ? order by rowid limit 2000`, c[table])
		if err != nil {
			return err
		}
		defer rows.Close()
		for budget > 0 && rows.Next() {
			n, err := each(rows)
			if err != nil {
				return err
			}
			budget -= n
		}
		return rows.Err()
	}
	var rowid int64
	var attrs string
	var session sql.NullString
	err = scan("otel_logs", "hash, ts_ms, name, session_id, attrs", func(r *sql.Rows) (int, error) {
		var row LogRow
		if err := r.Scan(&rowid, &row.Hash, &row.TsMs, &row.Name, &session, &attrs); err != nil {
			return 0, err
		}
		row.SessionID, row.Attrs = session.String, parseAttrs(attrs)
		b.Logs = append(b.Logs, row)
		next["otel_logs"] = rowid
		return len(attrs) + 128, nil
	})
	if err != nil {
		return b, nil, err
	}
	err = scan("otel_metrics", "hash, name, start_ms, end_ms, value, cumulative, session_id, attrs", func(r *sql.Rows) (int, error) {
		var row MetricRow
		var cum int
		if err := r.Scan(&rowid, &row.Hash, &row.Name, &row.StartMs, &row.EndMs, &row.Value, &cum, &session, &attrs); err != nil {
			return 0, err
		}
		row.Cumulative, row.SessionID, row.Attrs = cum == 1, session.String, parseAttrs(attrs)
		b.Metrics = append(b.Metrics, row)
		next["otel_metrics"] = rowid
		return len(attrs) + 128, nil
	})
	if err != nil {
		return b, nil, err
	}
	err = scan("otel_spans", "trace_id, span_id, coalesce(parent_span_id, ''), coalesce(name, ''), start_us, end_us, status, session_id, attrs", func(r *sql.Rows) (int, error) {
		var row SpanRow
		if err := r.Scan(&rowid, &row.TraceID, &row.SpanID, &row.ParentSpanID, &row.Name, &row.StartUs, &row.EndUs, &row.Status, &session, &attrs); err != nil {
			return 0, err
		}
		row.SessionID, row.Attrs = session.String, parseAttrs(attrs)
		if len(attrs) > maxRowAttrs {
			delete(row.Attrs, "span.events")
			row.Attrs["span.events_dropped"] = true
		}
		b.Spans = append(b.Spans, row)
		next["otel_spans"] = rowid
		return min(len(attrs), maxRowAttrs) + 128, nil
	})
	if err != nil {
		return b, nil, err
	}
	return b, next, nil
}

func parseAttrs(s string) Attrs {
	a := Attrs{}
	_ = json.Unmarshal([]byte(s), &a)
	return a
}

// ErrInvalidBatch 是上报的数据不合法。
var ErrInvalidBatch = errors.New("上报的遥测数据无效")

// Import 保存一台客户端上报的数据（管理服务用）。device 必须大于 0。
func (s *Store) Import(ctx context.Context, device int64, b Batch) (int, error) {
	if device <= 0 {
		return 0, fmt.Errorf("%w：客户端无效", ErrInvalidBatch)
	}
	if b.Len() > maxImportRows {
		return 0, fmt.Errorf("%w：一次最多 %d 行", ErrInvalidBatch, maxImportRows)
	}
	for _, r := range b.Logs {
		if r.Hash == "" || len(r.Hash) > 128 {
			return 0, fmt.Errorf("%w：事件缺少 hash", ErrInvalidBatch)
		}
		scrub(r.Attrs)
	}
	for _, r := range b.Metrics {
		if r.Hash == "" || len(r.Hash) > 128 {
			return 0, fmt.Errorf("%w：指标缺少 hash", ErrInvalidBatch)
		}
		scrub(r.Attrs)
	}
	for _, r := range b.Spans {
		if r.TraceID == "" || r.SpanID == "" {
			return 0, fmt.Errorf("%w：trace 缺少 id", ErrInvalidBatch)
		}
		scrub(r.Attrs)
		if evs, ok := r.Attrs["span.events"].([]any); ok {
			for _, e := range evs {
				if m, ok := e.(map[string]any); ok {
					scrub(Attrs(m))
				}
			}
		}
	}
	total := 0
	n, err := s.insertLogs(ctx, device, b.Logs)
	if err != nil {
		return total, err
	}
	total += n
	if n, err = s.insertMetrics(ctx, device, b.Metrics); err != nil {
		return total, err
	}
	total += n
	n, err = s.insertSpans(ctx, device, b.Spans)
	return total + n, err
}

// DeleteDevice 删除一台客户端上报的全部数据（管理服务删除客户端时用）。
func (s *Store) DeleteDevice(ctx context.Context, device int64) error {
	for _, t := range append(uploadTables, "otel_sessions") {
		if _, err := s.db.ExecContext(ctx, `delete from `+t+` where device_id = ?`, device); err != nil {
			return err
		}
	}
	return nil
}
