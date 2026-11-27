package dist

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ---- Claude 账号（BXC-F1-083）----
//
// 服务端登记账号的邮箱（也是它自己的 Gmail，Q-006）和 Gmail API 的授权（refresh token，加密保存，
// ADR-025），不保存 Claude 的密码。管理员可以填入 OAuth 凭据由服务端托管并下发（ADR-061，见 ai_cred.go）。客户端发起 Claude Code 登录后，服务端从这个 Gmail 取
// 登录邮件里的 magic link 交给客户端（BXC-F1-084）；登录本身由客户端本机的 Claude Code 完成（BXC-F1-085）。

const (
	// 登录邮件的发件人形如 no-reply-<随机串>@mail.anthropic.com（2026-11-12 实测）。
	claudeMailDomain = "mail.anthropic.com"

	loginTTL       = 10 * time.Minute // 发起后多久内等邮件
	loginKeep      = 30 * time.Minute // 结束的登录保留多久供查询状态
	mailPollEvery  = 5 * time.Second
	mailClockSkew  = 2 * time.Minute // 邮件服务器时间与本机的误差
	maxLinkWait    = 30 * time.Second
	maxMailMessage = 2 << 20
)

// magicLinkRE 匹配登录邮件里的链接，形如 https://claude.ai/magic-link#<十六进制>（实测）。
var magicLinkRE = regexp.MustCompile(`https://claude\.ai/magic-link#[^\s"'<>()（）]+`)

var emailRE = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)

// 账号类型（BXC-F0-045，ADR-053）：两种都由管理服务从账号自己的 Gmail 读登录邮件，
// Claude 取 magic link 并支持一键登录，Codex 取 OpenAI 发来的 6 位验证码。
const (
	KindClaude = "claude"
	KindCodex  = "codex"
	// KindGemini 是 Gemini 账号（gemini_accounts，单独一张表），只用于在线时长等跨类型的统计。
	KindGemini = "gemini"
)

