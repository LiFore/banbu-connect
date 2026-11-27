package dist

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 诊断数据：客户端把出口探测结果和系统事件随同步上报，管理员在「客户端」页面排查连接问题。
//
// 收集范围以「排查连接问题」为限，不收集使用情况：
//   - 收集：出口探测结果（IP、地区、耗时、错误）、Kill Switch 状态变化、内核和 TUN 故障、
//     网络类型变化（网卡、是不是 Wi-Fi、是否受信、能否读到名称），以及上报时的状态快照。
//   - 不收集：访问的域名和连接、流量、Wi-Fi 名称、开关机和休眠、服务或应用的启动退出、
//     开关代理、切换模式和线路这类操作。
//
// 客户端只在绑定了管理服务、且没有在「服务端」页面关闭诊断上报时收集和上报。

const (
	maxDiagRecords    = 500 // 一次上报的事件上限
	maxDiagFields     = 16  // 每条事件的字段数上限
	maxDiagText       = 500 // 消息、字段值的长度上限（字符）
	maxDiagKey        = 32  // 类型、级别、字段名的长度上限
	maxDiagSnapshot   = 64 << 10
	diagKeepDays      = 30   // 服务端保留天数
	diagKeepPerClient = 5000 // 每台客户端最多保留的事件数
)

