package dist

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 接入申请：客户端把设备名称、平台等信息提交给管理服务，管理员在「客户端」页面授权，
// 客户端点「刷新授权」时用申请时拿到的密钥领取令牌并自动绑定，免去手动复制令牌。
//
// 令牌只在领取时生成一次（服务端只存哈希）；领取后申请变为 claimed，再领取需要管理员重新授权。
// 申请接口不需要登录：每个 IP 最多 maxPendingPerIP 条、全部最多 maxPending 条待处理的申请。

// DefaultServerURL 是默认的管理服务地址。
const DefaultServerURL = "https://connect.duallee.cn"

const (
	EnrollPending  = "pending"
	EnrollApproved = "approved"
	EnrollClaimed  = "claimed"
	EnrollRejected = "rejected"

	maxPending      = 200
	maxPendingPerIP = 5
	enrollFile      = "enroll.json"
	installIDFile   = "install_id"
)

// EnrollRequest 是客户端提交的设备信息。
type EnrollRequest struct {
	InstallID  string `json:"install_id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	OSVersion  string `json:"os_version"`
	AppVersion string `json:"app_version"`
}

// EnrollTicket 是申请的回执：客户端保存 ID 和密钥，之后凭它领取令牌。
type EnrollTicket struct {
	ID     int64  `json:"id"`
	Secret string `json:"secret"`
	Status string `json:"status"`
}

// ClaimResult 是领取令牌的结果。只有授权后第一次领取时 Token 不为空。
type ClaimResult struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Token  string `json:"token,omitempty"`
}

// ---- 服务端 ----

// Enrollment 是管理页面看到的接入申请。
type Enrollment struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	OSVersion  string `json:"os_version"`
	AppVersion string `json:"app_version"`
	IP         string `json:"ip"`
	Status     string `json:"status"`
	DeviceID   *int64 `json:"device_id"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

var installIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Enroll 记录一条接入申请。同一台客户端（install_id 相同）再次申请时更新原来那条，回到待处理，旧密钥失效。
func (s *Server) Enroll(ctx context.Context, in EnrollRequest, ip string) (EnrollTicket, error) {
	in.Name = trimTo(in.Name, 64)
	in.Platform, in.OSVersion, in.AppVersion = trimTo(in.Platform, 64), trimTo(in.OSVersion, 64), trimTo(in.AppVersion, 32)
	if !installIDRE.MatchString(in.InstallID) {
		return EnrollTicket{}, &InputError{"安装 ID 无效"}
	}
	if in.Name == "" {
		return EnrollTicket{}, &InputError{"请填写设备名称"}
	}
	secret, err := randomHex(32)
	if err != nil {
		return EnrollTicket{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return EnrollTicket{}, err
	}
	defer tx.Rollback()
	var total, fromIP int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(ip = ?), 0) FROM enrollments
		WHERE status = 'pending' AND install_id != ?`, ip, in.InstallID).Scan(&total, &fromIP); err != nil {
		return EnrollTicket{}, err
	}
	if total >= maxPending || fromIP >= maxPendingPerIP {
		return EnrollTicket{}, ErrTooManyEnrollments
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO enrollments (secret_hash, install_id, name, platform, os_version, app_version, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(install_id) DO UPDATE SET secret_hash = excluded.secret_hash, name = excluded.name,
			platform = excluded.platform, os_version = excluded.os_version, app_version = excluded.app_version,
			ip = excluded.ip, status = 'pending', updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')
		RETURNING id`, hashToken(secret), in.InstallID, in.Name, in.Platform, in.OSVersion, in.AppVersion, ip).Scan(&id)
	if err != nil {
		return EnrollTicket{}, err
	}
	if err := tx.Commit(); err != nil {
		return EnrollTicket{}, err
	}
	return EnrollTicket{ID: id, Secret: secret, Status: EnrollPending}, nil
}

// ErrTooManyEnrollments 表示待处理的申请太多，API 返回 429。
var ErrTooManyEnrollments = errors.New("待处理的接入申请太多，请联系管理员")

