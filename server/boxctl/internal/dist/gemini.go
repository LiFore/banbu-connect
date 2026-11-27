package dist

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- Gemini（Antigravity）账号：登记、凭据托管、分配到设备（BXC-F0-017，ADR-046）----
//
// 管理服务保存每个账号的权威凭据：Antigravity 的凭据 JSON（含 refresh token），secret.Box 加密。
// 客户端在本机登录后上传（BXC-F0-020），本机 agy / 应用刷新后回收上传（BXC-F0-018）；每次接受新凭据
// 版本号加 1，客户端上传时带上它拿到的版本号，不一致说明别处已经更新过，拒绝（按版本号确认，参考 clauth）。
// 凭据只下发给分配到的客户端，管理接口不返回凭据内容。

const (
	maxGeminiCredential = 16 << 10
	maxGeminiDevices    = 20
)

// ErrCredentialConflict 是上传凭据时版本号不是最新的。
var ErrCredentialConflict = errors.New("凭据已在别处更新过")

// GeminiAccount 是登记的账号；凭据只返回有没有、版本和来源。
type GeminiAccount struct {
	ID            int64     `json:"id"`
	Email         string    `json:"email"`
	Note          string    `json:"note"`
	MaxDevices    int       `json:"max_devices"`
	Devices       []int64   `json:"devices"` // 分配到的客户端
	HasCredential bool      `json:"has_credential"`
	CredVersion   int64     `json:"cred_version"`
	CredUpdatedAt string    `json:"cred_updated_at"`
	CredSource    string    `json:"cred_source"`
	SubExpires    string    `json:"sub_expires"`         // 订阅到期日 YYYY-MM-DD，管理员填写，只用于提醒
	Plan          string    `json:"plan"`                // 套餐，管理员标记，只用于显示和筛选
	AuthTest      *AuthTest `json:"auth_test,omitempty"` // 最近一次鉴权测试（ADR-061）
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
}

// GeminiAccountInput 是新建或修改。新建时可以同时填凭据（AccessToken / RefreshToken，或在任意一栏粘贴凭据 JSON），
// 凭据里的 id_token 能读出邮箱时可以不填邮箱。修改时忽略这两项，改凭据走 SetGeminiCredential。
type GeminiAccountInput struct {
	Email        string  `json:"email"`
	Note         string  `json:"note"`
	MaxDevices   int     `json:"max_devices"`
	Devices      []int64 `json:"devices"`
	SubExpires   string  `json:"sub_expires"`
	Plan         string  `json:"plan"`
	AccessToken  string  `json:"access_token,omitempty"`
	RefreshToken string  `json:"refresh_token,omitempty"`
}

const selectGemini = `SELECT id, email, note, max_devices, length(credential) > 0, cred_version, cred_updated_at, cred_source,
	sub_expires, plan, created_at, updated_at FROM gemini_accounts`

func scanGemini(sc interface{ Scan(...any) error }) (GeminiAccount, error) {
	var a GeminiAccount
	err := sc.Scan(&a.ID, &a.Email, &a.Note, &a.MaxDevices, &a.HasCredential, &a.CredVersion, &a.CredUpdatedAt, &a.CredSource,
		&a.SubExpires, &a.Plan, &a.CreatedAt, &a.UpdatedAt)
	a.Devices = []int64{}
	return a, err
}

// geminiAssignments 返回每个账号分配到的客户端（不含已吊销的）。
func (s *Server) geminiAssignments(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.account_id, a.device_id FROM gemini_assignments a
		JOIN devices d ON d.id = a.device_id WHERE d.revoked = 0 ORDER BY a.account_id, a.device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var acc, dev int64
		if err := rows.Scan(&acc, &dev); err != nil {
			return nil, err
		}
		out[acc] = append(out[acc], dev)
	}
	return out, rows.Err()
}

