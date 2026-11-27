package dist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ---- 客户端：一键发起 Claude Code 登录（BXC-F1-085）----
//
// 页面点「登录」→ 本机服务向管理服务发起，记下待启动的登录 → 托盘外壳（运行在用户会话里）
// 从状态里看到后认领，执行 claude auth login --claudeai --email <邮箱> → 本机服务从管理服务取到
// magic link 后先留在本机（阶段 mail_ready），页面显示登录邮件 → 用户在邮件里点「打开登录链接」
// → 外壳的长轮询这时才拿到链接，用默认浏览器打开 → Claude Code 自己完成授权和保存凭据 →
// 外壳用 claude auth status 核对邮箱并回报。服务以 SYSTEM/root 运行，不能自己执行 claude，
// 否则凭据会写到管理员的目录。链接不交给页面，页面只能请求「打开」。

// 本机登录的阶段。
const (
	PhaseLaunching = "launching"    // 等托盘外壳启动 Claude Code
	PhaseWaiting   = "waiting_mail" // Claude Code 已启动，等登录邮件
	PhaseMailReady = "mail_ready"   // 登录邮件已到，等用户在邮件里点「打开登录链接」
	PhaseOpened    = "opened"       // magic link 已在浏览器打开，等授权完成
	PhaseDone      = "done"
	PhaseFailed    = "failed"
)

// pendingClaim 是外壳认领的期限：超过还没认领，多半是托盘没在运行。
const pendingClaim = 2 * time.Minute