// DiagRecord 是一条诊断事件。
type DiagRecord struct {
	Seq     int64             `json:"seq"` // 客户端生成，单调递增（纳秒时间戳），服务端据此去重
	Time    time.Time         `json:"time"`
	Kind    string            `json:"kind"`  // egress、strict_egress、network、killswitch、health、core
	Level   string            `json:"level"` // info、warn、error
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// DiagEgress 是一次出口探测的结果。
type DiagEgress struct {
	OK        bool      `json:"ok"`
	IP        string    `json:"ip,omitempty"`
	Country   string    `json:"country,omitempty"`
	Error     string    `json:"error,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// DiagSnapshot 是上报时的状态。原因文字里的 Wi-Fi 名称已去掉。
type DiagSnapshot struct {
	Time        time.Time `json:"time"`
	KillSwitch  string    `json:"killswitch"` // killswitch 的状态
	Path        string    `json:"path"`
	Reason      string    `json:"reason,omitempty"`
	DNSOK       bool      `json:"dns_ok"`
	TUNOK       bool      `json:"tun_ok"`
	CoreRunning bool      `json:"core_running"`
	CoreError   string    `json:"core_error,omitempty"`
	TUNError    string    `json:"tun_error,omitempty"`
	Network     DiagNet   `json:"network"`
	// Egress 是受信 Wi-Fi 上最近一次出口探测；不在受信 Wi-Fi 上时为空。
	Egress *DiagEgress  `json:"egress,omitempty"`
	Strict []DiagStrict `json:"strict,omitempty"`
}

// DiagNet 描述当前网络，不含 Wi-Fi 名称。
type DiagNet struct {
	Interface    string `json:"interface"`
	WiFi         bool   `json:"wifi"`
	Trusted      bool   `json:"trusted"`
	SSIDReadable bool   `json:"ssid_readable"`
	Hint         string `json:"hint,omitempty"`
}

// DiagStrict 是一个严格组的出口。
type DiagStrict struct {
	Group   string     `json:"group"`
	Allowed bool       `json:"allowed"`
	Egress  DiagEgress `json:"egress"`
}

// DiagReport 是一次上报。
type DiagReport struct {
	Snapshot DiagSnapshot `json:"snapshot"`
	Records  []DiagRecord `json:"records"`
}

// ---- 客户端 ----

// Report 上报诊断数据。没有绑定时什么也不做，返回 false。
func (c *Client) Report(ctx context.Context, r DiagReport) (bool, error) {
	c.mu.Lock()
	b := c.binding
	c.mu.Unlock()
	if b == nil {
		return false, nil
	}
	body, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	resp, err := c.doMethod(ctx, *b, http.MethodPost, "/api/v1/client/diagnostics", bytes.NewReader(body), func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
	})
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, responseError(resp)
	}
	return true, nil
}

// ---- 服务端 ----

func validKey(s string) bool {
	if s == "" || len(s) > maxDiagKey {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxDiagText {
		return s
	}
	return string([]rune(s)[:maxDiagText]) + "…"
}

func (r DiagReport) validate() error {
	if len(r.Records) > maxDiagRecords {
		return &InputError{fmt.Sprintf("一次最多上报 %d 条事件", maxDiagRecords)}
	}
	for i, rec := range r.Records {
		if rec.Seq <= 0 || rec.Time.IsZero() || !validKey(rec.Kind) || !validKey(rec.Level) {
			return &InputError{fmt.Sprintf("第 %d 条事件无效", i+1)}
		}
		if len(rec.Fields) > maxDiagFields {
			return &InputError{fmt.Sprintf("第 %d 条事件的字段过多", i+1)}
		}
		for k := range rec.Fields {
			if !validKey(k) {
				return &InputError{fmt.Sprintf("第 %d 条事件的字段名 %q 无效", i+1, k)}
			}
		}
	}
	return nil
}

// SaveDiagnostics 保存一台客户端的上报：快照覆盖，事件按 seq 去重追加，并清理过期的。
func (s *Server) SaveDiagnostics(ctx context.Context, deviceID int64, r DiagReport) error {
	if err := r.validate(); err != nil {
		return err
	}
	snap, err := json.Marshal(r.Snapshot)
	if err != nil {
		return err
	}
	if len(snap) > maxDiagSnapshot {
		return &InputError{"状态快照过大"}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE devices SET diag_snapshot = ?, diag_at = ? WHERE id = ?`,
		string(snap), now.Format(time.RFC3339), deviceID); err != nil {
		return err
	}
	for _, rec := range r.Records {
		fields := map[string]string{}
		for k, v := range rec.Fields {
			fields[k] = clip(v)
		}
		f, _ := json.Marshal(fields)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO device_diagnostics (device_id, seq, time, kind, level, message, fields)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, deviceID, rec.Seq, rec.Time.UTC().Format(time.RFC3339Nano),
			rec.Kind, rec.Level, clip(rec.Message), string(f)); err != nil {
			return err
		}
	}
	cutoff := now.AddDate(0, 0, -diagKeepDays).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_diagnostics WHERE device_id = ? AND (time < ? OR id NOT IN
		(SELECT id FROM device_diagnostics WHERE device_id = ? ORDER BY time DESC, seq DESC LIMIT ?))`,
		deviceID, cutoff, deviceID, diagKeepPerClient); err != nil {
		return err
	}
	return tx.Commit()
}

// StoredDiagRecord 是服务端保存的一条事件。
type StoredDiagRecord struct {
	DiagRecord
	ID         int64  `json:"id"`
	ReceivedAt string `json:"received_at"`
}

// DeviceDiagnostics 是管理页面看到的诊断数据。
type DeviceDiagnostics struct {
	Snapshot   *DiagSnapshot      `json:"snapshot"`
	SnapshotAt string             `json:"snapshot_at"`
	Records    []StoredDiagRecord `json:"records"`
}

// Diagnostics 返回一台客户端的快照和事件，按时间倒序。kind 为空表示全部；before 为 0 表示从最新开始。
func (s *Server) Diagnostics(ctx context.Context, deviceID int64, kind string, before int64, limit int) (DeviceDiagnostics, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var out DeviceDiagnostics
	var snap string
	err := s.DB.QueryRowContext(ctx, `SELECT diag_snapshot, diag_at FROM devices WHERE id = ?`, deviceID).Scan(&snap, &out.SnapshotAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if snap != "" {
		var sn DiagSnapshot
		if json.Unmarshal([]byte(snap), &sn) == nil {
			out.Snapshot = &sn
		}
	}
	q := `SELECT id, seq, time, kind, level, message, fields, received_at FROM device_diagnostics WHERE device_id = ?`
	args := []any{deviceID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	if before > 0 {
		q += ` AND id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY time DESC, seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Records = []StoredDiagRecord{}
	for rows.Next() {
		var r StoredDiagRecord
		var t, fields string
		if err := rows.Scan(&r.ID, &r.Seq, &t, &r.Kind, &r.Level, &r.Message, &fields, &r.ReceivedAt); err != nil {
			return out, err
		}
		r.Time, _ = time.Parse(time.RFC3339Nano, t)
		_ = json.Unmarshal([]byte(fields), &r.Fields)
		out.Records = append(out.Records, r)
	}
	return out, rows.Err()
}

func (s *Server) diagnosticsAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	mux.Handle("GET /api/v1/dist/devices/{id}/diagnostics", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, ErrNotFound, log)
			return
		}
		q := r.URL.Query()
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		d, err := s.Diagnostics(r.Context(), id, strings.TrimSpace(q.Get("kind")), before, limit)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
}

func (s *Server) diagnosticsClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	mux.Handle("POST /api/v1/client/diagnostics", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in DiagReport
		if !decode(w, r, &in) {
			return
		}
		if err := s.SaveDiagnostics(r.Context(), d.ID, in); err != nil {
			writeErr(w, err, log)
			return
		}
		_ = s.Touch(r.Context(), d.ID, remoteIP(r), clientInfo(r), 0)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}
