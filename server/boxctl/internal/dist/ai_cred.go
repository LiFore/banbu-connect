package dist

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---- Claude / Codex 的 OAuth 凭据托管、下发与额度（ADR-061）----
//
// 管理员可以手动填入 access token / refresh token（或粘贴 Claude 的凭据 JSON、Codex 的 auth.json），
// 客户端也可以把本机登录、刷新后的凭据上传。和 Gemini 一样按版本号乐观锁：每次接受新凭据版本号加 1，
// 客户端上传时带上它拿到的版本号，不一致说明别处已经更新过，拒绝。
// 凭据只下发给允许使用这个账号的客户端（ADR-060 的名单），管理接口不返回凭据内容。
// 管理服务自己不向 Anthropic / OpenAI 发请求：额度和鉴权测试都由客户端查询后回报。

const maxAICredential = 32 << 10

// AICredential 是统一格式的 OAuth 凭据。
type AICredential struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 访问令牌过期时间，Unix 毫秒；0 表示不知道
	IDToken      string `json:"id_token,omitempty"`   // Codex
	AccountID    string `json:"account_id,omitempty"` // Codex：ChatGPT-Account-Id
}

// AIQuotaWindow 是额度的一个窗口（Claude 的 5 小时 / 每周，Codex 的 primary / secondary）。
type AIQuotaWindow struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetAt     string  `json:"reset_at,omitempty"`
}

// AIQuota 是一个账号的额度。
type AIQuota struct {
	Windows   []AIQuotaWindow `json:"windows"`
	Plan      string          `json:"plan,omitempty"`
	Error     string          `json:"error,omitempty"`
	CheckedAt time.Time       `json:"checked_at"`
}

// jwtClaims 读 JWT 的载荷（不校验签名，只用于核对账号和取字段）。
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	return m
}

// CodexTokenInfo 从 Codex 的 JWT 里读邮箱、ChatGPT-Account-Id 和过期时间（Unix 毫秒）。
func CodexTokenInfo(tok string) (email, accountID string, expMs int64) {
	c := jwtClaims(tok)
	if c == nil {
		return "", "", 0
	}
	if e, ok := c["email"].(string); ok {
		email = strings.ToLower(e)
	}
	if p, ok := c["https://api.openai.com/profile"].(map[string]any); ok && email == "" {
		if e, ok := p["email"].(string); ok {
			email = strings.ToLower(e)
		}
	}
	if a, ok := c["https://api.openai.com/auth"].(map[string]any); ok {
		accountID, _ = a["chatgpt_account_id"].(string)
	}
	if exp, ok := c["exp"].(float64); ok {
		expMs = int64(exp) * 1000
	}
	return email, accountID, expMs
}

// Normalize 补齐 Codex 能从 JWT 读出的字段，检查至少有一个令牌。
func (c *AICredential) Normalize(kind string) error {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.IDToken = strings.TrimSpace(c.IDToken)
	c.AccountID = strings.TrimSpace(c.AccountID)
	if c.AccessToken == "" && c.RefreshToken == "" {
		return &InputError{"至少要填 Access Token 或 Refresh Token"}
	}
	if kind == KindCodex {
		for _, t := range []string{c.AccessToken, c.IDToken} {
			if _, acc, exp := CodexTokenInfo(t); acc != "" || exp > 0 {
				if c.AccountID == "" {
					c.AccountID = acc
				}
				if c.ExpiresAt == 0 && t == c.AccessToken {
					c.ExpiresAt = exp
				}
			}
		}
	}
	if b, _ := json.Marshal(c); len(b) > maxAICredential {
		return &InputError{"凭据太大"}
	}
	return nil
}

// Email 是凭据里能读出的邮箱（只有 Codex 的 id_token 有），读不出时为空。
func (c AICredential) Email() string {
	e, _, _ := CodexTokenInfo(c.IDToken)
	return e
}

// ParseAICredentialInput 解析管理员填写的内容：access / refresh 两栏各填一个令牌，
// 或在任意一栏粘贴 Claude 的凭据 JSON（{"claudeAiOauth":{...}}）、Codex 的 auth.json（{"tokens":{...}}）。
func ParseAICredentialInput(kind, access, refresh string) (AICredential, error) {
	access, refresh = strings.TrimSpace(access), strings.TrimSpace(refresh)
	for _, s := range []string{access, refresh} {
		if strings.HasPrefix(s, "{") {
			c, err := parseAICredentialJSON(s)
			if err != nil {
				return c, err
			}
			return c, c.Normalize(kind)
		}
	}
	c := AICredential{AccessToken: access, RefreshToken: refresh}
	return c, c.Normalize(kind)
}

