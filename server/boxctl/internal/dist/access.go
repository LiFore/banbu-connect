package dist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

// ---- 客户端接入状态 ----
//
// 客户端此刻怎么上网：经路由盒、受信 Wi-Fi 直连、本机代理、阻断，还是没有使用代理。客户端在状态变化时立即上报，
// 没变化时每分钟上报一次。管理服务记最近一次的快照（device_access），并按接入方式分段记历史
// （device_access_sessions）。与诊断数据分开：不受「诊断上报」开关影响，也不含 Wi-Fi 名称。

const (
	AccessBox     = "box"          // 在路由盒网络上，流量交给路由盒出口
	AccessTrusted = "trusted_wifi" // 在受信 Wi-Fi 上直连
	AccessLocal   = "local"        // 经本机内核（TUN 或系统代理）
	AccessHold    = "hold"         // 受保护服务被阻断
	AccessOff     = "off"          // 没有使用代理：内核没运行，也不在受信网络上

	// accessGap 是客户端多久没上报就算离线（客户端每分钟上报一次）。
	accessGap = 3 * time.Minute
	// accessKeep 是历史分段的保留时间。
	accessKeep = 90 * 24 * time.Hour
	// accessMaxText 是原因等文字的长度上限（字符）。
	accessMaxText = 300
)

func validAccessMode(m string) bool {
	switch m {
	case AccessBox, AccessTrusted, AccessLocal, AccessHold, AccessOff:
		return true
	}
	return false
}

// AccessState 是客户端上报的接入状态。
type AccessState struct {
	Mode        string  `json:"mode"`
	Reason      string  `json:"reason,omitempty"` // Kill Switch 的判断原因，Wi-Fi 名称已去掉
	CoreRunning bool    `json:"core_running"`
	TUN         bool    `json:"tun"`                  // TUN 实际在接管
	ProxyMode   string  `json:"proxy_mode,omitempty"` // rule / global / direct
	KillSwitch  string  `json:"killswitch,omitempty"` // Kill Switch 的状态
	Gate        bool    `json:"gate"`                 // 出口闸门直连在生效
	BoxID       string  `json:"box_id,omitempty"`     // 发现并验证了的路由盒
	BoxAddr     string  `json:"box_addr,omitempty"`
	Network     DiagNet `json:"network"`
	// Since 是客户端进入这个接入方式的时间（客户端时钟）。
	Since time.Time `json:"since"`
}