// ClaudeAccount 是登记的账号（Claude 或 Codex）；Gmail 授权只返回有没有。
type ClaudeAccount struct {
	ID              int64   `json:"id"`
	Kind            string  `json:"kind"`
	Email           string  `json:"email"`
	Note            string  `json:"note"`
	Devices         []int64 `json:"devices"` // 允许使用的客户端；空表示没有客户端能用（ADR-060）
	GmailAuthorized bool    `json:"gmail_authorized"`
	// 自建邮箱（IMAP，如 BillionMail）：登录名（空表示用账号邮箱）和有没有密码；填了密码时优先于 Gmail。
	MailUser   string `json:"mail_user"`
	HasMailbox bool   `json:"has_mailbox"`
	MailSource string `json:"mail_source"` // 读登录邮件的方式：imap、gmail，空表示没有
	// 管理员标记（只用于显示和筛选）：套餐；Claude 还区分个人号和 Team 号。
	Plan            string `json:"plan"`
	AccountType     string `json:"account_type"` // personal、team
	TeamRole        string `json:"team_role"`    // Team 号：owner（主号）、member（成员号）
	TeamName        string `json:"team_name"`
	TeamDomain      string `json:"team_domain"`
	LastLoginAt     string `json:"last_login_at"`
	LastLoginDevice string `json:"last_login_device"`
	LastError       string `json:"last_error"`
	// 托管的 OAuth 凭据（ADR-061）：只返回有没有、版本和来源。
	HasCredential bool   `json:"has_credential"`
	CredVersion   int64  `json:"cred_version"`
	CredUpdatedAt string `json:"cred_updated_at"`
	CredSource    string `json:"cred_source"`
	// 客户端最近上报的额度（ADR-061）。
	Quota       *AIQuota  `json:"quota,omitempty"`
	QuotaDevice string    `json:"quota_device"`
	SubExpires  string    `json:"sub_expires"`         // 订阅到期日 YYYY-MM-DD，管理员填写，只用于提醒（不向服务商查询）
	AuthTest    *AuthTest `json:"auth_test,omitempty"` // 最近一次鉴权测试
	// 令牌集中续期（ADR-071）。
	HolderDevice int64           `json:"holder_device"` // Codex：指定的持有者，0 表示自动
	Holder       int64           `json:"holder"`        // Codex：实际的持有者（持有 Refresh Token 的客户端），没有分配客户端时为 0
	CredExpires  int64           `json:"cred_expires"`  // 托管的 Access Token 过期时间（Unix 毫秒），0 表示不知道
	RefreshAt    string          `json:"refresh_at"`    // 最近一次续期成功的时间
	RefreshError string          `json:"refresh_error"` // 最近一次续期失败的原因
	Refresh      *AIRefreshState `json:"refresh,omitempty"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

// ClaudeAccountInput 是新建或修改。Kind 为空时新建为 claude、修改时沿用原类型；类型建好后不能改
// （Claude 和 Codex 是两类账号，和 Gemini 一样分开登记）。
// 新建时可以同时填凭据（AccessToken / RefreshToken，或在任意一栏粘贴凭据 JSON），Codex 的 id_token 能读出邮箱时
// 可以不填邮箱。修改时忽略这两项，改凭据走 SetClaudeCredential。
type ClaudeAccountInput struct {
	Kind         string  `json:"kind"`
	Email        string  `json:"email"`
	Note         string  `json:"note"`
	Devices      []int64 `json:"devices"`
	SubExpires   string  `json:"sub_expires"`
	HolderDevice int64   `json:"holder_device"` // 只对 Codex 有效，0 表示自动
	Plan         string  `json:"plan"`
	AccountType  string  `json:"account_type"` // 只对 Claude 有效：personal（默认）、team
	TeamRole     string  `json:"team_role"`    // Team 号：owner、member
	TeamName     string  `json:"team_name"`
	TeamDomain   string  `json:"team_domain"`
	MailUser     string  `json:"mail_user"`               // 自建邮箱的 IMAP 登录名，空表示用账号邮箱
	MailPassword string  `json:"mail_password,omitempty"` // 自建邮箱的 IMAP 密码；修改时为空表示不变
	AccessToken  string  `json:"access_token,omitempty"`
	RefreshToken string  `json:"refresh_token,omitempty"`
}

// ClientClaudeAccount 是下发给客户端的账号，只有类型、邮箱和备注。
type ClientClaudeAccount struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Email string `json:"email"`
	Note  string `json:"note"`
}

const selectClaude = `SELECT id, kind, email, note, devices, length(gmail_token) > 0, last_login_at, last_login_device, last_error,
	length(credential) > 0, cred_version, cred_updated_at, cred_source, quota, quota_device, sub_expires, holder_device, cred_expires, refresh_at, refresh_error,
	mail_user, length(mail_password) > 0, plan, account_type, team_role, team_name, team_domain,
	created_at, updated_at FROM claude_accounts`

func scanClaude(sc interface{ Scan(...any) error }) (ClaudeAccount, error) {
	var a ClaudeAccount
	var devices, quota string
	if err := sc.Scan(&a.ID, &a.Kind, &a.Email, &a.Note, &devices, &a.GmailAuthorized, &a.LastLoginAt, &a.LastLoginDevice, &a.LastError,
		&a.HasCredential, &a.CredVersion, &a.CredUpdatedAt, &a.CredSource, &quota, &a.QuotaDevice, &a.SubExpires, &a.HolderDevice, &a.CredExpires, &a.RefreshAt, &a.RefreshError,
		&a.MailUser, &a.HasMailbox, &a.Plan, &a.AccountType, &a.TeamRole, &a.TeamName, &a.TeamDomain,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	_ = json.Unmarshal([]byte(devices), &a.Devices)
	if quota != "" {
		var q AIQuota
		if json.Unmarshal([]byte(quota), &q) == nil {
			a.Quota = &q
		}
	}
	if a.Devices == nil {
		a.Devices = []int64{}
	}
	a.Holder = holderOf(a)
	switch {
	case a.HasMailbox:
		a.MailSource = "imap"
	case a.GmailAuthorized:
		a.MailSource = "gmail"
	}
	return a, nil
}

func (s *Server) ClaudeAccounts(ctx context.Context) ([]ClaudeAccount, error) {
	rows, err := s.DB.QueryContext(ctx, selectClaude+` ORDER BY kind, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClaudeAccount{}
	for rows.Next() {
		a, err := scanClaude(rows)
		if err != nil {
			return nil, err
		}
		a.AuthTest = s.authTest(a.Kind, a.ID)
		a.Refresh = s.refreshState(a.ID)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Server) claudeAccount(ctx context.Context, id int64) (ClaudeAccount, error) {
	a, err := scanClaude(s.DB.QueryRowContext(ctx, selectClaude+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err == nil {
		a.Refresh = s.refreshState(id)
	}
	return a, err
}

func (in *ClaudeAccountInput) normalize() error {
	if in.Kind == "" {
		in.Kind = KindClaude
	}
	if in.Kind != KindClaude && in.Kind != KindCodex {
		return &InputError{"账号类型只能是 claude 或 codex"}
	}
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
	if in.Devices == nil {
		in.Devices = []int64{}
	}
	if err := normalizeProfile(&in.Plan, &in.AccountType, &in.TeamRole, &in.TeamName, &in.TeamDomain, in.Kind == KindClaude); err != nil {
		return err
	}
	in.MailUser = strings.TrimSpace(in.MailUser)
	if in.Kind != KindCodex || !slices.Contains(in.Devices, in.HolderDevice) {
		in.HolderDevice = 0 // 持有者只对 Codex 有效，且必须是分配到的客户端
	}
	return nil
}

func uniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return &InputError{"这个邮箱已经登记过同类型的账号"}
	}
	return err
}

// CreateClaudeAccount 新建账号；填了凭据时先校验，再和账号一起保存（来源「管理员填写」），凭据无效时不建账号。
func (s *Server) CreateClaudeAccount(ctx context.Context, in ClaudeAccountInput) (ClaudeAccount, error) {
	if in.Kind == "" {
		in.Kind = KindClaude
	}
	var cred *AICredential
	if strings.TrimSpace(in.AccessToken) != "" || strings.TrimSpace(in.RefreshToken) != "" {
		c, err := ParseAICredentialInput(in.Kind, in.AccessToken, in.RefreshToken)
		if err != nil {
			return ClaudeAccount{}, err
		}
		email := strings.ToLower(strings.TrimSpace(in.Email))
		switch e := c.Email(); {
		case email == "":
			in.Email = e
		case e != "" && e != email:
			return ClaudeAccount{}, &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", e, email)}
		}
		cred = &c
	}
	if strings.TrimSpace(in.Email) == "" {
		return ClaudeAccount{}, &InputError{"请填写邮箱（凭据里读不出邮箱）"}
	}
	if err := in.normalize(); err != nil {
		return ClaudeAccount{}, err
	}
	devices, _ := json.Marshal(in.Devices)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO claude_accounts (kind, email, note, devices, sub_expires, holder_device,
		plan, account_type, team_role, team_name, team_domain) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Kind, in.Email, in.Note, string(devices), in.SubExpires, in.HolderDevice,
		in.Plan, in.AccountType, in.TeamRole, in.TeamName, in.TeamDomain)
	if err != nil {
		return ClaudeAccount{}, uniqueErr(err)
	}
	id, _ := res.LastInsertId()
	if in.MailPassword != "" {
		if err := s.setMailbox(ctx, id, in.MailUser, in.MailPassword); err != nil {
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM claude_accounts WHERE id = ?`, id)
			return ClaudeAccount{}, err
		}
	}
	if cred != nil {
		if _, err := s.storeAICredential(ctx, id, *cred, "管理员填写", -1); err != nil {
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM claude_accounts WHERE id = ?`, id)
			return ClaudeAccount{}, err
		}
	}
	s.geminiChanged()
	return s.claudeAccount(ctx, id)
}