func parseAICredentialJSON(s string) (AICredential, error) {
	var raw struct {
		Claude *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
		Tokens *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		AICredential
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return AICredential{}, &InputError{"粘贴的内容不是有效的 JSON"}
	}
	switch {
	case raw.Claude != nil:
		return AICredential{AccessToken: raw.Claude.AccessToken, RefreshToken: raw.Claude.RefreshToken, ExpiresAt: raw.Claude.ExpiresAt}, nil
	case raw.Tokens != nil:
		t := raw.Tokens
		return AICredential{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, AccountID: t.AccountID}, nil
	}
	return raw.AICredential, nil
}

func aiCredAAD(id int64) string { return "claude_accounts.credential:" + strconv.FormatInt(id, 10) }

func (s *Server) aiCredential(ctx context.Context, id int64) (*AICredential, int64, error) {
	var sealed []byte
	var version int64
	err := s.DB.QueryRowContext(ctx, `SELECT credential, cred_version FROM claude_accounts WHERE id = ?`, id).Scan(&sealed, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil || len(sealed) == 0 {
		return nil, version, err
	}
	raw, err := s.Secrets.Open(sealed, aiCredAAD(id))
	if err != nil {
		return nil, version, fmt.Errorf("解密凭据失败：%w", err)
	}
	var c AICredential
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, version, err
	}
	return &c, version, nil
}

// storeAICredential 保存凭据，版本号加 1。base >= 0 时要求当前版本号等于 base，否则返回 ErrCredentialConflict。
func (s *Server) storeAICredential(ctx context.Context, id int64, c AICredential, source string, base int64) (int64, error) {
	raw, _ := json.Marshal(c)
	sealed, err := s.Secrets.Seal(raw, aiCredAAD(id))
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	q := `UPDATE claude_accounts SET credential = ?, cred_version = cred_version + 1, cred_updated_at = ?, cred_source = ?,
		cred_expires = ?, refresh_error = '' WHERE id = ?`
	args := []any{sealed, now, source, c.ExpiresAt, id}
	if base >= 0 {
		q += ` AND cred_version = ?`
		args = append(args, base)
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var v int64
	if err := s.DB.QueryRowContext(ctx, `SELECT cred_version FROM claude_accounts WHERE id = ?`, id).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return v, ErrCredentialConflict
	}
	s.geminiChanged()
	return v, nil
}

// clearAICredential 清掉凭据和额度；版本号继续递增，客户端据此知道旧凭据作废。
func (s *Server) clearAICredential(ctx context.Context, id int64, source string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET credential = x'', cred_version = cred_version + 1,
		cred_updated_at = ?, cred_source = ?, cred_expires = 0, refresh_error = '', quota = '', quota_at = '', quota_device = '' WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), source, id)
	if err == nil {
		s.geminiChanged()
	}
	return err
}

// SetClaudeCredential 是管理员手动填写凭据。Codex 的 id_token 能读出邮箱时必须是这个账号。
func (s *Server) SetClaudeCredential(ctx context.Context, id int64, access, refresh string) (ClaudeAccount, error) {
	a, err := s.claudeAccount(ctx, id)
	if err != nil {
		return a, err
	}
	c, err := ParseAICredentialInput(a.Kind, access, refresh)
	if err != nil {
		return a, err
	}
	if e := c.Email(); e != "" && e != a.Email {
		return a, &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", e, a.Email)}
	}
	if _, err := s.storeAICredential(ctx, id, c, "管理员填写", -1); err != nil {
		return a, err
	}
	s.forgetAuthTest(a.Kind, id)
	return s.claudeAccount(ctx, id)
}

// ClearClaudeCredential 清掉托管的凭据；客户端本机工作区里已有的登录不动。
func (s *Server) ClearClaudeCredential(ctx context.Context, id int64) (ClaudeAccount, error) {
	if _, err := s.claudeAccount(ctx, id); err != nil {
		return ClaudeAccount{}, err
	}
	if err := s.clearAICredential(ctx, id, "已清除"); err != nil {
		return ClaudeAccount{}, err
	}
	return s.claudeAccount(ctx, id)
}

