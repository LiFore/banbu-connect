// Package subscriptions 负责订阅的持久化、更新预览与应用（M2-01/02/04/10）。
// 纯逻辑（解析、清洗、ID）在 internal/sub。
package subscriptions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"boxctl/internal/refs"
	"boxctl/internal/secret"
	"boxctl/internal/sub"
)

const (
	FetchDirect = "direct"
	FetchProxy  = "proxy"

	previewTTL = 10 * time.Minute
	timeLayout = time.RFC3339
)

var (
	ErrNotFound       = errors.New("订阅不存在")
	ErrNameExists     = errors.New("订阅名称已存在")
	ErrPreviewExpired = errors.New("预览已过期，请重新获取")
)

// ValidationError 表示输入不合法，API 返回 400。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// ReferencesError 表示操作会删除仍被引用的节点，需要用户确认（M2-10）。
type ReferencesError = refs.Error

type Subscription struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name"`
	URL         string        `json:"url,omitempty"` // 仅 reveal 时返回明文
	HasURL      bool          `json:"has_url"`
	UserAgent   string        `json:"user_agent"`
	FetchVia    string        `json:"fetch_via"`
	Enabled     bool          `json:"enabled"`
	Options     sub.Options   `json:"options"`
	UserInfo    *sub.UserInfo `json:"userinfo"`
	NodeCount   int           `json:"node_count"`
	LastFetchAt string        `json:"last_fetch_at"`
	LastError   string        `json:"last_error"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
}

// Input 是创建/修改的请求体。修改时 URL 为空表示保留原值。
type Input struct {
	Name        string       `json:"name"`
	DisplayName string       `json:"display_name"`
	URL         string       `json:"url"`
	UserAgent   string       `json:"user_agent"`
	FetchVia    string       `json:"fetch_via"`
	Enabled     *bool        `json:"enabled"`
	Options     *sub.Options `json:"options"`
}

type Fetcher func(context.Context, sub.FetchRequest) (sub.FetchResult, error)

type Service struct {
	db      *sql.DB
	secrets *secret.Box
	// FetchProxy 是 fetch_via=proxy 时使用的本地代理（Mihomo mixed-port）。
	FetchProxy string
	Fetch      Fetcher
	Validate   sub.Validator
	// OnAutoUpdate 在每次自动更新一个订阅后调用（成功时 err 为空，diff 是节点变化）。管理服务用来自动发布和推送通知。
	OnAutoUpdate func(sb Subscription, diff Diff, err error)
	now          func() time.Time

	mu       sync.Mutex
	previews map[string]*preview // key: preview id
}

func New(db *sql.DB, secrets *secret.Box, fetchProxy string) *Service {
	return &Service{
		db: db, secrets: secrets, FetchProxy: fetchProxy,
		Fetch: sub.Fetch, Validate: sub.ValidateWithMihomo,
		now: time.Now, previews: map[string]*preview{},
	}
}

var nameRe = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,32}$`)