// UpdateClaudeAccount 修改邮箱、备注、可用的客户端和订阅到期日。类型不能改。换了邮箱时清掉 Gmail 授权
// （授权属于原来的邮箱）、托管的凭据和额度。
func (s *Server) UpdateClaudeAccount(ctx context.Context, id int64, in ClaudeAccountInput) (ClaudeAccount, error) {
	old, err := s.claudeAccount(ctx, id)
	if err != nil {
		return ClaudeAccount{}, err
	}
	if in.Kind == "" {
		in.Kind = old.Kind
	}
	if in.Kind != old.Kind {
		return ClaudeAccount{}, &InputError{"账号类型不能修改：删除后按新类型重新添加"}
	}
	if err := in.normalize(); err != nil {
		return ClaudeAccount{}, err
	}
	devices, _ := json.Marshal(in.Devices)
	now := time.Now().UTC().Format(time.RFC3339)
	set := `email = ?, note = ?, devices = ?, sub_expires = ?, holder_device = ?, plan = ?, account_type = ?, team_role = ?, team_name = ?, team_domain = ?, updated_at = ?`
	if in.Email != old.Email {
		// Gmail 授权属于原来的邮箱；IMAP 登录名跟着邮箱走（没单独填登录名时），密码要重新填。
		set += `, gmail_token = x''`
		if old.MailUser == "" || in.MailUser == "" {
			set += `, mail_password = x''`
		}
		s.forgetGmailToken(id)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET `+set+` WHERE id = ?`, in.Email, in.Note, string(devices), in.SubExpires, in.HolderDevice,
		in.Plan, in.AccountType, in.TeamRole, in.TeamName, in.TeamDomain, now, id); err != nil {
		return ClaudeAccount{}, uniqueErr(err)
	}
	if in.MailPassword != "" || in.MailUser != old.MailUser {
		if err := s.setMailbox(ctx, id, in.MailUser, in.MailPassword); err != nil {
			return ClaudeAccount{}, err
		}
	}
	if in.Email != old.Email && (old.HasCredential || old.Quota != nil) {
		if err := s.clearAICredential(ctx, id, "已清除"); err != nil {
			return ClaudeAccount{}, err
		}
	}
	s.geminiChanged() // 分配或凭据变了，唤醒等待同步的客户端
	return s.claudeAccount(ctx, id)
}

func (s *Server) DeleteClaudeAccount(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM claude_accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.forgetGmailToken(id)
	s.geminiChanged()
	return nil
}

// TestClaudeAccount 用保存的授权读取这个 Gmail 的资料，检查授权是否有效。
func (s *Server) TestClaudeAccount(ctx context.Context, id int64) error {
	a, err := s.claudeAccount(ctx, id)
	if err != nil {
		return err
	}
	if l, ok, err := s.mailboxLogin(ctx, id); err != nil || ok {
		if err != nil {
			return err
		}
		return testMailbox(ctx, l)
	}
	if !a.GmailAuthorized {
		return &InputError{"这个账号还没有设置邮箱密码，也没有授权 Gmail"}
	}
	token, err := s.gmailAccessToken(ctx, id)
	if err != nil {
		return err
	}
	return s.checkGmailProfile(ctx, token, a.Email)
}

// allowed 表示这台客户端能用这个账号：必须在名单里，名单为空表示没有客户端能用（ADR-060），
// 新加入的客户端要在管理服务「AI 账号」里勾选后才有权限。
func allowed(a ClaudeAccount, deviceID int64) bool {
	return slices.Contains(a.Devices, deviceID)
}

// ClientClaudeAccounts 返回允许这台客户端使用、并且已授权 Gmail 的账号，只含 kinds 里的类型
// （为空时只有 claude：旧版客户端不认识 Codex 账号）。
func (s *Server) ClientClaudeAccounts(ctx context.Context, deviceID int64, kinds ...string) ([]ClientClaudeAccount, error) {
	if len(kinds) == 0 {
		kinds = []string{KindClaude}
	}
	all, err := s.ClaudeAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := []ClientClaudeAccount{}
	for _, a := range all {
		if a.MailSource != "" && allowed(a, deviceID) && slices.Contains(kinds, a.Kind) {
			out = append(out, ClientClaudeAccount{ID: a.ID, Kind: a.Kind, Email: a.Email, Note: a.Note})
		}
	}
	return out, nil
}

// ---- 登录：等待 magic link（BXC-F1-084）----
//
// 链接等同于登录凭据：只放在内存里，只交给发起登录的那台客户端一次，不入库、不写日志。

// 登录状态。
const (
	LoginWaiting   = "waiting"   // 等登录邮件
	LoginReady     = "ready"     // 已取到链接，等客户端取走
	LoginDelivered = "delivered" // 链接已交给客户端
	LoginDone      = "done"      // 客户端回报登录成功
	LoginFailed    = "failed"
	LoginExpired   = "expired" // 超时没收到邮件
)

// ClaudeLogin 是一次登录的状态。Link 只在交付的那次响应里出现。
type ClaudeLogin struct {
	ID        string `json:"id"`
	AccountID int64  `json:"account_id"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Link      string `json:"link,omitempty"`
	StartedAt string `json:"started_at"`
}

type claudeLogin struct {
	ClaudeLogin
	deviceID int64
	started  time.Time
	ended    time.Time
	link     string
	ready    chan struct{} // 取到链接或结束时关闭
	cancel   context.CancelFunc
}

// MailFetcher 在 since 之后到达的登录邮件里找 magic link，没有时返回空串。
// 授权失效返回 *MailAuthError（不再重试）。测试时替换。
type MailFetcher func(ctx context.Context, accountID int64, email string, since time.Time) (string, error)

// MailAuthError 是 Gmail 授权无效或被撤销，重试没有意义。
type MailAuthError struct{ Msg string }

func (e *MailAuthError) Error() string { return e.Msg }

func (s *Server) mailFetcher() MailFetcher {
	if s.FetchMail != nil {
		return s.FetchMail
	}
	return s.fetchLoginLink
}

// fetchLoginLink 按账号的收信方式取登录链接：设置了邮箱密码时用 IMAP，否则用 Gmail API。
func (s *Server) fetchLoginLink(ctx context.Context, accountID int64, email string, since time.Time) (string, error) {
	l, ok, err := s.mailboxLogin(ctx, accountID)
	if err != nil {
		return "", err
	}
	if !ok {
		return s.fetchGmailLink(ctx, accountID, email, since)
	}
	raws, err := s.listImapRaw(ctx, l, claudeMailDomain, since)
	if err != nil {
		return "", err
	}
	for _, r := range raws { // 新的在前
		if link := linkFromMessage(bytes.NewReader(r.Raw)); link != "" {
			return link, nil
		}
	}
	return "", nil
}

// 管理员给账号做的标记（只用于显示和筛选）。
const (
	AccountPersonal = "personal"
	AccountTeam     = "team"
	TeamOwner       = "owner"
	TeamMember      = "member"
)

var domainRE = regexp.MustCompile(`^[a-z0-9.\-]+\.[a-z]{2,}$`)

// normalizeProfile 校验套餐和 Team 标记。team 为 false 时（Codex、Gemini）只保留套餐。
func normalizeProfile(plan, accountType, role, name, domain *string, team bool) error {
	*plan = strings.TrimSpace(*plan)
	if len([]rune(*plan)) > 40 {
		return &InputError{"套餐最多 40 字"}
	}
	*accountType = strings.TrimSpace(*accountType)
	if !team || *accountType == "" {
		*accountType = AccountPersonal
	}
	switch *accountType {
	case AccountPersonal:
		*role, *name, *domain = "", "", ""
		return nil
	case AccountTeam:
	default:
		return &InputError{"账号类型只能是个人号（personal）或 Team 号（team）"}
	}
	if *role != TeamOwner && *role != TeamMember {
		return &InputError{"Team 号请选择主号还是成员号"}
	}
	*name = strings.TrimSpace(*name)
	if *name == "" || len([]rune(*name)) > 60 {
		return &InputError{"请填写 Team 名称（最多 60 字）"}
	}
	*domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(*domain)), "@")
	if *domain != "" && !domainRE.MatchString(*domain) {
		return &InputError{"邮箱域名格式不对，如 example.com"}
	}
	return nil
}

