package dist

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- Gmail API（ADR-025）----
//
// 管理员在 Google Cloud 里建「桌面应用」类型的 OAuth 客户端，把 client_id/secret 填到管理服务（加密保存）。
// 每个 Claude 账号授权一次只读权限（gmail.readonly）：服务端生成带 PKCE 的授权链接，回跳地址是本机回环地址，
// 浏览器会停在一个打不开的 http://127.0.0.1 页面，管理员把这个地址粘贴回来，服务端换取 refresh token。
// 这样管理服务没有域名、用自签名证书时也能授权（「Web 应用」类型的回跳地址要求域名和有效证书）。

const (
	gmailClientKey = "gmail_oauth_client"
	gmailTokenAAD  = "claude_accounts.gmail_token:"
	gmailScope     = "https://www.googleapis.com/auth/gmail.readonly"
	// gmailRedirect 是桌面应用客户端的回环回跳地址：Google 接受任意端口，这里不需要真的有服务监听。
	gmailRedirect = "http://127.0.0.1:53682/"
	gmailAuthTTL  = 15 * time.Minute
)

// GoogleAPI 是 Google 的接口地址，测试时替换为本地服务。
type GoogleAPI struct {
	AuthURL  string
	TokenURL string
	GmailURL string // users/me 之前的部分
	HTTP     *http.Client
}