func (s *Server) GeminiAccounts(ctx context.Context) ([]GeminiAccount, error) {
	rows, err := s.DB.QueryContext(ctx, selectGemini+` ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GeminiAccount{}
	for rows.Next() {
		a, err := scanGemini(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	assigned, err := s.geminiAssignments(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if d := assigned[out[i].ID]; d != nil {
			out[i].Devices = d
		}
		out[i].AuthTest = s.authTest("gemini", out[i].ID)
	}
	return out, nil
}

func (s *Server) geminiAccount(ctx context.Context, id int64) (GeminiAccount, error) {
	a, err := scanGemini(s.DB.QueryRowContext(ctx, selectGemini+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	assigned, err := s.geminiAssignments(ctx)
	if d := assigned[id]; d != nil {
		a.Devices = d
	}
	return a, err
}

func (in *GeminiAccountInput) normalize() error {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !emailRE.MatchString(in.Email) {
		return &InputError{"邮箱格式不对"}
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 200 {
		return &InputError{"备注最多 200 字"}
	}
	var err error
	if in.SubExpires, err = normalizeExpiry(in.SubExpires); err != nil {
		return err
	}
	var none string
	if err := normalizeProfile(&in.Plan, &none, &none, &none, &none, false); err != nil {
		return err
	}
	if in.MaxDevices == 0 {
		in.MaxDevices = 2
	}
	if in.MaxDevices < 1 || in.MaxDevices > maxGeminiDevices {
		return &InputError{fmt.Sprintf("设备上限要在 1～%d 之间", maxGeminiDevices)}
	}
	seen := map[int64]bool{}
	devices := []int64{}
	for _, d := range in.Devices {
		if d > 0 && !seen[d] {
			seen[d] = true
			devices = append(devices, d)
		}
	}
	in.Devices = devices
	if len(in.Devices) > in.MaxDevices {
		return &InputError{fmt.Sprintf("分配了 %d 台客户端，超过设备上限 %d", len(in.Devices), in.MaxDevices)}
	}
	return nil
}

func geminiUniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return &InputError{"这个邮箱已经登记过"}
	}
	return err
}

// setAssignments 在事务里替换账号的分配。客户端必须存在且没有吊销。
func setAssignments(ctx context.Context, tx *sql.Tx, id int64, devices []int64) error {
	for _, d := range devices {
		var revoked bool
		err := tx.QueryRowContext(ctx, `SELECT revoked FROM devices WHERE id = ?`, d).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked) {
			return &InputError{fmt.Sprintf("客户端 %d 不存在或已吊销", d)}
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gemini_assignments WHERE account_id = ?`, id); err != nil {
		return err
	}
	for _, d := range devices {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gemini_assignments (account_id, device_id) VALUES (?, ?)`, id, d); err != nil {
			return err
		}
	}
	return nil
}

// CreateGeminiAccount 新建账号；填了凭据时一起保存（来源「管理员填写」），凭据无效时不建账号。
func (s *Server) CreateGeminiAccount(ctx context.Context, in GeminiAccountInput) (GeminiAccount, error) {
	var raw []byte
	if strings.TrimSpace(in.AccessToken) != "" || strings.TrimSpace(in.RefreshToken) != "" {
		var info GeminiCredentialInfo
		var err error
		if raw, info, err = geminiCredRaw(in.AccessToken, in.RefreshToken); err != nil {
			return GeminiAccount{}, err
		}
		email := strings.ToLower(strings.TrimSpace(in.Email))
		switch {
		case email == "":
			in.Email = info.Email
		case info.Email != "" && info.Email != email:
			return GeminiAccount{}, &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", info.Email, email)}
		}
	}
	if strings.TrimSpace(in.Email) == "" {
		return GeminiAccount{}, &InputError{"请填写邮箱（凭据里读不出邮箱）"}
	}
	if err := in.normalize(); err != nil {
		return GeminiAccount{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return GeminiAccount{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO gemini_accounts (email, note, max_devices, sub_expires, plan) VALUES (?, ?, ?, ?, ?)`,
		in.Email, in.Note, in.MaxDevices, in.SubExpires, in.Plan)
	if err != nil {
		return GeminiAccount{}, geminiUniqueErr(err)
	}
	id, _ := res.LastInsertId()
	if err := setAssignments(ctx, tx, id, in.Devices); err != nil {
		return GeminiAccount{}, err
	}
	if raw != nil {
		sealed, err := s.Secrets.Seal(raw, geminiCredAAD(id))
		if err != nil {
			return GeminiAccount{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE gemini_accounts SET credential = ?, cred_version = cred_version + 1,
			cred_updated_at = ?, cred_source = '管理员填写' WHERE id = ?`, sealed, time.Now().UTC().Format(time.RFC3339), id); err != nil {
			return GeminiAccount{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GeminiAccount{}, err
	}
	s.geminiChanged()
	return s.geminiAccount(ctx, id)
}

// UpdateGeminiAccount 修改邮箱、备注、设备上限和分配。换了邮箱时清掉凭据：凭据属于原来的账号。
func (s *Server) UpdateGeminiAccount(ctx context.Context, id int64, in GeminiAccountInput) (GeminiAccount, error) {
	if err := in.normalize(); err != nil {
		return GeminiAccount{}, err
	}
	old, err := s.geminiAccount(ctx, id)
	if err != nil {
		return GeminiAccount{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return GeminiAccount{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE gemini_accounts SET email = ?, note = ?, max_devices = ?, sub_expires = ?, plan = ?, updated_at = ? WHERE id = ?`,
		in.Email, in.Note, in.MaxDevices, in.SubExpires, in.Plan, now, id); err != nil {
		return GeminiAccount{}, geminiUniqueErr(err)
	}
	if in.Email != old.Email {
		if err := clearCredential(ctx, tx, id, now); err != nil {
			return GeminiAccount{}, err
		}
	}
	if err := setAssignments(ctx, tx, id, in.Devices); err != nil {
		return GeminiAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return GeminiAccount{}, err
	}
	s.geminiChanged()
	return s.geminiAccount(ctx, id)
}

// clearCredential 清掉凭据；版本号继续递增，客户端据此知道旧凭据作废。
func clearCredential(ctx context.Context, tx *sql.Tx, id int64, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE gemini_accounts SET credential = x'', cred_version = cred_version + 1,
		cred_updated_at = ?, cred_source = '已清除' WHERE id = ?`, now, id)
	return err
}

func (s *Server) DeleteGeminiAccount(ctx context.Context, id int64) error {
	if err := s.execOne(ctx, `DELETE FROM gemini_accounts WHERE id = ?`, id); err != nil {
		return err
	}
	s.geminiChanged()
	return nil
}

// ClearGeminiCredential 清掉账号的凭据（比如账号被盗、要求重新登录），客户端随后删除本机凭据。
func (s *Server) ClearGeminiCredential(ctx context.Context, id int64) (GeminiAccount, error) {
	if _, err := s.geminiAccount(ctx, id); err != nil {
		return GeminiAccount{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return GeminiAccount{}, err
	}
	defer tx.Rollback()
	if err := clearCredential(ctx, tx, id, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return GeminiAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return GeminiAccount{}, err
	}
	s.geminiChanged()
	return s.geminiAccount(ctx, id)
}

// geminiCredRaw 把管理员填写的内容整理成 Antigravity 凭据 JSON：refresh token 必填，access token 可选；
// 也可以在任意一栏粘贴完整的凭据 JSON。
func geminiCredRaw(access, refresh string) ([]byte, GeminiCredentialInfo, error) {
	access, refresh = strings.TrimSpace(access), strings.TrimSpace(refresh)
	var raw []byte
	switch {
	case strings.HasPrefix(refresh, "{"):
		raw = []byte(refresh)
	case strings.HasPrefix(access, "{"):
		raw = []byte(access)
	default:
		if refresh == "" {
			return nil, GeminiCredentialInfo{}, &InputError{"Gemini 账号要填 Refresh Token：访问令牌一小时就过期"}
		}
		raw, _ = json.Marshal(map[string]any{
			"token":       map[string]string{"access_token": access, "token_type": "Bearer", "refresh_token": refresh},
			"auth_method": "consumer",
		})
	}
	info, err := ParseGeminiCredential(raw)
	if err != nil {
		return nil, info, err
	}
	return raw, info, nil
}

// SetGeminiCredential 是管理员手动填写凭据（ADR-061）：refresh token 必填，access token 可选；
// 也可以在任意一栏粘贴完整的 Antigravity 凭据 JSON。客户端用 refresh token 换访问令牌。
func (s *Server) SetGeminiCredential(ctx context.Context, id int64, access, refresh string) (GeminiAccount, error) {
	a, err := s.geminiAccount(ctx, id)
	if err != nil {
		return a, err
	}
	raw, info, err := geminiCredRaw(access, refresh)
	if err != nil {
		return a, err
	}
	if info.Email != "" && info.Email != a.Email {
		return a, &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", info.Email, a.Email)}
	}
	sealed, err := s.Secrets.Seal(raw, geminiCredAAD(id))
	if err != nil {
		return a, err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE gemini_accounts SET credential = ?, cred_version = cred_version + 1,
		cred_updated_at = ?, cred_source = '管理员填写' WHERE id = ?`, sealed, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return a, err
	}
	s.forgetAuthTest("gemini", id)
	s.geminiChanged()
	return s.geminiAccount(ctx, id)
}

// ---- 凭据 ----

func geminiCredAAD(id int64) string { return "gemini_accounts.credential:" + strconv.FormatInt(id, 10) }

// GeminiCredentialInfo 是从凭据 JSON 里读出的、核对用的字段。
type GeminiCredentialInfo struct {
	RefreshToken string
	AccessToken  string
	Expiry       time.Time
	Email        string // 来自 id_token，没有 id_token 时为空
}

// ParseGeminiCredential 检查 Antigravity 凭据 JSON：
// {"token":{"access_token","token_type","refresh_token","expiry"},"auth_method","id_token"}（cockpit-tools 读出的格式）。
// TODO(verify:V-agy-store) 未在真机确认 agy 和应用的凭据格式。
func ParseGeminiCredential(raw []byte) (GeminiCredentialInfo, error) {
	var c struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Expiry       string `json:"expiry"`
		} `json:"token"`
		IDToken string `json:"id_token"`
	}
	if len(raw) > maxGeminiCredential {
		return GeminiCredentialInfo{}, &InputError{"凭据太大"}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return GeminiCredentialInfo{}, &InputError{"凭据不是有效的 JSON"}
	}
	if c.Token.RefreshToken == "" {
		return GeminiCredentialInfo{}, &InputError{"凭据里没有 refresh token"}
	}
	info := GeminiCredentialInfo{RefreshToken: c.Token.RefreshToken, AccessToken: c.Token.AccessToken, Email: idTokenEmail(c.IDToken)}
	if c.Token.Expiry != "" {
		info.Expiry, _ = time.Parse(time.RFC3339Nano, c.Token.Expiry)
	}
	return info, nil
}

