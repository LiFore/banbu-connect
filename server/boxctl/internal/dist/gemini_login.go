package dist

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---- 每账号固定浏览器档案（BXC-F0-019，ADR-047）----
//
// 每个 Gemini 账号在当前用户的 Banbu Connect 数据目录下有一个浏览器档案，用系统 Chrome / Edge
// 加 --user-data-dir 打开，登录流量经严格组的本机探测入口（--proxy-server），出口固定。
// 浏览器要在用户会话里启动，由托盘外壳做：服务把待打开的档案放进状态，外壳认领后启动。
// 不做指纹伪装。

// browserClaim 是外壳认领的期限：超过还没认领，多半是托盘没在运行。
const browserClaim = 2 * time.Minute

// PendingBrowser 放在状态里给外壳：有待打开的浏览器档案。
type PendingBrowser struct {
	ID string `json:"id"`
}

// BrowserLaunch 是外壳认领后拿到的启动参数。Profile 是档案名（账号邮箱），外壳把它转成目录名。
// URL 只会是 Google 的地址；Proxy 为空表示内核没在运行，直接打开。
type BrowserLaunch struct {
	Profile string `json:"profile"`
	URL     string `json:"url"`
	Proxy   string `json:"proxy,omitempty"`
	Group   string `json:"group,omitempty"` // 经过的严格组，展示用
}

type pendingBrowser struct {
	id      string
	launch  BrowserLaunch
	created time.Time
	claimed bool
}

// GeminiRoute 返回 Gemini / Google 登录流量应走的严格组和它的本机入口（http://127.0.0.1:端口）；
// 由桌面服务设置，内核没在运行时返回空。
type GeminiRoute func() (group, proxy string)

// geminiAccountURL 是「打开浏览器档案」默认打开的页面。
const geminiAccountURL = "https://myaccount.google.com/"

// allowedBrowserURL 只允许打开 Google 账号相关的地址：外壳会按这个地址启动浏览器。
func allowedBrowserURL(u string) bool {
	return strings.HasPrefix(u, "https://accounts.google.com/") || strings.HasPrefix(u, "https://myaccount.google.com/")
}

// OpenGeminiProfile 请求外壳用这个账号的浏览器档案打开 url（空为 Google 账号页）。
func (c *Client) OpenGeminiProfile(accountID int64, u string) (BrowserLaunch, error) {
	if u == "" {
		u = geminiAccountURL
	}
	if !allowedBrowserURL(u) {
		return BrowserLaunch{}, &InputError{"只能打开 Google 账号的页面"}
	}
	c.mu.Lock()
	a := findGemini(c.gem.sync, accountID)
	c.mu.Unlock()
	if a == nil {
		return BrowserLaunch{}, &InputError{"这个账号没有分配给本机"}
	}
	l := BrowserLaunch{Profile: a.Email, URL: u}
	l.Group, l.Proxy = c.geminiRoute()
	c.mu.Lock()
	c.browser = &pendingBrowser{id: newLoginID(), launch: l, created: time.Now()}
	c.mu.Unlock()
	return l, nil
}

// PendingBrowser 返回待外壳打开的档案；认领过或过期的不再返回。
func (c *Client) PendingBrowser() *PendingBrowser {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.browser
	if b == nil || b.claimed || time.Since(b.created) > browserClaim {
		return nil
	}
	return &PendingBrowser{ID: b.id}
}

// ClaimBrowser 由外壳调用：认领并取得启动参数，每个请求只能认领一次。
func (c *Client) ClaimBrowser(id string) (BrowserLaunch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.browser
	if b == nil || b.id != id || b.claimed || time.Since(b.created) > browserClaim {
		return BrowserLaunch{}, ErrNotFound
	}
	b.claimed = true
	return b.launch, nil
}

func (c *Client) geminiRoute() (string, string) {
	if c.GeminiRoute == nil {
		return "", ""
	}
	return c.GeminiRoute()
}

