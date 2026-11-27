package dist

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"boxctl/internal/gen"
	"boxctl/internal/groups"
)

// 和管理服务之间的请求、响应结构（从服务端代码里拆出来的客户端用到的部分）。
// AICredentialUpload 是客户端上传的凭据。Replace 用于本机重新登录：总是取代旧的，不比较版本号。
type AICredentialUpload struct {
	BaseVersion int64        `json:"base_version"`
	Replace     bool         `json:"replace"`
	Source      string       `json:"source"` // login / refresh
	Credential  AICredential `json:"credential"`
}

// AIOnlineAccount 是客户端上报的一个在用账号。
type AIOnlineAccount struct {
	Kind      string `json:"kind"` // claude / codex / gemini
	AccountID int64  `json:"account_id"`
}

// AIQuotaSample 是客户端上报的一个账号的额度。
type AIQuotaSample struct {
	AccountID int64   `json:"account_id"`
	Quota     AIQuota `json:"quota"`
}

// AIRefreshResult 是客户端交回的续期结果。Permanent 表示 Refresh Token 被拒绝（失效或已被使用），重试没有意义。
type AIRefreshResult struct {
	AccountID   int64         `json:"account_id"`
	BaseVersion int64         `json:"base_version"`
	OK          bool          `json:"ok"`
	Error       string        `json:"error,omitempty"`
	Permanent   bool          `json:"permanent,omitempty"`
	Credential  *AICredential `json:"credential,omitempty"`
}

// AISync 是一次同步的结果：账号与凭据，以及交给这台客户端执行的鉴权测试。
type AISync struct {
	Hash     string          `json:"hash"`
	Accounts []AISyncAccount `json:"accounts"`
	Tests    []AuthTest      `json:"tests"`
	Jobs     []AIRefreshJob  `json:"jobs"` // 交给这台客户端执行的续期（ADR-071）
}

// GeminiOAuth 是下发给客户端的 OAuth 客户端。
type GeminiOAuth struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// GeminiSync 是一次同步的结果。Order 是建议的使用顺序（BXC-F0-023），只含有凭据的账号。
type GeminiSync struct {
	Hash     string              `json:"hash"`
	Accounts []GeminiSyncAccount `json:"accounts"`
	Order    []int64             `json:"order"`
	Switch   *GeminiSwitch       `json:"switch,omitempty"`
	// OAuth 是登录和刷新访问令牌用的 Antigravity OAuth 客户端；管理员还没填密钥时为空。
	OAuth *GeminiOAuth `json:"oauth,omitempty"`
	// Tests 是交给这台客户端执行的鉴权测试（ADR-061）。
	Tests []AuthTest `json:"tests,omitempty"`
}

// RuleVariant 是发布进版本里的方案（已按方案筛好节点和代理组，规则已去掉分段标记）。
type RuleVariant struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Version    string            `json:"version,omitempty"`
	Devices    []int64           `json:"devices"`
	Nodes      []map[string]any  `json:"nodes,omitempty"`
	Groups     []groups.Export   `json:"groups,omitempty"`
	Selections map[string]string `json:"selections,omitempty"`
	Rules      []string          `json:"rules"`
	RuleSets   []gen.RuleSet     `json:"rule_sets"`
	// Scoped 为 false 的旧版本快照没有节点、代理组和线路选择，沿用默认的。
	Scoped bool `json:"scoped,omitempty"`
}

// AICredential 是统一格式的 OAuth 凭据。
type AICredential struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 访问令牌过期时间，Unix 毫秒；0 表示不知道
	IDToken      string `json:"id_token,omitempty"`   // Codex
	AccountID    string `json:"account_id,omitempty"` // Codex：ChatGPT-Account-Id
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

// AIQuota 是一个账号的额度。
type AIQuota struct {
	Windows   []AIQuotaWindow `json:"windows"`
	Plan      string          `json:"plan,omitempty"`
	Error     string          `json:"error,omitempty"`
	CheckedAt time.Time       `json:"checked_at"`
}

// AIRefreshJob 是交给一台客户端执行的续期：用 Credential 里的 Refresh Token 换新令牌，交回结果。
type AIRefreshJob struct {
	ID          string       `json:"id"`
	AccountID   int64        `json:"account_id"`
	Kind        string       `json:"kind"`
	BaseVersion int64        `json:"base_version"`
	Credential  AICredential `json:"credential"`
}

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

// AuthTestResult 是客户端回报的测试结果。
type AuthTestResult struct {
	OK    bool     `json:"ok"`
	Error string   `json:"error"`
	Quota *AIQuota `json:"quota,omitempty"`
}

// GeminiSwitch 是管理页面下发的切换指令，客户端执行后回报 ID。
type GeminiSwitch struct {
	ID        string `json:"id"`
	AccountID int64  `json:"account_id"`
}