// LocalClaudeLogin 是本机当前（最近一次）的 Claude 登录，页面据此显示进度。
type LocalClaudeLogin struct {
	ID        string    `json:"id"`
	AccountID int64     `json:"account_id"`
	Email     string    `json:"email"`
	Phase     string    `json:"phase"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Claimed   bool      `json:"claimed"`

	link    string // 已取到的 magic link，只交给外壳
	openReq bool   // 用户点了「打开登录链接」
}

// PendingClaude 放在状态里给外壳：有待启动的登录。
type PendingClaude struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func (c *Client) binding0() (Binding, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.binding == nil {
		return Binding{}, &InputError{"还没有绑定管理服务"}
	}
	return *c.binding, nil
}

// doJSON 调用管理服务的客户端接口。
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	b, err := c.binding0()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	c.setInfoHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient(b, timeout).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("连接管理服务：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// ClaudeAccounts 返回管理服务允许本机使用的 Claude 和 Codex 账号（Kind 区分）。
func (c *Client) ClaudeAccounts(ctx context.Context) ([]ClientClaudeAccount, error) {
	var out []ClientClaudeAccount
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/client/claude-accounts?kinds=claude,codex", nil, &out, requestTimeout)
	if out == nil {
		out = []ClientClaudeAccount{}
	}
	return out, err
}

// StartClaudeLogin 让管理服务开始等这个账号的登录邮件，并记下待外壳启动的登录。
func (c *Client) StartClaudeLogin(ctx context.Context, accountID int64) (LocalClaudeLogin, error) {
	var l ClaudeLogin
	if err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/client/claude-accounts/%d/login", accountID), struct{}{}, &l, requestTimeout); err != nil {
		return LocalClaudeLogin{}, err
	}
	local := LocalClaudeLogin{ID: l.ID, AccountID: l.AccountID, Email: l.Email, Phase: PhaseLaunching, StartedAt: time.Now()}
	c.mu.Lock()
	c.claude = &local
	c.mu.Unlock()
	return local, nil
}

// ClaudeLogin 返回本机最近一次登录。
func (c *Client) ClaudeLogin() *LocalClaudeLogin {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claude == nil {
		return nil
	}
	l := *c.claude
	if l.Phase == PhaseLaunching && !l.Claimed && time.Since(l.StartedAt) > pendingClaim {
		l.Phase, l.Error = PhaseFailed, "托盘程序没有启动登录，请确认 Banbu Connect 托盘在运行"
	}
	return &l
}

// PendingClaude 返回待外壳启动的登录。
func (c *Client) PendingClaude() *PendingClaude {
	l := c.ClaudeLogin()
	if l == nil || l.Phase != PhaseLaunching || l.Claimed {
		return nil
	}
	return &PendingClaude{ID: l.ID, Email: l.Email}
}

func (c *Client) updateClaude(id string, f func(*LocalClaudeLogin)) (LocalClaudeLogin, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claude == nil || c.claude.ID != id {
		return LocalClaudeLogin{}, ErrNotFound
	}
	f(c.claude)
	return *c.claude, nil
}

// ClaimClaudeLogin 外壳认领待启动的登录，只能认领一次。
func (c *Client) ClaimClaudeLogin(id string) (LocalClaudeLogin, error) {
	var already bool
	l, err := c.updateClaude(id, func(l *LocalClaudeLogin) {
		if l.Claimed || l.Phase != PhaseLaunching {
			already = true
			return
		}
		l.Claimed, l.Phase = true, PhaseWaiting
	})
	if err == nil && already {
		return l, &InputError{"这次登录已经启动过"}
	}
	return l, err
}

// WaitClaudeLink 是外壳的长轮询：链接已取到、且用户点了「打开登录链接」时才返回链接，阶段改为已打开。
// 链接还没取到时经管理服务长轮询；取到后留在本机，阶段改为 mail_ready，等用户点击。
func (c *Client) WaitClaudeLink(ctx context.Context, id string, wait time.Duration) (ClaudeLogin, error) {
	c.mu.Lock()
	if c.claude == nil || c.claude.ID != id {
		c.mu.Unlock()
		return ClaudeLogin{}, ErrNotFound
	}
	have := c.claude.link != ""
	c.mu.Unlock()
	if !have {
		l, err := c.pollServerLink(ctx, id, wait)
		if err != nil || l.Link == "" {
			return l, err
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		c.mu.Lock()
		if c.claude == nil || c.claude.ID != id {
			c.mu.Unlock()
			return ClaudeLogin{}, ErrNotFound
		}
		local := c.claude
		out := ClaudeLogin{ID: id, AccountID: local.AccountID, Email: local.Email, Status: LoginDelivered}
		if local.Phase == PhaseFailed || local.Phase == PhaseDone {
			out.Status, out.Error = LoginFailed, local.Error
			c.mu.Unlock()
			return out, nil
		}
		if local.openReq {
			out.Link = local.link
			local.Phase, local.Error = PhaseOpened, ""
			c.mu.Unlock()
			return out, nil
		}
		c.mu.Unlock()
		select {
		case <-c.claudeOpen:
		case <-timer.C:
			return ClaudeLogin{ID: id, Status: LoginDelivered}, nil
		case <-ctx.Done():
			return ClaudeLogin{ID: id, Status: LoginDelivered}, nil
		}
	}
}

// OpenClaudeLink 是页面上的「打开登录链接」：让外壳在浏览器打开已取到的链接。
func (c *Client) OpenClaudeLink(id string) (LocalClaudeLogin, error) {
	var notReady bool
	l, err := c.updateClaude(id, func(l *LocalClaudeLogin) {
		if l.link == "" || (l.Phase != PhaseMailReady && l.Phase != PhaseOpened) {
			notReady = true
			return
		}
		l.openReq = true
	})
	if err != nil {
		return l, err
	}
	if notReady {
		return l, &InputError{"登录邮件还没到，或这次登录已结束"}
	}
	select {
	case c.claudeOpen <- struct{}{}:
	default:
	}
	return l, nil
}

// pollServerLink 经管理服务长轮询 magic link，取到时留在本机。
func (c *Client) pollServerLink(ctx context.Context, id string, wait time.Duration) (ClaudeLogin, error) {
	var l ClaudeLogin
	path := fmt.Sprintf("/api/v1/client/claude-logins/%s?wait=%d", url.PathEscape(id), int(wait.Seconds()))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &l, wait+requestTimeout); err != nil {
		return l, err
	}
	_, _ = c.updateClaude(id, func(local *LocalClaudeLogin) {
		switch {
		case l.Link != "":
			local.link, local.Phase, local.Error = l.Link, PhaseMailReady, ""
		case l.Status == LoginFailed || l.Status == LoginExpired:
			local.Phase, local.Error = PhaseFailed, l.Error
		case l.Status == LoginWaiting:
			local.Error = l.Error // 读邮箱重试中的提示
		}
	})
	return l, nil
}

// FinishClaudeLogin 把外壳核对的结果回报给管理服务。
func (c *Client) FinishClaudeLogin(ctx context.Context, id string, ok bool, email, msg string) (LocalClaudeLogin, error) {
	var l ClaudeLogin
	err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/client/claude-logins/%s/result", url.PathEscape(id)),
		map[string]any{"ok": ok, "email": email, "error": msg}, &l, requestTimeout)
	phase, errMsg := PhaseDone, ""
	switch {
	case err != nil && ok:
		// 本机已经登录成功，只是回报失败：仍算成功，提示一下。
		errMsg = "已登录，但回报管理服务失败：" + err.Error()
	case err != nil:
		phase, errMsg = PhaseFailed, msg
	case l.Status != LoginDone:
		phase, errMsg = PhaseFailed, l.Error
	}
	local, uerr := c.updateClaude(id, func(local *LocalClaudeLogin) { local.Phase, local.Error = phase, errMsg })
	if uerr != nil {
		return local, uerr
	}
	return local, nil
}

// ---- 在浏览器打开邮件里的登录链接 ----
//
// 页面运行在本机服务的网页里，没有打开外部链接的权限（ADR-023）：页面请求后，托盘外壳从状态里看到
// 待打开的链接，认领后用默认浏览器打开。只接受 Claude 的 magic link，页面不能借外壳打开任意网址。
// 不要求先点「登录」：自己手动登录的用户也能用（一键登录进行中、点的正是它的链接时，按登录流程处理）。

// openLinkTTL 是待打开链接的有效期：超过还没被认领，多半是托盘没在运行。
const openLinkTTL = time.Minute

// PendingOpen 放在状态里给外壳：有待打开的链接。不含链接本身，外壳认领时才拿到。
type PendingOpen struct {
	ID string `json:"id"`
}

type pendingOpen struct {
	id, link string
	at       time.Time
}

// IsMagicLink 判断是不是 Claude 的登录链接（整串匹配）。
func IsMagicLink(s string) bool {
	return magicLinkRE.FindString(s) == s && s != ""
}

// RequestOpenLink 请求托盘外壳在浏览器打开登录链接。
func (c *Client) RequestOpenLink(link string) error {
	if !IsMagicLink(link) {
		return &InputError{"只能打开 Claude 的登录链接"}
	}
	c.mu.Lock()
	l := c.claude
	viaLogin := l != nil && l.link == link && (l.Phase == PhaseMailReady || l.Phase == PhaseOpened)
	var id string
	if viaLogin {
		id = l.ID
	} else {
		c.openLink = &pendingOpen{id: newLoginID(), link: link, at: time.Now()}
	}
	c.mu.Unlock()
	if viaLogin {
		_, err := c.OpenClaudeLink(id)
		return err
	}
	return nil
}

// PendingOpen 返回待外壳打开的链接。
func (c *Client) PendingOpen() *PendingOpen {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openLink == nil || time.Since(c.openLink.at) > openLinkTTL {
		return nil
	}
	return &PendingOpen{ID: c.openLink.id}
}

// ClaimOpenLink 外壳认领待打开的链接，只能认领一次。
func (c *Client) ClaimOpenLink(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openLink == nil || c.openLink.id != id || time.Since(c.openLink.at) > openLinkTTL {
		return "", ErrNotFound
	}
	link := c.openLink.link
	c.openLink = nil
	return link, nil
}
