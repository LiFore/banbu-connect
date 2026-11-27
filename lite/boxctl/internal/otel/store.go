package otel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动
)

// 遥测量大、写得频繁，单独一个库，不和主库抢写锁。客户端和管理服务共用这套表：
// 客户端自己的数据 device_id 为 0；管理服务按上报的客户端（devices.id）分开存。
const schema = `
create table if not exists otel_spans (
  device_id integer not null default 0, trace_id text not null, span_id text not null, parent_span_id text, name text,
  start_us integer, end_us integer, status integer, session_id text, attrs text, received_ms integer,
  primary key (device_id, trace_id, span_id)
);
create table if not exists otel_logs (
  device_id integer not null default 0, hash text not null, ts_ms integer, name text, session_id text, attrs text, received_ms integer,
  primary key (device_id, hash)
);
create table if not exists otel_metrics (
  device_id integer not null default 0, hash text not null, name text, start_ms integer, end_ms integer, value real,
  cumulative integer, session_id text, attrs text, received_ms integer,
  primary key (device_id, hash)
);
create table if not exists otel_sessions (
  device_id integer not null default 0, session_id text not null, cwd text, first_ms integer, last_ms integer,
  primary key (device_id, session_id)
);
-- 客户端上报进度：各表已上报到的 rowid。
create table if not exists otel_upload (tbl text primary key, last_rowid integer not null);
create index if not exists idx_logs_ts on otel_logs (device_id, ts_ms);
create index if not exists idx_logs_name on otel_logs (device_id, name, ts_ms);
create index if not exists idx_logs_session on otel_logs (device_id, session_id);
create index if not exists idx_metrics_end on otel_metrics (device_id, name, end_ms);
create index if not exists idx_spans_start on otel_spans (start_us);
create index if not exists idx_sessions_last on otel_sessions (device_id, last_ms);
`

// Signal 是 OTLP 的三类数据。
type Signal string

const (
	Traces  Signal = "traces"
	Logs    Signal = "logs"
	Metrics Signal = "metrics"
)

// Store 是遥测库。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// OpenStore 打开（必要时创建）遥测库；path 为 ":memory:" 时用内存库（测试用）。
func OpenStore(path string) (*Store, error) {
	dsn := "file::memory:?_pragma=busy_timeout(5000)"
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Ingest 解析并写入一个 OTLP 请求体，返回收到和新写入（去重后）的条数。
func (s *Store) Ingest(ctx context.Context, signal Signal, body []byte) (received, inserted int, err error) {
	switch signal {
	case Traces:
		rows, err := ParseTraces(body)
		if err != nil {
			return 0, 0, badRequest(err)
		}
		n, err := s.insertSpans(ctx, 0, rows)
		return len(rows), n, err
	case Logs:
		rows, err := ParseLogs(body)
		if err != nil {
			return 0, 0, badRequest(err)
		}
		n, err := s.insertLogs(ctx, 0, rows)
		return len(rows), n, err
	case Metrics:
		rows, err := ParseMetrics(body)
		if err != nil {
			return 0, 0, badRequest(err)
		}
		n, err := s.insertMetrics(ctx, 0, rows)
		return len(rows), n, err
	}
	return 0, 0, fmt.Errorf("未知信号 %q", signal)
}

// Prune 删除 days 天之前的数据；days <= 0 不清理。
func (s *Store) Prune(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := s.now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	var total int64
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{"delete from otel_logs where ts_ms < ?", cutoff},
		{"delete from otel_metrics where end_ms < ?", cutoff},
		{"delete from otel_spans where start_us < ?", cutoff * 1000},
		{"delete from otel_sessions where last_ms < ?", cutoff},
	} {
		res, err := s.db.ExecContext(ctx, q.sql, q.arg)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	// 表清空后 rowid 会从头开始，上报进度跟着归零。
	for _, t := range uploadTables {
		if _, err := s.db.ExecContext(ctx, `delete from otel_upload where tbl = ? and not exists (select 1 from `+t+`)`, t); err != nil {
			return total, err
		}
	}
	return total, nil
}

// Counts 是库里的数据量。
type Counts struct {
	Spans        int64  `json:"spans"`
	Logs         int64  `json:"logs"`
	Metrics      int64  `json:"metrics"`
	Sessions     int64  `json:"sessions"`
	LastReceived *int64 `json:"last_received_ms,omitempty"`
}

// Counts 统计各表条数和最近一次收到数据的时间。
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	var last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `select
		(select count(*) from otel_spans), (select count(*) from otel_logs), (select count(*) from otel_metrics),
		(select count(*) from otel_sessions),
		max(coalesce((select max(received_ms) from otel_spans), 0), coalesce((select max(received_ms) from otel_logs), 0),
		    coalesce((select max(received_ms) from otel_metrics), 0))`).
		Scan(&c.Spans, &c.Logs, &c.Metrics, &c.Sessions, &last)
	if last.Valid && last.Int64 > 0 {
		c.LastReceived = &last.Int64
	}
	return c, err
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) (int, error)) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	n, err := fn(tx)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	return n, tx.Commit()
}