// AICredentialUpload 是客户端上传的凭据。Replace 用于本机重新登录：总是取代旧的，不比较版本号。
type AICredentialUpload struct {
	BaseVersion int64        `json:"base_version"`
	Replace     bool         `json:"replace"`
	Source      string       `json:"source"` // login / refresh
	Credential  AICredential `json:"credential"`
}

// UploadClaudeCredential 保存客户端上传的凭据，返回新的版本号。
func (s *Server) UploadClaudeCredential(ctx context.Context, d Device, id int64, up AICredentialUpload) (int64, error) {
	a, err := s.claudeAccount(ctx, id)
	if err != nil {
		return 0, err
	}
	if !allowed(a, d.ID) {
		return 0, ErrNotFound
	}
	c := up.Credential
	if err := c.Normalize(a.Kind); err != nil {
		return 0, err
	}
	if e := c.Email(); e != "" && e != a.Email {
		return 0, &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", e, a.Email)}
	}
	source := d.Name + " 刷新"
	if up.Source == "login" {
		source = d.Name + " 登录"
	}
	base := up.BaseVersion
	if up.Replace {
		base = -1
	}
	return s.storeAICredential(ctx, id, c, source, base)
}

// ---- 客户端同步 ----

// AISyncAccount 是允许这台客户端使用的 Claude / Codex 账号；Credential 为空表示没有托管凭据。
type AISyncAccount struct {
	ID         int64         `json:"id"`
	Kind       string        `json:"kind"`
	Email      string        `json:"email"`
	Note       string        `json:"note"`
	Version    int64         `json:"version"`
	Credential *AICredential `json:"credential,omitempty"`
	// Mail 表示管理服务授权了这个账号的 Gmail：Claude 可以一键登录（服务端取登录链接），客户端能看登录邮件。
	// 账号不一定是 Gmail 注册的；没有时客户端改用 claude / codex 自己的登录流程，登录后上报凭据（ADR-067）。
	Mail bool `json:"mail"`
	// TokenOnly 表示这台客户端只拿 Access Token（ADR-071）：Credential 里没有 Refresh Token，续期由管理服务安排；
	// 本机登录得到的 Refresh Token 上报后要从工作区去掉。Codex 的持有者为 false，拿完整凭据。
	TokenOnly bool `json:"token_only"`
	// Refresh 是进行中的续期：notice 阶段本机正在用这个账号时要提醒用户，并回报忙不忙。
	Refresh *AIRefreshNoticeView `json:"refresh,omitempty"`
}

// AISync 是一次同步的结果：账号与凭据，以及交给这台客户端执行的鉴权测试。
type AISync struct {
	Hash     string          `json:"hash"`
	Accounts []AISyncAccount `json:"accounts"`
	Tests    []AuthTest      `json:"tests"`
	Jobs     []AIRefreshJob  `json:"jobs"` // 交给这台客户端执行的续期（ADR-071）
}

// AISync 返回这台客户端应持有的 Claude / Codex 账号与凭据。
func (s *Server) AISync(ctx context.Context, deviceID int64) (AISync, error) {
	out := AISync{Accounts: []AISyncAccount{}, Tests: s.deviceTests(deviceID, KindClaude, KindCodex), Jobs: []AIRefreshJob{}}
	all, err := s.ClaudeAccounts(ctx)
	if err != nil {
		return out, err
	}
	for _, a := range all {
		if !allowed(a, deviceID) {
			continue
		}
		sa := AISyncAccount{ID: a.ID, Kind: a.Kind, Email: a.Email, Note: a.Note, Version: a.CredVersion, Mail: a.MailSource != "",
			TokenOnly: tokenOnly(a, deviceID)}
		var job *AIRefreshState
		sa.Refresh, job = s.refreshView(a.ID, deviceID)
		if a.HasCredential {
			full, _, err := s.aiCredential(ctx, a.ID)
			if err != nil {
				return out, err
			}
			sa.Credential = full
			if sa.TokenOnly {
				sa.Credential = stripRefresh(full)
			}
			if job != nil && full != nil && full.RefreshToken != "" && job.base == a.CredVersion {
				out.Jobs = append(out.Jobs, AIRefreshJob{ID: job.ID, AccountID: a.ID, Kind: a.Kind, BaseVersion: a.CredVersion, Credential: *full})
			}
		}
		out.Accounts = append(out.Accounts, sa)
	}
	body, _ := json.Marshal(out)
	sum := sha256.Sum256(body)
	out.Hash = hex.EncodeToString(sum[:12])
	return out, nil
}

