package dist

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- 管理服务：接收与保存代理连接统计（BXC-F0-026、BXC-F0-027、BXC-F0-053）----

const (
	maxConnBatches    = 60
	maxConnMinutes    = 400
	maxConnRecords    = 2500
	maxConnText       = 255
	connRecordKeep    = 7 * 24 * time.Hour
	connMinuteKeep    = 30 * 24 * time.Hour
	connPruneEvery    = time.Hour
	connLiveRequested = time.Minute // 管理页面打开实时连接后，客户端保持推送的时间
)

func clipConn(s string) string {
	if len(s) <= maxConnText {
		return s
	}
	s = s[:maxConnText]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func pathJSON(p []string) (path, group, exit string) {
	if p == nil {
		p = []string{}
	}
	for i := range p {
		p[i] = clipConn(p[i])
	}
	b, _ := json.Marshal(p)
	if len(p) > 0 {
		group, exit = p[0], p[len(p)-1]
	}
	return string(b), group, exit
}

type sourceInfo struct{ mac, name string }

// RecordConns 保存一次上报，按（客户端, 序号）去重，返回已保存的最大序号。
func (s *Server) RecordConns(ctx context.Context, d Device, rep ConnReport) (ConnReportResult, error) {
	if len(rep.Batches) > maxConnBatches {
		return ConnReportResult{}, &InputError{"一次上报的批数太多"}
	}
	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ConnReportResult{}, err
	}
	defer tx.Rollback()
	var acked int64
	type hourKey struct{ hour, source string }
	type hourAgg struct {
		name            string
		up, down, conns int64
	}
	hours := map[hourKey]*hourAgg{}
	type trafficKey struct{ hour, name string }
	traffic := map[trafficKey][2]int64{}
	var lan []LANAddr
	for _, b := range rep.Batches {
		if len(b.Minutes) > maxConnMinutes || len(b.Records) > maxConnRecords {
			return ConnReportResult{}, &InputError{"一批的数据太多"}
		}
		acked = max(acked, b.Seq)
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conn_batches (device_id, seq, received_at) VALUES (?, ?, ?)`,
			d.ID, b.Seq, now.Format(time.RFC3339))
		if err != nil {
			return ConnReportResult{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // 已经收到过
		}
		if b.LAN != nil {
			lan = b.LAN
		}
		src := map[string]sourceInfo{}
		for _, x := range b.Sources {
			src[x.IP] = sourceInfo{mac: strings.ToLower(clipConn(x.MAC)), name: clipConn(x.Name)}
		}
		for _, m := range b.Minutes {
			up, down := max(m.Upload, 0), max(m.Download, 0)
			path, grp, exit := pathJSON(m.Path)
			si := src[m.Source]
			minute := m.Minute.UTC().Truncate(time.Minute)
			if _, err := tx.ExecContext(ctx, `INSERT INTO conn_minutes (device_id, minute, path, exit_node, grp, host, rule, payload, process,
				source_ip, source_mac, source_name, upload, download, conns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				d.ID, minute.Format(time.RFC3339), path, exit, grp, clipConn(m.Host), clipConn(m.Rule), clipConn(m.Payload), clipConn(m.Process),
				clipConn(m.Source), si.mac, si.name, up, down, max(m.Conns, 0)); err != nil {
				return ConnReportResult{}, err
			}
			key := si.mac
			if key == "" {
				key = clipConn(m.Source)
			}
			hk := hourKey{minute.Truncate(time.Hour).Format(time.RFC3339), key}
			h := hours[hk]
			if h == nil {
				h = &hourAgg{name: si.name}
				hours[hk] = h
			}
			h.up += up
			h.down += down
			h.conns += max(m.Conns, 0)
			// 按代理组 / 节点的小时流量（用量提醒）：路径上每一跳各算一次。
			seen := map[string]bool{}
			for _, name := range m.Path {
				if name = clipConn(name); name != "" && !seen[name] {
					seen[name] = true
					tk := trafficKey{hk.hour, name}
					traffic[tk] = [2]int64{traffic[tk][0] + up, traffic[tk][1] + down}
				}
			}
		}
		for _, u := range b.Unattributed {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conn_unattributed (device_id, minute, upload, download) VALUES (?, ?, ?, ?)
				ON CONFLICT (device_id, minute) DO UPDATE SET upload = upload + excluded.upload, download = download + excluded.download`,
				d.ID, u.Minute.UTC().Truncate(time.Minute).Format(time.RFC3339), max(u.Upload, 0), max(u.Download, 0)); err != nil {
				return ConnReportResult{}, err
			}
		}
		for _, r := range b.Records {
			path, grp, exit := pathJSON(r.Path)
			si := src[r.Source]
			if _, err := tx.ExecContext(ctx, `INSERT INTO conn_records (device_id, start_at, end_at, network, host, destination, rule, payload,
				path, exit_node, grp, process, source_ip, source_mac, source_name, upload, download)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				d.ID, r.Start.UTC().Format(time.RFC3339), r.End.UTC().Format(time.RFC3339), clipConn(r.Network), clipConn(r.Host), clipConn(r.Destination),
				clipConn(r.Rule), clipConn(r.Payload), path, exit, grp, clipConn(r.Process), clipConn(r.Source), si.mac, si.name,
				max(r.Upload, 0), max(r.Download, 0)); err != nil {
				return ConnReportResult{}, err
			}
		}
	}
	for k, h := range hours {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conn_hours (device_id, hour, source_key, source_name, upload, download, conns)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (device_id, hour, source_key) DO UPDATE SET
			upload = upload + excluded.upload, download = download + excluded.download, conns = conns + excluded.conns,
			source_name = CASE WHEN excluded.source_name != '' THEN excluded.source_name ELSE source_name END`,
			d.ID, k.hour, k.source, h.name, h.up, h.down, h.conns); err != nil {
			return ConnReportResult{}, err
		}
	}
	for k, v := range traffic {
		if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_hours (hour, name, upload, download) VALUES (?, ?, ?, ?)
			ON CONFLICT (hour, name) DO UPDATE SET upload = upload + excluded.upload, download = download + excluded.download`,
			k.hour, k.name, v[0], v[1]); err != nil {
			return ConnReportResult{}, err
		}
	}
	if lan != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_lan WHERE device_id = ?`, d.ID); err != nil {
			return ConnReportResult{}, err
		}
		for _, a := range lan[:min(len(lan), 16)] {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO device_lan (device_id, mac, ip, updated_at) VALUES (?, ?, ?, ?)`,
				d.ID, strings.ToLower(clipConn(a.MAC)), clipConn(a.IP), now.Format(time.RFC3339)); err != nil {
				return ConnReportResult{}, err
			}
		}
	}
	if s.connPruned.Load() < now.Add(-connPruneEvery).Unix() {
		s.connPruned.Store(now.Unix())
		for _, q := range []struct {
			sql string
			age time.Duration
		}{
			{`DELETE FROM conn_records WHERE end_at < ?`, connRecordKeep},
			{`DELETE FROM conn_minutes WHERE minute < ?`, connMinuteKeep},
			{`DELETE FROM conn_unattributed WHERE minute < ?`, connMinuteKeep},
			{`DELETE FROM conn_batches WHERE received_at < ?`, connMinuteKeep},
		} {
			if _, err := tx.ExecContext(ctx, q.sql, now.Add(-q.age).Format(time.RFC3339)); err != nil {
				return ConnReportResult{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ConnReportResult{}, err
	}
	return ConnReportResult{Acked: acked, Live: s.liveWanted(d.ID)}, nil
}

func (s *Server) connClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, fail func(http.ResponseWriter, error)) {
	mux.Handle("POST /api/v1/client/conns", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in ConnReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{"bad_request", "请求内容无效"})
			return
		}
		res, err := s.RecordConns(r.Context(), d, in)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
}