func newLoginID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartClaudeLogin 开始等这个账号的登录邮件。同一账号已有进行中的登录时取消旧的。
func (s *Server) StartClaudeLogin(ctx context.Context, deviceID int64, accountID int64) (ClaudeLogin, error) {
	a, err := s.claudeAccount(ctx, accountID)
	if err != nil {
		return ClaudeLogin{}, err
	}
	if !allowed(a, deviceID) {
		return ClaudeLogin{}, ErrNotFound
	}
	if a.Kind != KindClaude {
		return ClaudeLogin{}, &InputError{"一键登录只支持 Claude 账号；Codex 登录时在「邮件」里看验证码"}
	}
	if a.MailSource == "" {
		return ClaudeLogin{}, &InputError{"这个账号还没有在管理服务上设置收信（邮箱密码或 Gmail 授权）"}
	}
	now := time.Now()
	wctx, cancel := context.WithTimeout(context.Background(), loginTTL)
	l := &claudeLogin{
		ClaudeLogin: ClaudeLogin{ID: newLoginID(), AccountID: a.ID, Email: a.Email, Status: LoginWaiting,
			StartedAt: now.UTC().Format(time.RFC3339)},
		deviceID: deviceID, started: now, ready: make(chan struct{}), cancel: cancel,
	}

	s.loginMu.Lock()
	if s.logins == nil {
		s.logins = map[string]*claudeLogin{}
	}
	for id, old := range s.logins {
		if !old.ended.IsZero() && now.Sub(old.ended) > loginKeep {
			delete(s.logins, id)
			continue
		}
		if old.AccountID == accountID && old.ended.IsZero() {
			s.endLocked(old, LoginFailed, "已被新的登录取代")
		}
	}
	s.logins[l.ID] = l
	out := l.ClaudeLogin // 后台任务启动后会在锁内改 l，返回值要在这之前复制
	s.loginMu.Unlock()

	go s.watchMail(wctx, l)
	return out, nil
}