func clipN(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (a *AccessState) normalize() error {
	if !validAccessMode(a.Mode) {
		return &InputError{"接入方式无效"}
	}
	a.Reason = clipN(a.Reason, accessMaxText)
	a.ProxyMode = clipN(a.ProxyMode, 16)
	a.KillSwitch = clipN(a.KillSwitch, 32)
	a.BoxID = clipN(a.BoxID, 64)
	a.BoxAddr = clipN(a.BoxAddr, 64)
	a.Network.Interface = clipN(a.Network.Interface, 64)
	a.Network.Hint = clipN(a.Network.Hint, accessMaxText)
	return nil
}

// DeviceAccess 是一台客户端最近一次上报的接入状态。
type DeviceAccess struct {
	DeviceID   int64       `json:"device_id"`
	State      AccessState `json:"state"`
	Since      string      `json:"since"` // 管理服务记的这一段的开始时间
	ReportedAt string      `json:"reported_at"`
	Online     bool        `json:"online"` // 最近 accessGap 内上报过
}

// AccessSession 是一段接入方式。
type AccessSession struct {
	Mode    string `json:"mode"`
	BoxID   string `json:"box_id,omitempty"`
	StartAt string `json:"start_at"`
	EndAt   string `json:"end_at"` // 空表示还在进行
	LastAt  string `json:"last_seen"`
}

// RecordAccess 记录一次上报：更新快照；接入方式或路由盒变了、或客户端离线过，就结束上一段、开新的一段。
func (s *Server) RecordAccess(ctx context.Context, deviceID int64, st AccessState, now time.Time) error {
	if err := st.normalize(); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nowS := onlineTime(now)
	var (
		id                 int64
		mode, boxID, lastS string
		startS             string
	)
	err = tx.QueryRowContext(ctx, `SELECT id, mode, box_id, start_at, last_seen FROM device_access_sessions
		WHERE device_id = ? AND end_at = '' ORDER BY id DESC LIMIT 1`, deviceID).Scan(&id, &mode, &boxID, &startS, &lastS)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id = 0
	case err != nil:
		return err
	}
	// 重复的未结束分段（不应出现）一并结束。
	if _, err := tx.ExecContext(ctx, `UPDATE device_access_sessions SET end_at = last_seen
		WHERE device_id = ? AND end_at = '' AND id != ?`, deviceID, id); err != nil {
		return err
	}
	since := nowS
	if id != 0 {
		offline := now.Sub(parseOnlineTime(lastS)) > accessGap
		same := mode == st.Mode && boxID == st.BoxID
		switch {
		case same && !offline:
			if _, err := tx.ExecContext(ctx, `UPDATE device_access_sessions SET last_seen = ? WHERE id = ?`, nowS, id); err != nil {
				return err
			}
			since = startS
		case offline:
			if _, err := tx.ExecContext(ctx, `UPDATE device_access_sessions SET end_at = last_seen WHERE id = ?`, id); err != nil {
				return err
			}
			id = 0
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE device_access_sessions SET end_at = ?, last_seen = ? WHERE id = ?`, nowS, nowS, id); err != nil {
				return err
			}
			id = 0
		}
	}
	if id == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_access_sessions (device_id, mode, box_id, start_at, last_seen)
			VALUES (?, ?, ?, ?, ?)`, deviceID, st.Mode, st.BoxID, nowS, nowS); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_access (device_id, mode, state, since, reported_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET mode = excluded.mode, state = excluded.state, since = excluded.since,
		reported_at = excluded.reported_at`, deviceID, st.Mode, string(raw), since, nowS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_access_sessions WHERE end_at != '' AND end_at < ?`,
		onlineTime(now.Add(-accessKeep))); err != nil {
		return err
	}
	return tx.Commit()
}

// DeviceAccesses 返回所有上报过的客户端的接入状态。
func (s *Server) DeviceAccesses(ctx context.Context, now time.Time) ([]DeviceAccess, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT device_id, state, since, reported_at FROM device_access`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceAccess{}
	for rows.Next() {
		var d DeviceAccess
		var raw string
		if err := rows.Scan(&d.DeviceID, &raw, &d.Since, &d.ReportedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &d.State)
		d.Online = now.Sub(parseOnlineTime(d.ReportedAt)) <= accessGap
		out = append(out, d)
	}
	return out, rows.Err()
}

// AccessHistory 返回一台客户端 from 之后的分段，新的在前。离线的未结束分段按最后一次上报结束。
func (s *Server) AccessHistory(ctx context.Context, deviceID int64, from, now time.Time) ([]AccessSession, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT mode, box_id, start_at, end_at, last_seen FROM device_access_sessions
		WHERE device_id = ? AND (end_at = '' OR end_at >= ?) ORDER BY start_at DESC, id DESC LIMIT 500`, deviceID, onlineTime(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessSession{}
	for rows.Next() {
		var a AccessSession
		if err := rows.Scan(&a.Mode, &a.BoxID, &a.StartAt, &a.EndAt, &a.LastAt); err != nil {
			return nil, err
		}
		if a.EndAt == "" && now.Sub(parseOnlineTime(a.LastAt)) > accessGap {
			a.EndAt = a.LastAt
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Server) accessAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	mux.Handle("GET /api/v1/dist/access", authed(func(w http.ResponseWriter, r *http.Request) {
		out, err := s.DeviceAccesses(r.Context(), time.Now())
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.Handle("GET /api/v1/dist/devices/{id}/access", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, ErrNotFound, log)
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 || days > 90 {
			days = 7
		}
		now := time.Now()
		out, err := s.AccessHistory(r.Context(), id, now.Add(-time.Duration(days)*24*time.Hour), now)
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
}

func (s *Server) accessClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	mux.Handle("POST /api/v1/client/access", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in AccessState
		if !decode(w, r, &in) {
			return
		}
		if err := s.RecordAccess(r.Context(), d.ID, in, time.Now()); err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

// ReportAccess 上报此刻的接入状态。
func (c *Client) ReportAccess(ctx context.Context, st AccessState) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/access", st, nil, requestTimeout)
}