// idTokenEmail 读 id_token（JWT）里的 email。只用于核对是不是同一个账号，不校验签名。
func idTokenEmail(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(body, &claims) != nil {
		return ""
	}
	return strings.ToLower(claims.Email)
}

func (s *Server) geminiCredential(ctx context.Context, id int64) ([]byte, int64, error) {
	var sealed []byte
	var version int64
	err := s.DB.QueryRowContext(ctx, `SELECT credential, cred_version FROM gemini_accounts WHERE id = ?`, id).Scan(&sealed, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil || len(sealed) == 0 {
		return nil, version, err
	}
	raw, err := s.Secrets.Open(sealed, geminiCredAAD(id))
	if err != nil {
		return nil, version, fmt.Errorf("解密凭据失败：%w", err)
	}
	return raw, version, nil
}

func (s *Server) assigned(ctx context.Context, accountID, deviceID int64) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gemini_assignments WHERE account_id = ? AND device_id = ?`,
		accountID, deviceID).Scan(&n)
	return n > 0, err
}

// CredentialUpload 是客户端上传的凭据。Replace 用于本机重新登录：新登录的凭据总是取代旧的，不比较版本号。
type CredentialUpload struct {
	BaseVersion int64           `json:"base_version"`
	Replace     bool            `json:"replace"`
	Source      string          `json:"source"` // login / refresh
	Credential  json.RawMessage `json:"credential"`
}

// UploadGeminiCredential 保存客户端上传的凭据，返回新的版本号。账号必须分配给这台客户端；
// 凭据里有 id_token 时，邮箱必须是这个账号；不是重新登录时版本号必须是最新的。
func (s *Server) UploadGeminiCredential(ctx context.Context, d Device, accountID int64, up CredentialUpload) (int64, error) {
	ok, err := s.assigned(ctx, accountID, d.ID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrNotFound
	}
	a, err := s.geminiAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	raw := bytes.TrimSpace(up.Credential)
	info, err := ParseGeminiCredential(raw)
	if err != nil {
		return 0, err
	}
	if info.Email != "" && info.Email != a.Email {
		return 0, &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", info.Email, a.Email)}
	}
	sealed, err := s.Secrets.Seal(raw, geminiCredAAD(accountID))
	if err != nil {
		return 0, err
	}
	source := d.Name + " 刷新"
	if up.Source == "login" {
		source = d.Name + " 登录"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	q := `UPDATE gemini_accounts SET credential = ?, cred_version = cred_version + 1, cred_updated_at = ?, cred_source = ?
		WHERE id = ?`
	args := []any{sealed, now, source, accountID}
	if !up.Replace {
		q += ` AND cred_version = ?`
		args = append(args, up.BaseVersion)
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a.CredVersion, ErrCredentialConflict
	}
	s.geminiChanged()
	var v int64
	err = s.DB.QueryRowContext(ctx, `SELECT cred_version FROM gemini_accounts WHERE id = ?`, accountID).Scan(&v)
	return v, err
}

// geminiChanged 唤醒等待同步的客户端（见 GeminiSyncWait）。
func (s *Server) geminiChanged() {
	s.geminiMu.Lock()
	defer s.geminiMu.Unlock()
	if s.geminiWake != nil {
		close(s.geminiWake)
	}
	s.geminiWake = make(chan struct{})
}

// geminiWaiter 返回下次变化时关闭的通道。
func (s *Server) geminiWaiter() <-chan struct{} {
	s.geminiMu.Lock()
	defer s.geminiMu.Unlock()
	if s.geminiWake == nil {
		s.geminiWake = make(chan struct{})
	}
	return s.geminiWake
}