// googleHTTP 是访问 Google 的 HTTP 客户端：经 Gemini 的严格组出去，和浏览器档案、各客户端的出口一致。
func (c *Client) googleHTTP(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if _, p := c.geminiRoute(); p != "" {
		if u, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ---- 一键登录 Antigravity 并上传凭据（BXC-F0-020）----
//
// 不驱动 agy / 应用的登录界面，客户端自己跑一遍 Google OAuth（做法见 docs/Antigravity实测.md 的 cockpit-tools 一节）：
// 本机服务在 127.0.0.1 随机端口等回跳 → 外壳用该账号的固定浏览器档案打开授权页（经严格组出去）→
// 用户授权后浏览器回跳到本机 → 服务换 token、核对邮箱，拼成 Antigravity 的凭据 JSON 上传管理服务（重新登录取代旧凭据）。
// 回跳打不开时，可以把地址栏里的回跳地址粘贴到页面上。

const (
	geminiLoginTTL  = 10 * time.Minute
	geminiLoginKeep = 30 * time.Minute
)

// 登录阶段。
const (
	GeminiPhaseOpening    = "opening"     // 等外壳打开浏览器档案
	GeminiPhaseAuthorize  = "authorizing" // 授权页已打开，等用户授权
	GeminiPhaseExchanging = "exchanging"  // 收到回跳，正在换 token、上传
	GeminiPhaseDone       = "done"
	GeminiPhaseFailed     = "failed"
)

// geminiScopes 是 Antigravity 登录要的范围（cockpit-tools 读出）。TODO(verify:V-agy-browser)
var geminiScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
	"https://www.googleapis.com/auth/aicode",
}

// geminiEndpoints 是 Google 的接口地址；测试时替换。
var geminiEndpoints = struct {
	Auth, Token, UserInfo, CloudCode string
}{
	Auth:      "https://accounts.google.com/o/oauth2/v2/auth",
	Token:     "https://oauth2.googleapis.com/token",
	UserInfo:  "https://www.googleapis.com/oauth2/v2/userinfo",
	CloudCode: "https://cloudcode-pa.googleapis.com",
}

// LocalGeminiLogin 是本机最近一次 Gemini 登录，页面据此显示进度。
type LocalGeminiLogin struct {
	ID        string    `json:"id"`
	AccountID int64     `json:"account_id"`
	Email     string    `json:"email"`
	Phase     string    `json:"phase"`
	Error     string    `json:"error,omitempty"`
	Group     string    `json:"group,omitempty"` // 授权流量经过的严格组
	StartedAt time.Time `json:"started_at"`
}

type geminiLogin struct {
	LocalGeminiLogin
	state, verifier, redirect string
	oauth                     GeminiOAuth
	srv                       *http.Server
	ended                     time.Time
}

func (c *Client) geminiLoginLocked() *LocalGeminiLogin {
	l := c.gem.login
	if l == nil || (!l.ended.IsZero() && time.Since(l.ended) > geminiLoginKeep) {
		return nil
	}
	out := l.LocalGeminiLogin
	if out.Phase == GeminiPhaseOpening && c.browser != nil && c.browser.claimed {
		out.Phase = GeminiPhaseAuthorize
	}
	return &out
}

// StartGeminiLogin 开始登录这个账号：在本机等回跳，请外壳用这个账号的浏览器档案打开 Google 授权页。
func (c *Client) StartGeminiLogin(accountID int64) (LocalGeminiLogin, error) {
	c.mu.Lock()
	a := findGemini(c.gem.sync, accountID)
	oauth := c.gem.sync.OAuth
	c.mu.Unlock()
	if a == nil {
		return LocalGeminiLogin{}, &InputError{"这个账号没有分配给本机"}
	}
	if oauth == nil || oauth.ClientSecret == "" {
		return LocalGeminiLogin{}, &InputError{"管理服务还没有设置 Antigravity OAuth 客户端"}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return LocalGeminiLogin{}, fmt.Errorf("本机等待回跳失败：%w", err)
	}
	l := &geminiLogin{
		LocalGeminiLogin: LocalGeminiLogin{ID: newLoginID(), AccountID: a.ID, Email: a.Email, Phase: GeminiPhaseOpening, StartedAt: time.Now()},
		state:            newLoginID(),
		verifier:         newLoginID() + newLoginID() + newLoginID(), // 72 个十六进制字符，符合 PKCE 43～128
		redirect:         fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", ln.Addr().(*net.TCPAddr).Port),
		oauth:            *oauth,
	}
	sum := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"client_id":              {oauth.ClientID},
		"redirect_uri":           {l.redirect},
		"response_type":          {"code"},
		"scope":                  {strings.Join(geminiScopes, " ")},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"state":                  {l.state},
		"login_hint":             {a.Email},
		"code_challenge":         {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method":  {"S256"},
	}
	authURL := geminiEndpoints.Auth + "?" + q.Encode()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		err := c.finishGeminiLogin(r.Context(), l.ID, r.URL.Query())
		msg := "已登录 " + l.Email + "，凭据已同步到管理服务。可以关闭这个页面。"
		if err != nil {
			msg = "登录没有完成：" + err.Error()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Banbu Connect</title><p style="font:15px sans-serif;margin:40px">%s</p>`,
			html.EscapeString(msg))
	})
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	launch := BrowserLaunch{Profile: a.Email, URL: authURL}
	launch.Group, launch.Proxy = c.geminiRoute()
	l.Group = launch.Group

	c.mu.Lock()
	if old := c.gem.login; old != nil {
		c.endGeminiLoginLocked(old, GeminiPhaseFailed, "已被新的登录取代")
	}
	c.gem.login = l
	c.browser = &pendingBrowser{id: newLoginID(), launch: launch, created: time.Now()}
	out := l.LocalGeminiLogin
	c.mu.Unlock()

	go func() { _ = l.srv.Serve(ln) }()
	go func() {
		time.Sleep(geminiLoginTTL)
		c.mu.Lock()
		c.endGeminiLoginLocked(l, GeminiPhaseFailed, "10 分钟内没有完成授权")
		c.mu.Unlock()
	}()
	return out, nil
}

// endGeminiLoginLocked 结束登录、关掉回跳监听。调用方持有 c.mu。
func (c *Client) endGeminiLoginLocked(l *geminiLogin, phase, msg string) {
	if !l.ended.IsZero() {
		return
	}
	l.Phase, l.Error, l.ended = phase, msg, time.Now()
	srv := l.srv
	go func() {
		time.Sleep(time.Second) // 让回跳页面的响应先发出去
		_ = srv.Close()
	}()
}

// FinishGeminiLoginURL 用页面粘贴的回跳地址完成登录。
func (c *Client) FinishGeminiLoginURL(ctx context.Context, id, pasted string) error {
	u, err := url.Parse(strings.TrimSpace(pasted))
	if err != nil || u.Query().Get("state") == "" {
		return &InputError{"回跳地址不对：应形如 http://127.0.0.1:端口/oauth-callback?state=…&code=…"}
	}
	return c.finishGeminiLogin(ctx, id, u.Query())
}

func (c *Client) finishGeminiLogin(ctx context.Context, id string, q url.Values) error {
	c.mu.Lock()
	l := c.gem.login
	if l == nil || l.ID != id || !l.ended.IsZero() || l.Phase == GeminiPhaseExchanging {
		c.mu.Unlock()
		return ErrNotFound
	}
	if q.Get("state") != l.state {
		c.mu.Unlock()
		return &InputError{"回跳地址不属于这次登录"}
	}
	if e := q.Get("error"); e != "" {
		c.endGeminiLoginLocked(l, GeminiPhaseFailed, "Google 返回："+e)
		c.mu.Unlock()
		return &InputError{"Google 返回：" + e}
	}
	l.Phase = GeminiPhaseExchanging
	acc, email, oauth, verifier, redirect := l.AccountID, l.Email, l.oauth, l.verifier, l.redirect
	c.mu.Unlock()

	version, cred, err := c.exchangeGemini(ctx, acc, email, oauth, q.Get("code"), verifier, redirect)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.endGeminiLoginLocked(l, GeminiPhaseFailed, err.Error())
		return err
	}
	c.endGeminiLoginLocked(l, GeminiPhaseDone, "")
	// 不等下一次同步：立即用上新凭据；本机还没有当前账号时用它。
	if a := findGemini(c.gem.sync, acc); a != nil && version > a.Version {
		a.Credential, a.Version = cred, version
	}
	if c.gem.saved.Active == 0 {
		c.gem.saved.Active = acc
		c.gem.reported = false // 下一次同步时回报
		c.saveGeminiLocked()
	}
	return nil
}

type googleToken struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
}

// GoogleGrantError 是 refresh token 或授权码失效（invalid_grant）。
type GoogleGrantError struct{ Desc string }

func (e *GoogleGrantError) Error() string {
	return "授权已失效，需要重新登录（" + e.Desc + "）"
}

func (c *Client) googleToken(ctx context.Context, form url.Values) (googleToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiEndpoints.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return googleToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.googleHTTP(30 * time.Second).Do(req)
	if err != nil {
		return googleToken{}, fmt.Errorf("连接 Google 失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "invalid_grant" {
			return googleToken{}, &GoogleGrantError{e.Desc}
		}
		return googleToken{}, fmt.Errorf("Google 拒绝了请求（HTTP %d %s %s）", resp.StatusCode, e.Error, e.Desc)
	}
	var t googleToken
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return googleToken{}, errors.New("Google 的响应里没有访问令牌")
	}
	return t, nil
}

// googleEmail 取访问令牌所属的邮箱。
func (c *Client) googleEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geminiEndpoints.UserInfo, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.googleHTTP(20 * time.Second).Do(req)
	if err != nil {
		return "", fmt.Errorf("读取 Google 账号失败：%w", err)
	}
	defer resp.Body.Close()
	var u struct {
		Email string `json:"email"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u) != nil {
		return "", fmt.Errorf("读取 Google 账号失败（HTTP %d）", resp.StatusCode)
	}
	return strings.ToLower(u.Email), nil
}

// geminiCredentialJSON 拼成 Antigravity 的凭据 JSON（格式见 ParseGeminiCredential）。TODO(verify:V-agy-store)
func geminiCredentialJSON(t googleToken, refresh string, now time.Time) []byte {
	typ := t.TokenType
	if typ == "" {
		typ = "Bearer"
	}
	b, _ := json.Marshal(map[string]any{
		"token": map[string]string{
			"access_token":  t.AccessToken,
			"token_type":    typ,
			"refresh_token": refresh,
			"expiry":        now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC().Format("2006-01-02T15:04:05.000000Z"),
		},
		"auth_method": "consumer",
		"id_token":    t.IDToken,
	})
	return b
}

// exchangeGemini 用授权码换 token、核对邮箱、上传管理服务，返回新的版本号和凭据。
func (c *Client) exchangeGemini(ctx context.Context, accountID int64, email string, oauth GeminiOAuth, code, verifier, redirect string) (int64, []byte, error) {
	if code == "" {
		return 0, nil, &InputError{"回跳地址里没有授权码"}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	t, err := c.googleToken(ctx, url.Values{
		"code": {code}, "client_id": {oauth.ClientID}, "client_secret": {oauth.ClientSecret},
		"redirect_uri": {redirect}, "grant_type": {"authorization_code"}, "code_verifier": {verifier},
	})
	if err != nil {
		return 0, nil, err
	}
	if t.RefreshToken == "" {
		return 0, nil, &InputError{"Google 没有给 refresh token：请到 myaccount.google.com/permissions 撤销 Antigravity 的授权后重新登录"}
	}
	got := idTokenEmail(t.IDToken)
	if got == "" {
		if got, err = c.googleEmail(ctx, t.AccessToken); err != nil {
			return 0, nil, err
		}
	}
	if got != email {
		return 0, nil, &InputError{fmt.Sprintf("登录的是 %s，不是 %s", got, email)}
	}
	cred := geminiCredentialJSON(t, t.RefreshToken, time.Now())
	var resp struct {
		Version int64 `json:"version"`
	}
	if err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/v1/client/gemini/accounts/%d/credential", accountID),
		CredentialUpload{Replace: true, Source: "login", Credential: cred}, &resp, requestTimeout); err != nil {
		return 0, nil, fmt.Errorf("上传凭据失败：%w", err)
	}
	return resp.Version, cred, nil
}