func affected(res sql.Result, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func attrsJSON(a Attrs) string {
	b, _ := json.Marshal(a)
	return string(b)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) insertSpans(ctx context.Context, device int64, rows []SpanRow) (int, error) {
	recv := s.now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) (int, error) {
		total := 0
		for _, r := range rows {
			n, err := affected(tx.ExecContext(ctx, `insert or ignore into otel_spans
				(device_id, trace_id, span_id, parent_span_id, name, start_us, end_us, status, session_id, attrs, received_ms) values (?,?,?,?,?,?,?,?,?,?,?)`,
				device, r.TraceID, r.SpanID, r.ParentSpanID, r.Name, r.StartUs, r.EndUs, r.Status, nullable(r.SessionID), attrsJSON(r.Attrs), recv))
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	})
}

func (s *Store) insertLogs(ctx context.Context, device int64, rows []LogRow) (int, error) {
	recv := s.now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) (int, error) {
		total := 0
		for _, r := range rows {
			n, err := affected(tx.ExecContext(ctx, `insert or ignore into otel_logs (device_id, hash, ts_ms, name, session_id, attrs, received_ms) values (?,?,?,?,?,?,?)`,
				device, r.Hash, r.TsMs, r.Name, nullable(r.SessionID), attrsJSON(r.Attrs), recv))
			if err != nil {
				return 0, err
			}
			total += n
			if r.SessionID == "" {
				continue
			}
			// 事件带 workspace.host_paths（会话工作目录）。
			var cwd any
			if paths, ok := r.Attrs["workspace.host_paths"].([]any); ok && len(paths) > 0 {
				if p, ok := paths[0].(string); ok {
					cwd = p
				}
			}
			if _, err := tx.ExecContext(ctx, `insert into otel_sessions (device_id, session_id, cwd, first_ms, last_ms) values (?,?,?,?,?)
				on conflict(device_id, session_id) do update set first_ms = min(first_ms, excluded.first_ms), last_ms = max(last_ms, excluded.last_ms),
				cwd = coalesce(cwd, excluded.cwd)`, device, r.SessionID, cwd, r.TsMs, r.TsMs); err != nil {
				return 0, err
			}
		}
		return total, nil
	})
}

func (s *Store) insertMetrics(ctx context.Context, device int64, rows []MetricRow) (int, error) {
	recv := s.now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) (int, error) {
		total := 0
		for _, r := range rows {
			cum := 0
			if r.Cumulative {
				cum = 1
			}
			n, err := affected(tx.ExecContext(ctx, `insert or ignore into otel_metrics
				(device_id, hash, name, start_ms, end_ms, value, cumulative, session_id, attrs, received_ms) values (?,?,?,?,?,?,?,?,?,?)`,
				device, r.Hash, r.Name, r.StartMs, r.EndMs, r.Value, cum, nullable(r.SessionID), attrsJSON(r.Attrs), recv))
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	})
}

// RequestError 是请求体本身的问题（返回 400）。
type RequestError struct{ Err error }

func (e *RequestError) Error() string { return "请求体无效：" + e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

func badRequest(err error) error { return &RequestError{err} }