// ClaimEnrollment 查询申请状态；已授权时生成令牌并返回，申请变为已领取。
func (s *Server) ClaimEnrollment(ctx context.Context, id int64, secret string) (ClaimResult, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, err
	}
	defer tx.Rollback()
	var r ClaimResult
	var deviceID sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT status, name, device_id FROM enrollments WHERE id = ? AND secret_hash = ?`,
		id, hashToken(secret)).Scan(&r.Status, &r.Name, &deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimResult{}, ErrNotFound
	}
	if err != nil {
		return ClaimResult{}, err
	}
	if r.Status != EnrollApproved {
		return r, nil
	}
	token, err := randomHex(32)
	if err != nil {
		return ClaimResult{}, err
	}
	token = "bc_" + token
	res, err := tx.ExecContext(ctx, `UPDATE devices SET token_hash = ? WHERE id = ? AND revoked = 0`, hashToken(token), deviceID.Int64)
	if err != nil {
		return ClaimResult{}, err
	}
	if n, _ := res.RowsAffected(); !deviceID.Valid || n == 0 {
		// 授权后客户端又被删除或吊销了：按拒绝处理。
		r.Status = EnrollRejected
	} else {
		r.Status, r.Token = EnrollClaimed, token
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrollments SET status = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`,
		r.Status, id); err != nil {
		return ClaimResult{}, err
	}
	return r, tx.Commit()
}

// Enrollments 列出接入申请，待处理的在前。
func (s *Server) Enrollments(ctx context.Context) ([]Enrollment, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, platform, os_version, app_version, ip, status, device_id, created_at, updated_at
		FROM enrollments ORDER BY status != 'pending', updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Enrollment{}
	for rows.Next() {
		var e Enrollment
		var dev sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Name, &e.Platform, &e.OSVersion, &e.AppVersion, &e.IP, &e.Status, &dev, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if dev.Valid {
			e.DeviceID = &dev.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ApproveEnrollment 授权：建一台客户端（之前授权过的沿用原来那台，并解除吊销），等客户端来领取令牌。
// name 为空时用申请里的设备名称。
func (s *Server) ApproveEnrollment(ctx context.Context, id int64, name string) (Enrollment, error) {
	name = trimTo(name, 64)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()
	var reqName, platform, appVersion, status string
	var dev sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT name, platform, app_version, status, device_id FROM enrollments WHERE id = ?`, id).
		Scan(&reqName, &platform, &appVersion, &status, &dev)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrNotFound
	}
	if err != nil {
		return Enrollment{}, err
	}
	// 管理员审批时改了名称就算改过名（之后不被上报的电脑名覆盖）；申请里的名称是客户端上报的。
	custom := name != "" && name != reqName
	if name == "" {
		name = reqName
	}
	// 领取前令牌是一个谁也不知道的随机值，这台客户端在领取前无法同步。
	placeholder, err := randomHex(32)
	if err != nil {
		return Enrollment{}, err
	}
	updated := false
	if dev.Valid {
		res, err := tx.ExecContext(ctx, `UPDATE devices SET name = ?, reported_name = ?, name_custom = ?, revoked = 0, token_hash = ? WHERE id = ?`,
			name, reqName, custom, hashToken(placeholder), dev.Int64)
		if err != nil {
			return Enrollment{}, err
		}
		n, _ := res.RowsAffected()
		updated = n > 0
	}
	if !updated {
		res, err := tx.ExecContext(ctx, `INSERT INTO devices (name, reported_name, name_custom, token_hash, platform, app_version) VALUES (?, ?, ?, ?, ?, ?)`,
			name, reqName, custom, hashToken(placeholder), platform, appVersion)
		if err != nil {
			return Enrollment{}, err
		}
		dev.Int64, _ = res.LastInsertId()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrollments SET status = 'approved', name = ?, device_id = ?,
		updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`, name, dev.Int64, id); err != nil {
		return Enrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Enrollment{}, err
	}
	return s.enrollment(ctx, id)
}

func (s *Server) RejectEnrollment(ctx context.Context, id int64) error {
	return s.execOne(ctx, `UPDATE enrollments SET status = 'rejected', updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = ?`, id)
}

func (s *Server) DeleteEnrollment(ctx context.Context, id int64) error {
	return s.execOne(ctx, `DELETE FROM enrollments WHERE id = ?`, id)
}

func (s *Server) enrollment(ctx context.Context, id int64) (Enrollment, error) {
	list, err := s.Enrollments(ctx)
	if err != nil {
		return Enrollment{}, err
	}
	for _, e := range list {
		if e.ID == id {
			return e, nil
		}
	}
	return Enrollment{}, ErrNotFound
}

func (s *Server) enrollAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	id := func(r *http.Request) (int64, bool) {
		v, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		return v, err == nil
	}
	mux.Handle("GET /api/v1/dist/enrollments", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Enrollments(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("POST /api/v1/dist/enrollments/{id}/approve", authed(func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(r)
		var in struct {
			Name string `json:"name"`
		}
		if !ok {
			fail(w, ErrNotFound)
			return
		}
		if !decode(w, r, &in) {
			return
		}
		e, err := s.ApproveEnrollment(r.Context(), v, in.Name)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, e)
	}))
	action := func(pattern string, f func(context.Context, int64) error) {
		mux.Handle(pattern, authed(func(w http.ResponseWriter, r *http.Request) {
			v, ok := id(r)
			if !ok {
				fail(w, ErrNotFound)
				return
			}
			if err := f(r.Context(), v); err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}))
	}
	action("POST /api/v1/dist/enrollments/{id}/reject", s.RejectEnrollment)
	action("DELETE /api/v1/dist/enrollments/{id}", s.DeleteEnrollment)
}

func (s *Server) enrollClientRoutes(mux *http.ServeMux, log *slog.Logger) {
	mux.Handle("POST /api/v1/client/enroll", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in EnrollRequest
		if !decode(w, r, &in) {
			return
		}
		t, err := s.Enroll(r.Context(), in, remoteIP(r))
		if errors.Is(err, ErrTooManyEnrollments) {
			writeJSON(w, http.StatusTooManyRequests, apiError{"too_many", err.Error()})
			return
		}
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}))
	mux.Handle("POST /api/v1/client/enroll/claim", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID     int64  `json:"id"`
			Secret string `json:"secret"`
		}
		if !decode(w, r, &in) {
			return
		}
		res, err := s.ClaimEnrollment(r.Context(), in.ID, in.Secret)
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, apiError{"not_found", "申请不存在或已被管理员删除，请重新申请"})
			return
		}
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
}

