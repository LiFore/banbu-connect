package dist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	syncInterval    = 5 * time.Minute
	retryInterval   = 30 * time.Second
	requestTimeout  = 20 * time.Second
	maxProfileBytes = 16 << 20
)

// Binding 是客户端与管理服务的绑定，保存在数据目录的 server.json（只有管理员可读）。
type Binding struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// Fingerprint 是服务端证书的 SHA-256（十六进制）。服务端用自签名证书时必须填写，
	// 填了就只认这张证书；为空时按系统根证书校验。
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ClientStatus 是同步状态，展示用。不含令牌。
type ClientStatus struct {
	Bound       bool       `json:"bound"`
	URL         string     `json:"url"`
	Pinned      bool       `json:"pinned"`
	Version     int64      `json:"version"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Note        string     `json:"note"`
	LastSync    *time.Time `json:"last_sync,omitempty"`
	LastError   string     `json:"last_error"`
}

// Client 在桌面服务里运行：定期拉取配置，变化时通知重新生成内核配置。
type Client struct {
	dir        string
	appVersion string
	log        *slog.Logger
	kick       chan struct{}
	claudeOpen chan struct{} // 用户点了「打开登录链接」
	enrollMu   sync.Mutex    // 串行化接入申请的读写

	mu       sync.Mutex
	binding  *Binding
	profile  *Profile
	lastSync time.Time
	lastErr  string
	claude   *LocalClaudeLogin // 本机最近一次 Claude 登录，只在内存里
	openLink *pendingOpen      // 页面请求在浏览器打开的登录链接，等外壳认领
	gem      geminiState       // Gemini 账号同步、额度与登录（gemini_client.go）
	browser  *pendingBrowser   // 待外壳打开的浏览器档案（gemini_login.go）
	// gemTokens 是 Gemini 账号的访问令牌缓存，只在内存里（gemini_quota.go）。
	gemTokens    map[int64]cachedAccess
	gemQuotaKick chan struct{}
	ai           aiState // Claude / Codex 账号与凭据同步（ai_client.go）
	aiChanged    chan struct{}
	testsHandled map[string]bool // 已经开始执行的鉴权测试

	// GeminiRoute 给出 Gemini / Google 流量应走的代理入口；为空时直连（Lite 版不设置，总是直连）。
	GeminiRoute GeminiRoute
}

func NewClient(dir, appVersion string, log *slog.Logger) (*Client, error) {
	c := &Client{dir: dir, appVersion: appVersion, log: log, kick: make(chan struct{}, 1), claudeOpen: make(chan struct{}, 1),
		gemQuotaKick: make(chan struct{}, 1), aiChanged: make(chan struct{}, 1)}
	var b Binding
	switch err := readJSON(filepath.Join(dir, "server.json"), &b); {
	case err == nil:
		c.binding = &b
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("读取服务端绑定失败：%w", err)
	}
	var p Profile
	switch err := readJSON(filepath.Join(dir, "profile.json"), &p); {
	case err == nil:
		c.profile = &p
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("读取下发的配置失败：%w", err)
	}
	return c, nil
}

// Profile 返回已下发的配置。没有绑定时返回 false，使用本地配置。
func (c *Client) Profile() (Profile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.binding == nil || c.profile == nil {
		return Profile{}, false
	}
	return *c.profile, true
}

func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := ClientStatus{LastError: c.lastErr}
	if c.binding != nil {
		s.Bound, s.URL, s.Pinned = true, c.binding.URL, c.binding.Fingerprint != ""
	}
	if !c.lastSync.IsZero() {
		t := c.lastSync
		s.LastSync = &t
	}
	if p := c.profile; p != nil && c.binding != nil {
		s.Version, s.Note = p.Version, p.Note
		if !p.PublishedAt.IsZero() {
			t := p.PublishedAt
			s.PublishedAt = &t
		}
	}
	return s
}

// Run 定期同步。onChange 在下发的配置变化（含解绑）后调用。
func (c *Client) Run(ctx context.Context, onChange func()) {
	for {
		wait := syncInterval
		if changed, err := c.Sync(ctx); err != nil {
			wait = retryInterval
		} else if changed {
			onChange()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-c.kick:
		}
	}
}

// Kick 让 Run 立即同步一次。
func (c *Client) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Sync 拉取一次配置。返回配置是否变化。
func (c *Client) Sync(ctx context.Context) (bool, error) {
	c.mu.Lock()
	b := c.binding
	have := int64(0)
	if c.profile != nil {
		have = c.profile.Version
	}
	c.mu.Unlock()
	if b == nil {
		return false, nil
	}
	p, err := c.fetch(ctx, *b, have)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSync = time.Now().UTC()
	if err != nil {
		c.lastErr = err.Error()
		return false, err
	}
	c.lastErr = ""
	if p == nil {
		return false, nil
	}
	if err := writeJSON0600(filepath.Join(c.dir, "profile.json"), p); err != nil {
		c.lastErr = "保存下发的配置失败：" + err.Error()
		return false, err
	}
	c.profile = p
	if c.log != nil {
		c.log.Info("已同步服务端配置", "version", p.Version)
	}
	return true, nil
}

// Bind 绑定服务端：先拉取一次配置，成功才保存。
func (c *Client) Bind(ctx context.Context, b Binding) (ClientStatus, error) {
	b, err := normalizeBinding(b)
	if err != nil {
		return ClientStatus{}, err
	}
	p, err := c.fetch(ctx, b, 0)
	if err != nil {
		return ClientStatus{}, &InputError{"连接服务端失败：" + err.Error()}
	}
	if err := writeJSON0600(filepath.Join(c.dir, "server.json"), b); err != nil {
		return ClientStatus{}, err
	}
	if err := writeJSON0600(filepath.Join(c.dir, "profile.json"), p); err != nil {
		return ClientStatus{}, err
	}
	c.mu.Lock()
	c.binding, c.profile, c.lastSync, c.lastErr = &b, p, time.Now().UTC(), ""
	c.mu.Unlock()
	return c.Status(), nil
}

// Unbind 解除绑定，删除下发的配置，回到本地配置。
func (c *Client) Unbind() error {
	for _, f := range []string{"server.json", "profile.json"} {
		if err := os.Remove(filepath.Join(c.dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	c.mu.Lock()
	c.binding, c.profile, c.lastErr = nil, nil, ""
	c.mu.Unlock()
	return nil
}

// fetch 拉取配置；版本没变时返回 nil。
func (c *Client) fetch(ctx context.Context, b Binding, have int64) (*Profile, error) {
	resp, err := c.do(ctx, b, "/api/v1/client/profile", func(r *http.Request) {
		if have > 0 {
			r.Header.Set("If-None-Match", fmt.Sprintf(`"v%d"`, have))
		}
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, responseError(resp)
	}
	var p Profile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProfileBytes)).Decode(&p); err != nil {
		return nil, fmt.Errorf("配置内容无效：%w", err)
	}
	if p.Version <= 0 {
		return nil, errors.New("配置没有版本号")
	}
	return &p, nil
}

func (c *Client) do(ctx context.Context, b Binding, path string, edit func(*http.Request)) (*http.Response, error) {
	return c.doMethod(ctx, b, http.MethodGet, path, nil, edit)
}

func (c *Client) doMethod(ctx context.Context, b Binding, method, path string, body io.Reader, edit func(*http.Request)) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	req, err := http.NewRequestWithContext(ctx, method, b.URL+path, body)
	if err != nil {
		cancel()
		return nil, err
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	c.setInfoHeaders(req)
	if edit != nil {
		edit(req)
	}
	resp, err := httpClient(b, 0).Do(req)
	if err != nil {
		cancel()
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// httpClient 经系统代理以外的直连访问服务端。设了指纹时只认那张证书（自签名证书用）。
func httpClient(b Binding, timeout time.Duration) *http.Client {
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true}
	if b.Fingerprint != "" {
		want := b.Fingerprint
		tr.TLSClientConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // 由下面的指纹校验代替
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("服务端没有提供证书")
				}
				sum := sha256.Sum256(raw[0])
				if hex.EncodeToString(sum[:]) != want {
					return errors.New("服务端证书与绑定时的指纹不一致")
				}
				return nil
			},
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func normalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(":", "", " ", "", "sha256", "", "=", "").Replace(s)
	return s
}

func responseError(resp *http.Response) error {
	var e apiError
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return &HTTPError{Status: resp.StatusCode, Msg: fmt.Sprintf("%s（HTTP %d）", e.Message, resp.StatusCode)}
	}
	return &HTTPError{Status: resp.StatusCode, Msg: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

// HTTPError 是管理服务返回的非 200 响应。
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

// ---- 更新：Tauri updater 访问本机服务，由服务转发到管理服务（服务端证书和令牌都不交给外壳） ----

// CheckUpdate 查询更新。没有绑定或没有更新时返回 nil。
func (c *Client) CheckUpdate(ctx context.Context, target, arch, current string) (*UpdateInfo, error) {
	c.mu.Lock()
	b := c.binding
	c.mu.Unlock()
	if b == nil {
		return nil, nil
	}
	q := url.Values{"target": {target}, "arch": {arch}, "current_version": {current}}
	resp, err := c.do(ctx, *b, "/api/v1/client/update?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, responseError(resp)
	}
	var u UpdateInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Download 把更新包转发给 w：管理服务跳转到签名地址（OSS 或管理服务自己）时跟过去下载。
func (c *Client) Download(ctx context.Context, id int64, w http.ResponseWriter) error {
	resp, err := c.OpenBlob(ctx, fmt.Sprintf("/api/v1/client/releases/%d/download", id))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, resp.Body)
	return err
}

// ---- 文件 ----

func readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(b)).Decode(out)
}

func writeJSON0600(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