func (s *Server) google() GoogleAPI {
	g := s.Google
	if g.AuthURL == "" {
		g.AuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	if g.TokenURL == "" {
		g.TokenURL = "https://oauth2.googleapis.com/token"
	}
	if g.GmailURL == "" {
		g.GmailURL = "https://gmail.googleapis.com/gmail/v1"
	}
	if g.HTTP == nil {
		g.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	return g
}

// ---- OAuth 客户端设置 ----

type gmailClient struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// GmailClientInfo 是页面看到的 OAuth 客户端设置；secret 只返回有没有。
type GmailClientInfo struct {
	ClientID    string `json:"client_id"`
	HasSecret   bool   `json:"has_secret"`
	RedirectURI string `json:"redirect_uri"`
}

func (s *Server) loadGmailClient(ctx context.Context) (gmailClient, error) {
	var stored struct {
		Sealed []byte `json:"sealed"`
	}
	if err := s.loadSetting(ctx, gmailClientKey, &stored); err != nil || len(stored.Sealed) == 0 {
		return gmailClient{}, err
	}
	b, err := s.Secrets.Open(stored.Sealed, gmailClientKey)
	if err != nil {
		return gmailClient{}, fmt.Errorf("解密 Google OAuth 客户端失败：%w", err)
	}
	var c gmailClient
	return c, json.Unmarshal(b, &c)
}

func (s *Server) GmailClient(ctx context.Context) (GmailClientInfo, error) {
	c, err := s.loadGmailClient(ctx)
	return GmailClientInfo{ClientID: c.ClientID, HasSecret: c.ClientSecret != "", RedirectURI: gmailRedirect}, err
}

// SetGmailClient 保存 OAuth 客户端。secret 为空表示不变。
func (s *Server) SetGmailClient(ctx context.Context, clientID, secret string) (GmailClientInfo, error) {
	clientID, secret = strings.TrimSpace(clientID), strings.TrimSpace(secret)
	if !strings.HasSuffix(clientID, ".apps.googleusercontent.com") {
		return GmailClientInfo{}, &InputError{"客户端 ID 应以 .apps.googleusercontent.com 结尾"}
	}
	old, err := s.loadGmailClient(ctx)
	if err != nil {
		return GmailClientInfo{}, err
	}
	if secret == "" {
		secret = old.ClientSecret
	}
	if secret == "" {
		return GmailClientInfo{}, &InputError{"请填写客户端密钥"}
	}
	b, _ := json.Marshal(gmailClient{ClientID: clientID, ClientSecret: secret})
	sealed, err := s.Secrets.Seal(b, gmailClientKey)
	if err != nil {
		return GmailClientInfo{}, err
	}
	if err := s.saveSetting(ctx, gmailClientKey, map[string][]byte{"sealed": sealed}); err != nil {
		return GmailClientInfo{}, err
	}
	// 换了客户端，已缓存的访问令牌作废（refresh token 属于原客户端，需要重新授权）。
	s.gmailMu.Lock()
	s.gmailTokens = nil
	s.gmailMu.Unlock()
	return s.GmailClient(ctx)
}

// ---- 授权 ----

type gmailPending struct {
	verifier, state string
	created         time.Time
}

func randomB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// StartGmailAuth 返回这个账号的授权链接（带 PKCE，login_hint 预填邮箱）。
func (s *Server) StartGmailAuth(ctx context.Context, accountID int64) (string, error) {
	a, err := s.claudeAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	c, err := s.loadGmailClient(ctx)
	if err != nil {
		return "", err
	}
	if c.ClientID == "" {
		return "", &InputError{"请先填写 Google OAuth 客户端"}
	}
	p := &gmailPending{verifier: randomB64(48), state: randomB64(16), created: time.Now()}
	sum := sha256.Sum256([]byte(p.verifier))
	s.gmailMu.Lock()
	if s.gmailPending == nil {
		s.gmailPending = map[int64]*gmailPending{}
	}
	s.gmailPending[accountID] = p
	s.gmailMu.Unlock()

	q := url.Values{
		"client_id":             {c.ClientID},
		"redirect_uri":          {gmailRedirect},
		"response_type":         {"code"},
		"scope":                 {gmailScope},
		"access_type":           {"offline"},
		"prompt":                {"consent"}, // 每次都返回 refresh token
		"login_hint":            {a.Email},
		"state":                 {p.state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	return s.google().AuthURL + "?" + q.Encode(), nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (s *Server) postToken(ctx context.Context, form url.Values) (tokenResponse, int, error) {
	g := s.google()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return tokenResponse{}, 0, fmt.Errorf("连接 Google：%w", err)
	}
	defer resp.Body.Close()
	var t tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return t, resp.StatusCode, fmt.Errorf("Google 返回无效内容（HTTP %d）", resp.StatusCode)
	}
	return t, resp.StatusCode, nil
}

// codeFromPasted 从粘贴的回跳地址（或单独的授权码）里取 code 和 state。
func codeFromPasted(pasted string) (code, state string) {
	pasted = strings.TrimSpace(pasted)
	if i := strings.IndexByte(pasted, '?'); i >= 0 {
		pasted = pasted[i+1:]
	}
	if q, err := url.ParseQuery(pasted); err == nil && q.Get("code") != "" {
		return q.Get("code"), q.Get("state")
	}
	return pasted, ""
}

// FinishGmailAuth 用粘贴回来的回跳地址换取 refresh token，核对授权的 Gmail 与账号邮箱一致后加密保存。
func (s *Server) FinishGmailAuth(ctx context.Context, accountID int64, pasted string) (ClaudeAccount, error) {
	a, err := s.claudeAccount(ctx, accountID)
	if err != nil {
		return ClaudeAccount{}, err
	}
	c, err := s.loadGmailClient(ctx)
	if err != nil {
		return ClaudeAccount{}, err
	}
	s.gmailMu.Lock()
	p := s.gmailPending[accountID]
	s.gmailMu.Unlock()
	if p == nil || time.Since(p.created) > gmailAuthTTL {
		return ClaudeAccount{}, &InputError{"授权已过期，请重新点「授权 Gmail」"}
	}
	if strings.Contains(pasted, "error=") {
		return ClaudeAccount{}, &InputError{"Google 没有完成授权（可能点了取消）"}
	}
	code, state := codeFromPasted(pasted)
	if code == "" {
		return ClaudeAccount{}, &InputError{"请粘贴浏览器地址栏里的完整地址"}
	}
	if state != "" && state != p.state {
		return ClaudeAccount{}, &InputError{"这个地址不是本次授权的结果，请重新授权"}
	}
	t, status, err := s.postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"redirect_uri":  {gmailRedirect},
		"code_verifier": {p.verifier},
	})
	if err != nil {
		return ClaudeAccount{}, err
	}
	if status != http.StatusOK || t.AccessToken == "" {
		return ClaudeAccount{}, &InputError{fmt.Sprintf("换取授权失败：%s %s", t.Error, t.Description)}
	}
	if t.RefreshToken == "" {
		return ClaudeAccount{}, &InputError{"Google 没有返回 refresh token，请重新授权"}
	}
	if err := s.checkGmailProfile(ctx, t.AccessToken, a.Email); err != nil {
		return ClaudeAccount{}, err
	}
	sealed, err := s.Secrets.Seal([]byte(t.RefreshToken), gmailTokenAAD+strconv.FormatInt(accountID, 10))
	if err != nil {
		return ClaudeAccount{}, err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE claude_accounts SET gmail_token = ?, last_error = '' WHERE id = ?`, sealed, accountID); err != nil {
		return ClaudeAccount{}, err
	}
	s.gmailMu.Lock()
	delete(s.gmailPending, accountID)
	if s.gmailTokens == nil {
		s.gmailTokens = map[int64]cachedToken{}
	}
	s.gmailTokens[accountID] = cachedToken{t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}
	s.gmailMu.Unlock()
	return s.claudeAccount(ctx, accountID)
}

// ---- 访问令牌 ----

type cachedToken struct {
	token   string
	expires time.Time
}

func (s *Server) forgetGmailToken(accountID int64) {
	s.gmailMu.Lock()
	delete(s.gmailTokens, accountID)
	s.gmailMu.Unlock()
}

// gmailAccessToken 返回有效的访问令牌，快过期时用 refresh token 刷新。
func (s *Server) gmailAccessToken(ctx context.Context, accountID int64) (string, error) {
	s.gmailMu.Lock()
	if t, ok := s.gmailTokens[accountID]; ok && time.Until(t.expires) > time.Minute {
		s.gmailMu.Unlock()
		return t.token, nil
	}
	s.gmailMu.Unlock()

	var sealed []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT gmail_token FROM claude_accounts WHERE id = ?`, accountID).Scan(&sealed); err != nil {
		return "", err
	}
	if len(sealed) == 0 {
		return "", &MailAuthError{"这个账号还没有授权 Gmail"}
	}
	refresh, err := s.Secrets.Open(sealed, gmailTokenAAD+strconv.FormatInt(accountID, 10))
	if err != nil {
		return "", fmt.Errorf("解密 Gmail 授权失败：%w", err)
	}
	c, err := s.loadGmailClient(ctx)
	if err != nil {
		return "", err
	}
	t, status, err := s.postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {string(refresh)},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
	})
	if err != nil {
		return "", err
	}
	if t.Error == "invalid_grant" || t.Error == "unauthorized_client" || t.Error == "invalid_client" {
		// 授权被撤销、过期（测试状态的应用 7 天过期），或换了 OAuth 客户端。
		return "", &MailAuthError{"Gmail 授权已失效，请在管理服务「Claude 账号」里重新授权（" + t.Error + "）"}
	}
	if status != http.StatusOK || t.AccessToken == "" {
		return "", fmt.Errorf("刷新 Gmail 授权失败（HTTP %d %s）", status, t.Error)
	}
	s.gmailMu.Lock()
	if s.gmailTokens == nil {
		s.gmailTokens = map[int64]cachedToken{}
	}
	s.gmailTokens[accountID] = cachedToken{t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}
	s.gmailMu.Unlock()
	return t.AccessToken, nil
}

