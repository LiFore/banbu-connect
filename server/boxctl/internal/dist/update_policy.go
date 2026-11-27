package dist

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- 客户端更新方式与最低版本（S1.10，ADR-057）----
//
// 每个更新包可以设置更新方式：
//   - normal：客户端打开首页时提示，用户点「更新」（原来的做法）；
//   - push：客户端后台定时检查，发现后发系统通知；
//   - silent：客户端后台自动下载安装，下次退出、重启或空闲时生效；
//   - force：强制更新：客户端后台自动下载安装；下次打开应用时全屏提示，更新并重启后才能继续使用。
// 另可设置最低版本：客户端版本低于已发布版本里最高的最低版本时同样按 force 处理。
// 只有装了带这些逻辑的版本（0.2.5 之后）才会按这里的方式更新。

const (
	UpdateNormal = "normal"
	UpdatePush   = "push"
	UpdateSilent = "silent"
	UpdateForce  = "force"
)

// ReleasePolicy 是一个更新包的更新方式与最低版本。
type ReleasePolicy struct {
	Mode       string `json:"mode"`
	MinVersion string `json:"min_version"`
}

// SetReleasePolicy 设置更新方式和最低版本。最低版本不能高于这个包自己的版本。
func (s *Server) SetReleasePolicy(ctx context.Context, id int64, p ReleasePolicy) error {
	if p.Mode == "" {
		p.Mode = UpdateNormal
	}
	if p.Mode != UpdateNormal && p.Mode != UpdatePush && p.Mode != UpdateSilent && p.Mode != UpdateForce {
		return &InputError{"更新方式只能是 normal、push、silent 或 force"}
	}
	p.MinVersion = strings.TrimPrefix(strings.TrimSpace(p.MinVersion), "v")
	if p.MinVersion != "" {
		if !versionRE.MatchString(p.MinVersion) {
			return &InputError{"最低版本的格式应为 1.2.3"}
		}
		var version string
		err := s.DB.QueryRowContext(ctx, `SELECT version FROM releases WHERE id = ?`, id).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if CompareVersions(p.MinVersion, version) > 0 {
			return &InputError{"最低版本不能高于这个更新包的版本 " + version}
		}
	}
	return s.execOne(ctx, `UPDATE releases SET mode = ?, min_version = ? WHERE id = ?`, p.Mode, p.MinVersion, id)
}

// updateMode 给出这台客户端应按什么方式更新到 rel：低于发给它的已发布版本里最高的最低版本时为 force。
func (s *Server) updateMode(ctx context.Context, rel Release, target string, device int64, current string) (string, string, error) {
	rows, err := s.DB.QueryContext(ctx, selectRelease+` WHERE target = ? AND published = 1 AND min_version != ''`, target)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	minVersion := ""
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return "", "", err
		}
		if r.For(device) && (minVersion == "" || CompareVersions(r.MinVersion, minVersion) > 0) {
			minVersion = r.MinVersion
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	mode := rel.Mode
	if mode == "" {
		mode = UpdateNormal
	}
	if minVersion != "" && current != "" && CompareVersions(current, minVersion) < 0 {
		mode = UpdateForce
	}
	return mode, minVersion, nil
}

// ---- 各客户端的更新进度（BXC-F0-058）----

// UpdateProgressReport 是客户端上报的更新进度。
type UpdateProgressReport struct {
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Phase   string `json:"phase"` // requested / checking / downloading / installing / installed / failed / none
	Error   string `json:"error,omitempty"`
}

var updatePhases = map[string]bool{"requested": true, "checking": true, "downloading": true, "installing": true, "installed": true, "failed": true, "none": true, "restarting": true}

// RecordUpdateProgress 保存客户端最近一次更新进度。
func (s *Server) RecordUpdateProgress(ctx context.Context, deviceID int64, p UpdateProgressReport) error {
	if !updatePhases[p.Phase] {
		return &InputError{"未知的更新阶段"}
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO device_updates (device_id, version, mode, phase, error, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id) DO UPDATE SET version = excluded.version, mode = excluded.mode, phase = excluded.phase,
		error = excluded.error, updated_at = excluded.updated_at`,
		deviceID, clipConn(p.Version), clipConn(p.Mode), p.Phase, clip(p.Error), time.Now().UTC().Format(time.RFC3339))
	return err
}

// DeviceUpdate 是管理页面上一台客户端的版本与更新进度。
type DeviceUpdate struct {
	DeviceID   int64  `json:"device_id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version"`
	LastSeen   string `json:"last_seen"`
	Version    string `json:"version"` // 正在更新到的版本
	Mode       string `json:"mode"`
	Phase      string `json:"phase"`
	Error      string `json:"error"`
	UpdatedAt  string `json:"updated_at"`
}

// DeviceUpdates 返回未吊销客户端的当前版本与最近一次更新进度。
func (s *Server) DeviceUpdates(ctx context.Context) ([]DeviceUpdate, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.name, d.platform, d.app_version, d.last_seen,
		COALESCE(u.version, ''), COALESCE(u.mode, ''), COALESCE(u.phase, ''), COALESCE(u.error, ''), COALESCE(u.updated_at, '')
		FROM devices d LEFT JOIN device_updates u ON u.device_id = d.id WHERE d.revoked = 0 ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceUpdate{}
	for rows.Next() {
		var u DeviceUpdate
		if err := rows.Scan(&u.DeviceID, &u.Name, &u.Platform, &u.AppVersion, &u.LastSeen, &u.Version, &u.Mode, &u.Phase, &u.Error, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Server) updatePolicyAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	mux.Handle("PUT /api/v1/dist/releases/{id}/policy", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, ErrNotFound, log)
			return
		}
		var in ReleasePolicy
		if !decode(w, r, &in) {
			return
		}
		if err := s.SetReleasePolicy(r.Context(), id, in); err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("GET /api/v1/dist/update-status", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.DeviceUpdates(r.Context())
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
}

func (s *Server) updateClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	mux.Handle("POST /api/v1/client/update/progress", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in UpdateProgressReport
		if !decode(w, r, &in) {
			return
		}
		if err := s.RecordUpdateProgress(r.Context(), d.ID, in); err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

// ---- 客户端 ----

// ReportUpdateProgress 把更新进度报给管理服务。
func (c *Client) ReportUpdateProgress(ctx context.Context, p UpdateProgressReport) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/update/progress", p, nil, requestTimeout)
}