// endLocked 结束登录。调用方持有 loginMu。
func (s *Server) endLocked(l *claudeLogin, status, msg string) {
	if !l.ended.IsZero() {
		return
	}
	l.Status, l.Error, l.ended, l.link = status, msg, time.Now(), ""
	l.cancel()
	select {
	case <-l.ready:
	default:
		close(l.ready)
	}
}

func (s *Server) watchMail(ctx context.Context, l *claudeLogin) {
	since := l.started.Add(-mailClockSkew)
	fetch := s.mailFetcher()
	tick := time.NewTicker(mailPollEvery)
	defer tick.Stop()
	for {
		link, err := fetch(ctx, l.AccountID, l.Email, since)
		s.loginMu.Lock()
		if !l.ended.IsZero() {
			s.loginMu.Unlock()
			return
		}
		var auth *MailAuthError
		switch {
		case link != "":
			l.link, l.Status, l.Error = link, LoginReady, ""
			close(l.ready)
			s.loginMu.Unlock()
			return
		case errors.As(err, &auth):
			s.endLocked(l, LoginFailed, auth.Error())
			s.loginMu.Unlock()
			return
		case err != nil && ctx.Err() == nil:
			l.Error = "读取登录邮件失败，正在重试：" + err.Error() // 网络抖动，继续重试
		}
		s.loginMu.Unlock()
		select {
		case <-ctx.Done():
			s.loginMu.Lock()
			s.endLocked(l, LoginExpired, "10 分钟内没有收到登录邮件")
			s.loginMu.Unlock()
			return
		case <-tick.C:
		}
	}
}

