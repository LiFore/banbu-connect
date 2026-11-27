package dist

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

// ---- 服务端 ----

var installIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

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

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Enrollment 返回本机的申请；没有时为 nil。
func (c *Client) Enrollment() (*EnrollmentView, error) {
	c.enrollMu.Lock()
	defer c.enrollMu.Unlock()
	e, err := c.loadEnrollment()
	return e.view(), err
}