// ---- 客户端 ----

// LocalEnrollment 是本机保存的申请（enroll.json，只有管理员可读）。
type LocalEnrollment struct {
	URL         string    `json:"url"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	ID          int64     `json:"id"`
	Secret      string    `json:"secret"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	RequestedAt time.Time `json:"requested_at"`
	// Token 是已领取、但还没绑定成功（比如服务端还没发布配置）的令牌，下次刷新时重试绑定。
	Token     string `json:"token,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// EnrollmentView 是界面看到的申请，不含密钥和令牌。
type EnrollmentView struct {
	URL         string    `json:"url"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	RequestedAt time.Time `json:"requested_at"`
	LastError   string    `json:"last_error,omitempty"`
}

func (e *LocalEnrollment) view() *EnrollmentView {
	if e == nil {
		return nil
	}
	return &EnrollmentView{URL: e.URL, Name: e.Name, Status: e.Status, RequestedAt: e.RequestedAt, LastError: e.LastError}
}

// InstallID 返回本机的安装 ID，没有时生成。解绑后保留：同一台再次申请时管理员看到的是同一条。
func (c *Client) InstallID() (string, error) {
	path := filepath.Join(c.dir, installIDFile)
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); installIDRE.MatchString(id) {
			return id, nil
		}
	}
	id, err := randomHex(16)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func (c *Client) loadEnrollment() (*LocalEnrollment, error) {
	var e LocalEnrollment
	switch err := readJSON(filepath.Join(c.dir, enrollFile), &e); {
	case err == nil:
		return &e, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	default:
		return nil, err
	}
}