// AISyncWait 在结果的 hash 与 since 不同时立即返回，否则等变化，最多 wait。
// canRefresh 表示客户端能执行续期任务（新版客户端同步时带 caps=refresh），只有这样的客户端会被挑去续期。
func (s *Server) AISyncWait(ctx context.Context, deviceID int64, since string, wait time.Duration, canRefresh bool) (AISync, error) {
	if canRefresh {
		s.noteRefreshCap(deviceID)
	}
	wait = min(wait, maxSyncWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woke := s.geminiWaiter()
		out, err := s.AISync(ctx, deviceID)
		if err != nil || out.Hash != since || wait <= 0 {
			return out, err
		}
		select {
		case <-woke:
		case <-deadline.C:
			return out, nil
		case <-ctx.Done():
			return out, nil
		}
	}
}

// AIQuotaSample 是客户端上报的一个账号的额度。
type AIQuotaSample struct {
	AccountID int64   `json:"account_id"`
	Quota     AIQuota `json:"quota"`
}

// RecordAIQuota 保存客户端上报的额度。只接受允许这台客户端使用的账号。
func (s *Server) RecordAIQuota(ctx context.Context, d Device, samples []AIQuotaSample) error {
	if len(samples) > maxQuotaSamples {
		return &InputError{"一次上报的账号太多"}
	}
	for _, smp := range samples {
		a, err := s.claudeAccount(ctx, smp.AccountID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !allowed(a, d.ID) || len(smp.Quota.Windows) > maxQuotaBuckets {
			continue
		}
		if err := s.saveAIQuota(ctx, smp.AccountID, d.Name, smp.Quota); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) saveAIQuota(ctx context.Context, id int64, device string, q AIQuota) error {
	if q.CheckedAt.IsZero() {
		q.CheckedAt = time.Now()
	}
	if len([]rune(q.Error)) > 300 {
		q.Error = string([]rune(q.Error)[:300])
	}
	b, _ := json.Marshal(q)
	_, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET quota = ?, quota_at = ?, quota_device = ? WHERE id = ?`,
		string(b), q.CheckedAt.UTC().Format(time.RFC3339), device, id)
	return err
}

// ---- 鉴权测试 ----
//
// 管理页面点「测试鉴权」→ 管理服务挑一台允许使用这个账号、最近联系过的客户端，把测试放进它的同步结果 →
// 客户端用托管的凭据（必要时先用 refresh token 刷新）查一次额度，回报结果。测试只放在内存里。

const (
	authTestTimeout = 90 * time.Second // 客户端多久没回报算失败
	authTestOnline  = 5 * time.Minute  // 多久内联系过管理服务算在线
)

// 测试状态。
const (
	TestPending = "pending"
	TestOK      = "ok"
	TestFailed  = "failed"
)

// AuthTest 是一次鉴权测试。Kind 是 claude / codex / gemini。
type AuthTest struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	AccountID  int64    `json:"account_id"`
	DeviceID   int64    `json:"device_id"`
	DeviceName string   `json:"device_name"`
	Status     string   `json:"status"`
	Message    string   `json:"message,omitempty"`
	Quota      *AIQuota `json:"quota,omitempty"`
	StartedAt  string   `json:"started_at"`
	started    time.Time
}

func authTestKey(kind string, id int64) string { return kind + ":" + strconv.FormatInt(id, 10) }

// expireTestsLocked 把超时没回报的测试标为失败。调用方持有 testMu。
func (s *Server) expireTestsLocked(now time.Time) {
	for _, t := range s.authTests {
		if t.Status == TestPending && now.Sub(t.started) > authTestTimeout {
			t.Status, t.Message = TestFailed, fmt.Sprintf("「%s」没有回报结果：可能离线、托盘没在运行，或客户端版本太旧", t.DeviceName)
		}
	}
}

func (s *Server) authTest(kind string, id int64) *AuthTest {
	s.testMu.Lock()
	defer s.testMu.Unlock()
	s.expireTestsLocked(time.Now())
	if t := s.authTests[authTestKey(kind, id)]; t != nil {
		c := *t
		return &c
	}
	return nil
}

func (s *Server) forgetAuthTest(kind string, id int64) {
	s.testMu.Lock()
	delete(s.authTests, authTestKey(kind, id))
	s.testMu.Unlock()
}

// deviceTests 返回交给这台客户端、还没回报的测试。
func (s *Server) deviceTests(deviceID int64, kinds ...string) []AuthTest {
	s.testMu.Lock()
	defer s.testMu.Unlock()
	s.expireTestsLocked(time.Now())
	out := []AuthTest{}
	for _, t := range s.authTests {
		if t.DeviceID == deviceID && t.Status == TestPending && slices.Contains(kinds, t.Kind) {
			out = append(out, AuthTest{ID: t.ID, Kind: t.Kind, AccountID: t.AccountID})
		}
	}
	slices.SortFunc(out, func(a, b AuthTest) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// pickTestDevice 从候选客户端里挑最近联系过管理服务、没有吊销的一台。
func (s *Server) pickTestDevice(ctx context.Context, candidates []int64) (int64, string, error) {
	if len(candidates) == 0 {
		return 0, "", &InputError{"这个账号还没有分配给任何客户端：鉴权测试由客户端执行"}
	}
	var best int64
	var bestName, bestSeen string
	for _, id := range candidates {
		var name, seen string
		var revoked bool
		err := s.DB.QueryRowContext(ctx, `SELECT name, last_seen, revoked FROM devices WHERE id = ?`, id).Scan(&name, &seen, &revoked)
		if errors.Is(err, sql.ErrNoRows) || revoked {
			continue
		}
		if err != nil {
			return 0, "", err
		}
		if seen > bestSeen {
			best, bestName, bestSeen = id, name, seen
		}
	}
	t, err := time.Parse(time.RFC3339, bestSeen)
	if best == 0 || err != nil || time.Since(t) > authTestOnline {
		return 0, "", &InputError{"分配到的客户端最近 5 分钟都没有联系管理服务：鉴权测试要由在线的客户端执行"}
	}
	return best, bestName, nil
}

// StartAuthTest 发起鉴权测试。kind 为 gemini 时 id 是 Gemini 账号，否则是 Claude / Codex 账号。
func (s *Server) StartAuthTest(ctx context.Context, kind string, id int64) (AuthTest, error) {
	var candidates []int64
	var has bool
	if kind == "gemini" {
		a, err := s.geminiAccount(ctx, id)
		if err != nil {
			return AuthTest{}, err
		}
		candidates, has = a.Devices, a.HasCredential
	} else {
		a, err := s.claudeAccount(ctx, id)
		if err != nil {
			return AuthTest{}, err
		}
		kind, candidates, has = a.Kind, a.Devices, a.HasCredential
	}
	if !has {
		return AuthTest{}, &InputError{"这个账号还没有凭据：先填入 Access Token 或 Refresh Token"}
	}
	dev, name, err := s.pickTestDevice(ctx, candidates)
	if err != nil {
		return AuthTest{}, err
	}
	now := time.Now()
	t := &AuthTest{ID: newLoginID(), Kind: kind, AccountID: id, DeviceID: dev, DeviceName: name, Status: TestPending,
		StartedAt: now.UTC().Format(time.RFC3339), started: now}
	s.testMu.Lock()
	if s.authTests == nil {
		s.authTests = map[string]*AuthTest{}
	}
	s.authTests[authTestKey(kind, id)] = t
	out := *t
	s.testMu.Unlock()
	s.geminiChanged() // 唤醒这台客户端的同步
	return out, nil
}

// AuthTestResult 是客户端回报的测试结果。
type AuthTestResult struct {
	OK    bool     `json:"ok"`
	Error string   `json:"error"`
	Quota *AIQuota `json:"quota,omitempty"`
}

// FinishAuthTest 记录客户端回报的结果；Claude / Codex 测试成功并带了额度时一并保存。
func (s *Server) FinishAuthTest(ctx context.Context, d Device, id string, r AuthTestResult) error {
	s.testMu.Lock()
	var t *AuthTest
	for _, x := range s.authTests {
		if x.ID == id && x.DeviceID == d.ID {
			t = x
		}
	}
	if t == nil {
		s.testMu.Unlock()
		return ErrNotFound
	}
	msg := strings.TrimSpace(r.Error)
	if len([]rune(msg)) > 300 {
		msg = string([]rune(msg)[:300])
	}
	if r.OK {
		t.Status, t.Message = TestOK, ""
	} else {
		if msg == "" {
			msg = "鉴权失败"
		}
		t.Status, t.Message = TestFailed, msg
	}
	t.Quota = r.Quota
	kind, account := t.Kind, t.AccountID
	s.testMu.Unlock()
	s.geminiChanged()
	if kind != "gemini" && r.Quota != nil {
		return s.saveAIQuota(ctx, account, d.Name, *r.Quota)
	}
	return nil
}