// ---- 读取邮件 ----

func (s *Server) gmailGet(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.google().GmailURL+"/users/me"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.google().HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("连接 Gmail：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return &MailAuthError{"Gmail 授权已失效，请重新授权"}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return fmt.Errorf("Gmail 接口错误（HTTP %d）：%s", resp.StatusCode, e.Error.Message)
	}
	return json.Unmarshal(body, out)
}

// checkGmailProfile 核对授权的 Gmail 就是账号的邮箱。
func (s *Server) checkGmailProfile(ctx context.Context, token, want string) error {
	var p struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := s.gmailGet(ctx, token, "/profile", &p); err != nil {
		return err
	}
	if !strings.EqualFold(p.EmailAddress, want) {
		return &InputError{fmt.Sprintf("授权的是 %s，不是 %s；请在 Google 页面选对账号", p.EmailAddress, want)}
	}
	return nil
}

// fetchGmailLink 搜 since 之后 mail.anthropic.com 发来的邮件，取最新一封里的 magic link。
func (s *Server) fetchGmailLink(ctx context.Context, accountID int64, _ string, since time.Time) (string, error) {
	token, err := s.gmailAccessToken(ctx, accountID)
	if err != nil {
		return "", err
	}
	q := url.Values{"q": {fmt.Sprintf("from:%s after:%d", claudeMailDomain, since.Unix())}, "maxResults": {"5"}}
	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := s.gmailGet(ctx, token, "/messages?"+q.Encode(), &list); err != nil {
		return "", err
	}
	var link string
	var newest int64
	for _, m := range list.Messages {
		var msg struct {
			Raw          string `json:"raw"`
			InternalDate string `json:"internalDate"` // 毫秒
		}
		if err := s.gmailGet(ctx, token, "/messages/"+url.PathEscape(m.ID)+"?format=raw", &msg); err != nil {
			return "", err
		}
		at, _ := strconv.ParseInt(msg.InternalDate, 10, 64)
		if at < since.UnixMilli() || at <= newest {
			continue
		}
		raw, err := base64.URLEncoding.DecodeString(padBase64(msg.Raw))
		if err != nil {
			continue
		}
		if l := linkFromMessage(bytes.NewReader(raw)); l != "" {
			link, newest = l, at
		}
	}
	return link, nil
}

func padBase64(s string) string {
	if n := len(s) % 4; n != 0 {
		s += strings.Repeat("=", 4-n)
	}
	return s
}
