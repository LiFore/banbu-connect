package dist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ---- 客户端：同步 Claude / Codex 账号与托管的凭据（ADR-061）----
//
// 本机服务长轮询管理服务，拿到允许本机使用的账号、托管的凭据和交给本机的鉴权测试。
// 凭据写进哪个工作区、何时读回上传，由 desktop 包按工作区状态决定（服务以 SYSTEM/root 运行，
// 写钥匙串和用户目录要交给托盘外壳）。

const aiSyncWait = 25 * time.Second

type aiState struct {
	sync    AISync
	synced  bool
	lastErr string
}

// RunAI 持续同步，直到 ctx 结束。没有绑定时清空并等待。
func (c *Client) RunAI(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := c.binding0(); err != nil {
			c.setAISync(AISync{}, false)
			sleepCtx(ctx, retryInterval)
			continue
		}
		c.mu.Lock()
		since := c.ai.sync.Hash
		c.mu.Unlock()
		var out AISync
		// caps=refresh：本机能执行令牌续期任务（ADR-071），管理服务只把续期交给这样的客户端。
		q := url.Values{"since": {since}, "wait": {strconv.Itoa(int(aiSyncWait / time.Second))}, "caps": {"refresh"}}
		err := c.doJSON(ctx, http.MethodGet, "/api/v1/client/ai/sync?"+q.Encode(), nil, &out, aiSyncWait+20*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			c.ai.lastErr = err.Error()
			c.mu.Unlock()
			wait := retryInterval
			var he *HTTPError
			if errors.As(err, &he) && he.Status == http.StatusNotFound {
				wait = syncInterval // 管理服务还是旧版本，没有这个接口
			}
			sleepCtx(ctx, wait)
			continue
		}
		c.setAISync(out, true)
	}
}

func (c *Client) setAISync(out AISync, synced bool) {
	c.mu.Lock()
	changed := out.Hash != c.ai.sync.Hash || synced != c.ai.synced
	c.ai = aiState{sync: out, synced: synced}
	c.mu.Unlock()
	if changed {
		select {
		case c.aiChanged <- struct{}{}:
		default:
		}
	}
}

// AIChanged 在同步结果变化时收到通知。
func (c *Client) AIChanged() <-chan struct{} { return c.aiChanged }

// AISyncState 返回最近一次同步的结果；还没同步成功过时第二个返回值为 false。
func (c *Client) AISyncState() (AISync, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ai.sync, c.ai.synced
}

// UploadAICredential 上传本机的凭据，返回新的版本号。版本号过期时返回 ErrCredentialConflict。
// 成功后同步结果里的凭据和版本号同时更新，避免下一次同步前把旧凭据又写回工作区。
func (c *Client) UploadAICredential(ctx context.Context, accountID int64, up AICredentialUpload) (int64, error) {
	var resp struct {
		Version int64 `json:"version"`
	}
	err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/v1/client/ai/accounts/%d/credential", accountID), up, &resp, requestTimeout)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict {
		return 0, ErrCredentialConflict
	}
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	for i := range c.ai.sync.Accounts {
		a := &c.ai.sync.Accounts[i]
		if a.ID == accountID && (up.Replace || a.Version == up.BaseVersion) {
			cred := up.Credential
			a.Credential, a.Version = &cred, resp.Version
		}
	}
	c.mu.Unlock()
	return resp.Version, nil
}

// AckAIRefresh 回报对续期通知的处理：busy 表示本机正在用这个账号（ADR-071）。
func (c *Client) AckAIRefresh(ctx context.Context, id string, busy bool) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/ai/refresh/"+url.PathEscape(id)+"/ack", map[string]bool{"busy": busy}, nil, requestTimeout)
}

// FinishAIRefresh 交回续期结果。新的 Refresh Token 只在这里，网络出错时重试几次；
// 管理服务明确拒绝（4xx，包括版本冲突）时不再重试。
func (c *Client) FinishAIRefresh(ctx context.Context, id string, r AIRefreshResult) error {
	var err error
	for attempt := range 6 {
		if attempt > 0 {
			sleepCtx(ctx, time.Duration(attempt)*5*time.Second)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		err = c.doJSON(ctx, http.MethodPost, "/api/v1/client/ai/refresh/"+url.PathEscape(id)+"/result", r, nil, requestTimeout)
		var he *HTTPError
		if err == nil || (errors.As(err, &he) && he.Status >= 400 && he.Status < 500) {
			return err
		}
	}
	return err
}

// ReportAIQuota 上报各账号的额度。
func (c *Client) ReportAIQuota(ctx context.Context, samples []AIQuotaSample) error {
	if len(samples) == 0 {
		return nil
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/ai/quota", map[string]any{"samples": samples}, nil, requestTimeout)
}

// ReportAIOnline 上报此刻在用的 AI 账号（覆盖式），管理服务据此统计在线时长。
func (c *Client) ReportAIOnline(ctx context.Context, accounts []AIOnlineAccount) error {
	if accounts == nil {
		accounts = []AIOnlineAccount{}
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/ai/online", map[string]any{"accounts": accounts}, nil, requestTimeout)
}

// GeminiActive 返回本机当前的 Gemini 账号；还没同步过或没有时为 0。
func (c *Client) GeminiActive() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadGeminiLocked()
	if !c.gem.synced {
		return 0
	}
	return c.gem.saved.Active
}

// ClaimAuthTest 标记测试已开始执行；已经执行过的返回 false（同步结果在回报前可能重复出现）。
func (c *Client) ClaimAuthTest(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.testsHandled == nil {
		c.testsHandled = map[string]bool{}
	}
	if c.testsHandled[id] {
		return false
	}
	if len(c.testsHandled) > 200 {
		clear(c.testsHandled)
	}
	c.testsHandled[id] = true
	return true
}

// FinishAuthTest 回报鉴权测试的结果。
func (c *Client) FinishAuthTest(ctx context.Context, id string, r AuthTestResult) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/auth-tests/"+url.PathEscape(id)+"/result", r, nil, requestTimeout)
}

// runGeminiTests 执行交给本机的 Gemini 鉴权测试：用托管的凭据换访问令牌、查一次额度。
func (c *Client) runGeminiTests(ctx context.Context, out GeminiSync) {
	for _, t := range out.Tests {
		if !c.ClaimAuthTest(t.ID) {
			continue
		}
		go func(t AuthTest) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			r := AuthTestResult{}
			a := findGemini(out, t.AccountID)
			switch {
			case a == nil || len(a.Credential) == 0:
				r.Error = "本机没有同步到这个账号的凭据"
			case out.OAuth == nil:
				r.Error = "管理服务还没有设置 Antigravity OAuth 客户端"
			default:
				if _, err := c.fetchGeminiQuota(ctx, *a, *out.OAuth); err != nil {
					r.Error = err.Error()
				} else {
					r.OK = true
					c.KickGeminiQuota()
				}
			}
			if err := c.FinishAuthTest(ctx, t.ID, r); err != nil {
				c.log.Warn("回报鉴权测试结果失败", "err", err)
			}
		}(t)
	}
}