func (s *Service) validate(in *Input, creating bool) error {
	in.Name = strings.TrimSpace(in.Name)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.URL = strings.TrimSpace(in.URL)
	if !nameRe.MatchString(in.Name) {
		return &ValidationError{"名称须为 1–32 位字母、数字、中文、下划线或连字符（用于显示名模板的 {sub}）"}
	}
	if len([]rune(in.DisplayName)) > 64 {
		return &ValidationError{"显示名称最长 64 字"}
	}
	if in.URL != "" || creating {
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return &ValidationError{"订阅地址须为 http(s) 链接"}
		}
	}
	if in.UserAgent == "" {
		in.UserAgent = sub.DefaultUserAgent
	}
	if in.FetchVia == "" {
		in.FetchVia = FetchDirect
	}
	if in.FetchVia != FetchDirect && in.FetchVia != FetchProxy {
		return &ValidationError{"拉取方式须为 direct 或 proxy"}
	}
	if in.Options == nil {
		def := sub.DefaultOptions()
		in.Options = &def
	}
	if err := in.Options.Validate(); err != nil {
		return &ValidationError{err.Error()}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, in Input) (Subscription, error) {
	if err := s.validate(&in, true); err != nil {
		return Subscription{}, err
	}
	id := "s_" + randomHex(8)
	urlCT, err := s.secrets.Seal([]byte(in.URL), urlAAD(id))
	if err != nil {
		return Subscription{}, err
	}
	opts, _ := json.Marshal(in.Options)
	enabled := in.Enabled == nil || *in.Enabled
	now := s.stamp()
	_, err = s.db.ExecContext(ctx, `INSERT INTO subscriptions
		(id, name, display_name, url_secret, user_agent, fetch_via, enabled, options, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.DisplayName, urlCT, in.UserAgent, in.FetchVia, enabled, string(opts), now, now)
	if err != nil {
		return Subscription{}, mapUnique(err)
	}
	return s.Get(ctx, id, false)
}

func (s *Service) Update(ctx context.Context, id string, in Input) (Subscription, error) {
	if err := s.validate(&in, false); err != nil {
		return Subscription{}, err
	}
	cur, err := s.Get(ctx, id, false)
	if err != nil {
		return Subscription{}, err
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	opts, _ := json.Marshal(in.Options)
	q := `UPDATE subscriptions SET name = ?, display_name = ?, user_agent = ?, fetch_via = ?,
		enabled = ?, options = ?, updated_at = ?`
	args := []any{in.Name, in.DisplayName, in.UserAgent, in.FetchVia, enabled, string(opts), s.stamp()}
	if in.URL != "" {
		ct, err := s.secrets.Seal([]byte(in.URL), urlAAD(id))
		if err != nil {
			return Subscription{}, err
		}
		q += `, url_secret = ?`
		args = append(args, ct)
	}
	if _, err := s.db.ExecContext(ctx, q+` WHERE id = ?`, append(args, id)...); err != nil {
		return Subscription{}, mapUnique(err)
	}
	return s.Get(ctx, id, false)
}

const selectSub = `SELECT s.id, s.name, s.display_name, s.url_secret, s.user_agent, s.fetch_via, s.enabled,
	s.options, s.userinfo, COALESCE(s.last_fetch_at, ''), s.last_error, s.created_at, s.updated_at,
	(SELECT COUNT(*) FROM proxy_nodes n WHERE n.subscription_id = s.id)
	FROM subscriptions s`

func (s *Service) List(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, selectSub+` ORDER BY s.created_at, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		sb, _, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// Get 读取订阅；reveal 为 true 时返回明文 URL（M1-08：默认脱敏）。
func (s *Service) Get(ctx context.Context, id string, reveal bool) (Subscription, error) {
	sb, urlCT, err := s.scan(s.db.QueryRowContext(ctx, selectSub+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil || !reveal || len(urlCT) == 0 {
		return sb, err
	}
	plain, err := s.secrets.Open(urlCT, urlAAD(id))
	if err != nil {
		return Subscription{}, err
	}
	sb.URL = string(plain)
	return sb, nil
}

type scanner interface{ Scan(...any) error }

func (s *Service) scan(row scanner) (Subscription, []byte, error) {
	var sb Subscription
	var urlCT []byte
	var opts, ui string
	err := row.Scan(&sb.ID, &sb.Name, &sb.DisplayName, &urlCT, &sb.UserAgent, &sb.FetchVia, &sb.Enabled,
		&opts, &ui, &sb.LastFetchAt, &sb.LastError, &sb.CreatedAt, &sb.UpdatedAt, &sb.NodeCount)
	if err != nil {
		return Subscription{}, nil, err
	}
	sb.HasURL = len(urlCT) > 0
	sb.Options = sub.DefaultOptions()
	if err := json.Unmarshal([]byte(opts), &sb.Options); err != nil {
		return Subscription{}, nil, fmt.Errorf("订阅 %s 的 options 损坏：%w", sb.ID, err)
	}
	var info sub.UserInfo
	if json.Unmarshal([]byte(ui), &info) == nil && info != (sub.UserInfo{}) {
		sb.UserInfo = &info
	}
	return sb, urlCT, nil
}

// Delete 删除订阅及其节点。节点仍被引用且未确认时返回 *ReferencesError。
func (s *Service) Delete(ctx context.Context, id string, confirm bool) error {
	if _, err := s.Get(ctx, id, false); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name FROM proxy_nodes WHERE subscription_id = ?`, id)
	if err != nil {
		return err
	}
	removed := map[string]string{}
	for rows.Next() {
		var nid, name string
		if err := rows.Scan(&nid, &name); err != nil {
			rows.Close()
			return err
		}
		removed[nid] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	impacts, err := refs.Find(ctx, s.db, removed)
	if err != nil {
		return err
	}
	if len(impacts) > 0 && !confirm {
		return &ReferencesError{Impacts: impacts}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

func (s *Service) stamp() string { return s.now().UTC().Format(timeLayout) }

func urlAAD(id string) string { return "subscriptions.url:" + id }

func mapUnique(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: subscriptions.name") {
		return ErrNameExists
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败意味着系统熵源不可用
	}
	return hex.EncodeToString(b)
}