// GeminiSyncAccount 是分配给这台客户端的账号；Credential 为空表示还没有人登录。
type GeminiSyncAccount struct {
	ID         int64           `json:"id"`
	Email      string          `json:"email"`
	Note       string          `json:"note"`
	Version    int64           `json:"version"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

// AIQuotaWindow 是额度的一个窗口（Claude 的 5 小时 / 每周，Codex 的 primary / secondary）。
type AIQuotaWindow struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetAt     string  `json:"reset_at,omitempty"`
}

// AIRefreshNoticeView 是下发给客户端的续期状态（不含任务内容）。
type AIRefreshNoticeView struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Deadline string `json:"deadline"`
}

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

// ClientClaudeAccount 是下发给客户端的账号，只有类型、邮箱和备注。
type ClientClaudeAccount struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Email string `json:"email"`
	Note  string `json:"note"`
}

// GeminiCredentialInfo 是从凭据 JSON 里读出的、核对用的字段。
type GeminiCredentialInfo struct {
	RefreshToken string
	AccessToken  string
	Expiry       time.Time
	Email        string // 来自 id_token，没有 id_token 时为空
}

// GeminiStateReport 是客户端回报的当前账号；SwitchDone 是刚执行完的切换指令。
type GeminiStateReport struct {
	ActiveAccountID int64  `json:"active_account_id"`
	SwitchDone      string `json:"switch_done"`
}

// InputError 是用户输入错误，API 返回 400。
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

// UpdateInfo 是更新检查的结果。客户端服务把 ReleaseID 换成本机下载地址交给 Tauri updater。
type UpdateInfo struct {
	Version string `json:"version"`
	// Mode 是更新方式：normal / push / silent / force（客户端低于最低版本时）；旧版管理服务没有，按 normal。
	Mode       string `json:"mode,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
	Notes      string `json:"notes"`
	PubDate    string `json:"pub_date"`
	ReleaseID  int64  `json:"release_id"`
	Signature  string `json:"signature"`
	Size       int64  `json:"size"`
}

// ErrCredentialConflict 是上传凭据时版本号不是最新的。
var ErrCredentialConflict = errors.New("凭据已在别处更新过")

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// 登录状态。
const (
	LoginWaiting   = "waiting"   // 等登录邮件
	LoginReady     = "ready"     // 已取到链接，等客户端取走
	LoginDelivered = "delivered" // 链接已交给客户端
	LoginDone      = "done"      // 客户端回报登录成功
	LoginFailed    = "failed"
	LoginExpired   = "expired" // 超时没收到邮件
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 客户端随请求报告自己的版本和平台。
const (
	headerAppVersion = "X-Banbu-Version"
	headerPlatform   = "X-Banbu-Platform"
	// 设备名和系统版本可能含中文，客户端做 URL 编码（url.QueryEscape）后放进请求头。
	headerHostname  = "X-Banbu-Hostname"
	headerOSVersion = "X-Banbu-OS"
)

// 账号类型（BXC-F0-045，ADR-053）：两种都由管理服务从账号自己的 Gmail 读登录邮件，
// Claude 取 magic link 并支持一键登录，Codex 取 OpenAI 发来的 6 位验证码。
const (
	KindClaude = "claude"
	KindCodex  = "codex"
	// KindGemini 是 Gemini 账号（gemini_accounts，单独一张表），只用于在线时长等跨类型的统计。
	KindGemini = "gemini"
)

// magicLinkRE 匹配登录邮件里的链接，形如 https://claude.ai/magic-link#<十六进制>（实测）。
var magicLinkRE = regexp.MustCompile(`https://claude\.ai/magic-link#[^\s"'<>()（）]+`)

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

func newLoginID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CredentialUpload 是客户端上传的凭据。Replace 用于本机重新登录：新登录的凭据总是取代旧的，不比较版本号。
type CredentialUpload struct {
	BaseVersion int64           `json:"base_version"`
	Replace     bool            `json:"replace"`
	Source      string          `json:"source"` // login / refresh
	Credential  json.RawMessage `json:"credential"`
}

const (
	// GeminiExhausted 是剩余比例低于它就当作耗尽的阈值。
	GeminiExhausted  = 0.05
	geminiFreshness  = time.Hour
	geminiBurnWindow = 2 * time.Hour
	geminiSoon       = 30 * time.Minute
)

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

const maxAICredential = 32 << 10

const (
	maxGeminiCredential = 16 << 10
	maxGeminiDevices    = 20
)

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

// 续期阶段。
const (
	RefreshNotice  = "notice"  // 通知客户端，等宽限期
	RefreshRunning = "running" // 已交给一台客户端执行
	RefreshWaiting = "waiting" // 没有在线的客户端可以执行，稍后再试
	RefreshFailed  = "failed"  // 失败；Permanent 时 Refresh Token 已失效，等重新登录或填写
)