func (c *Client) saveEnrollment(e *LocalEnrollment) error {
	if e == nil {
		err := os.Remove(filepath.Join(c.dir, enrollFile))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeJSON0600(filepath.Join(c.dir, enrollFile), e)
}

// Enrollment 返回本机的申请；没有时为 nil。
func (c *Client) Enrollment() (*EnrollmentView, error) {
	c.enrollMu.Lock()
	defer c.enrollMu.Unlock()
	e, err := c.loadEnrollment()
	return e.view(), err
}

// Enroll 向管理服务提交接入申请。
func (c *Client) Enroll(ctx context.Context, serverURL, fingerprint string, in EnrollRequest) (*EnrollmentView, error) {
	c.enrollMu.Lock()
	defer c.enrollMu.Unlock()
	b, err := normalizeBinding(Binding{URL: serverURL, Token: "-", Fingerprint: fingerprint})
	if err != nil {
		return nil, err
	}
	if in.InstallID, err = c.InstallID(); err != nil {
		return nil, err
	}
	in.AppVersion = c.appVersion
	var t EnrollTicket
	if err := c.postJSON(ctx, b, "/api/v1/client/enroll", in, &t); err != nil {
		return nil, &InputError{"提交申请失败：" + err.Error()}
	}
	e := &LocalEnrollment{URL: b.URL, Fingerprint: b.Fingerprint, ID: t.ID, Secret: t.Secret, Name: strings.TrimSpace(in.Name),
		Status: t.Status, RequestedAt: time.Now().UTC()}
	if err := c.saveEnrollment(e); err != nil {
		return nil, err
	}
	return e.view(), nil
}

// RefreshEnrollment 查询申请；已授权时领取令牌并绑定。返回是否已绑定。
func (c *Client) RefreshEnrollment(ctx context.Context) (*EnrollmentView, bool, error) {
	c.enrollMu.Lock()
	defer c.enrollMu.Unlock()
	e, err := c.loadEnrollment()
	if err != nil || e == nil {
		return nil, false, err
	}
	b := Binding{URL: e.URL, Fingerprint: e.Fingerprint}
	if e.Token == "" {
		var res ClaimResult
		err := c.postJSON(ctx, b, "/api/v1/client/enroll/claim", map[string]any{"id": e.ID, "secret": e.Secret}, &res)
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			e.Status, e.LastError = EnrollRejected, se.Error()
			_ = c.saveEnrollment(e)
			return e.view(), false, nil
		}
		if err != nil {
			return e.view(), false, &InputError{"查询授权失败：" + err.Error()}
		}
		e.Status, e.Token, e.LastError = res.Status, res.Token, ""
		if res.Status == EnrollClaimed && res.Token == "" {
			e.LastError = "授权已被领取过，请让管理员重新授权"
		}
		if err := c.saveEnrollment(e); err != nil {
			return nil, false, err
		}
	}
	if e.Token == "" {
		return e.view(), false, nil
	}
	b.Token = e.Token
	if _, err := c.Bind(ctx, b); err != nil {
		e.LastError = "已授权，但绑定失败：" + strings.TrimPrefix(err.Error(), "连接服务端失败：")
		_ = c.saveEnrollment(e)
		return e.view(), false, nil
	}
	_ = c.saveEnrollment(nil)
	return nil, true, nil
}

// CancelEnrollment 删除本机的申请（服务端的记录留给管理员处理）。
func (c *Client) CancelEnrollment() error {
	c.enrollMu.Lock()
	defer c.enrollMu.Unlock()
	return c.saveEnrollment(nil)
}

type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string { return e.err.Error() }

// postJSON 发一个不带令牌的请求（申请和领取用申请密钥，不需要客户端令牌）。
func (c *Client) postJSON(ctx context.Context, b Binding, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	b.Token = ""
	resp, err := c.doMethod(ctx, b, http.MethodPost, path, bytes.NewReader(body), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &statusError{resp.StatusCode, responseError(resp)}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// normalizeBinding 整理地址和指纹并校验，Bind 和 Enroll 共用。
func normalizeBinding(b Binding) (Binding, error) {
	b.URL = strings.TrimRight(strings.TrimSpace(b.URL), "/")
	b.Token = strings.TrimSpace(b.Token)
	b.Fingerprint = normalizeFingerprint(b.Fingerprint)
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return b, &InputError{"服务端地址须为 https 地址，如 " + DefaultServerURL}
	}
	if b.Token == "" {
		return b, &InputError{"请填写客户端令牌（在管理服务的「客户端」页面生成）"}
	}
	if b.Fingerprint != "" && len(b.Fingerprint) != 64 {
		return b, &InputError{"证书指纹须为 SHA-256（64 位十六进制）"}
	}
	return b, nil
}