func (s *Server) deviceLogin(deviceID int64, id string) (*claudeLogin, error) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	l, ok := s.logins[id]
	if !ok || l.deviceID != deviceID {
		return nil, ErrNotFound
	}
	return l, nil
}

// WaitClaudeLink 等链接（最多 wait）。链接只交付一次，之后的查询只返回状态。
func (s *Server) WaitClaudeLink(ctx context.Context, deviceID int64, id string, wait time.Duration) (ClaudeLogin, error) {
	l, err := s.deviceLogin(deviceID, id)
	if err != nil {
		return ClaudeLogin{}, err
	}
	if wait > maxLinkWait {
		wait = maxLinkWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-l.ready:
	case <-timer.C:
	case <-ctx.Done():
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	out := l.ClaudeLogin
	if l.Status == LoginReady && l.link != "" {
		out.Link = l.link
		l.link, l.Status = "", LoginDelivered
		out.Status = LoginDelivered
	}
	return out, nil
}

// FinishClaudeLogin 记录客户端回报的结果。ok 时核对 Claude Code 登录的邮箱。
func (s *Server) FinishClaudeLogin(ctx context.Context, d Device, id string, ok bool, email, msg string) (ClaudeLogin, error) {
	l, err := s.deviceLogin(d.ID, id)
	if err != nil {
		return ClaudeLogin{}, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if ok && email != l.Email {
		ok, msg = false, fmt.Sprintf("Claude Code 登录的是 %s，不是 %s", email, l.Email)
	}
	msg = strings.TrimSpace(msg)
	if len([]rune(msg)) > 300 {
		msg = string([]rune(msg)[:300])
	}
	s.loginMu.Lock()
	if ok {
		s.endLocked(l, LoginDone, "")
	} else {
		if msg == "" {
			msg = "登录没有完成"
		}
		s.endLocked(l, LoginFailed, msg)
	}
	out := l.ClaudeLogin
	s.loginMu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	if ok {
		_, err = s.DB.ExecContext(ctx, `UPDATE claude_accounts SET last_login_at = ?, last_login_device = ?, last_error = '' WHERE id = ?`,
			now, d.Name, l.AccountID)
	} else {
		_, err = s.DB.ExecContext(ctx, `UPDATE claude_accounts SET last_error = ? WHERE id = ?`, msg, l.AccountID)
	}
	return out, err
}

// ---- 解析登录邮件 ----

// linkFromMessage 解析一封邮件（RFC 822 原文），发件人须是 mail.anthropic.com，返回正文里的 magic link。
func linkFromMessage(r io.Reader) string {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return ""
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || !strings.HasSuffix(strings.ToLower(from.Address), "@"+claudeMailDomain) {
		return ""
	}
	return linkFromPart(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
}

func linkFromPart(contentType, encoding string, body io.Reader, depth int) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if depth > 5 || params["boundary"] == "" {
			return ""
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return ""
			}
			if l := linkFromPart(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1); l != "" {
				return l
			}
		}
	}
	if !strings.HasPrefix(mediaType, "text/") {
		return ""
	}
	var rd io.Reader = body
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		rd = quotedprintable.NewReader(body) // 解码后折行和 =3D 不会打断链接
	case "base64":
		rd = base64.NewDecoder(base64.StdEncoding, newlineStripper{body})
	}
	b, err := io.ReadAll(io.LimitReader(rd, maxMailMessage))
	if err != nil && len(b) == 0 {
		return ""
	}
	b = bytes.ReplaceAll(b, []byte("&amp;"), []byte("&"))
	return string(magicLinkRE.Find(b))
}

// newlineStripper 去掉 base64 正文里的换行。
type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	for {
		k, err := n.r.Read(p)
		j := 0
		for _, c := range p[:k] {
			if c != '\r' && c != '\n' {
				p[j] = c
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}
