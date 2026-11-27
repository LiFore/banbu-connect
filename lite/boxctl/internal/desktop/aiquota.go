package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"boxctl/internal/dist"
)

const kindGemini = "gemini"

// ---- Claude / Codex 额度（BXC-F0-039、BXC-F0-040）----
//
// 外壳扫描工作区时读出访问令牌（只读，不刷新：刷新会和正在运行的 claude / codex 抢 refresh token），
// 本机服务每 5 分钟用它查一次额度，经账号类型对应的严格组出去。两个接口都不公开，版本更新可能失效：
//   - Claude：GET https://api.anthropic.com/api/oauth/usage，Bearer + anthropic-beta: oauth-2025-04-20，
//     返回 five_hour / seven_day / seven_day_opus … 各项的 utilization（已用百分比）和 resets_at；
//   - Codex：GET https://chatgpt.com/backend-api/wham/usage，Bearer + ChatGPT-Account-Id，
//     返回 rate_limit.primary_window（5 小时）/ secondary_window（每周）的 used_percent、limit_window_seconds、reset_at。
// 令牌过期（401）时显示「需要在终端里用一次 claude / codex 刷新登录」，不自己刷新。
// 只在保护服务放通时查询（quotaGate）：代理没在运行、Kill Switch 在阻断时一律不发，不会直连出去。

const (
	aiQuotaEvery = 5 * time.Minute
	// 暂停查询时隔这么久再看一次放通了没有。
	aiQuotaPausedRetry = 30 * time.Second
)

// AIQuotaWindow 是额度的一个窗口；和上报管理服务的格式相同（ADR-061）。
type AIQuotaWindow = dist.AIQuotaWindow

// AIQuota 是一个账号的额度。
type AIQuota = dist.AIQuota

// aiQuotaEndpoints 是接口地址；测试时替换。
var aiQuotaEndpoints = struct{ Claude, Codex string }{
	Claude: "https://api.anthropic.com/api/oauth/usage",
	Codex:  "https://chatgpt.com/backend-api/wham/usage",
}

// quotaRoute 给出账号类型的请求应走的本机代理入口（空表示直连，只在受信网络上）；
// 保护服务没有放通时返回错误，这时不能发请求。
type quotaRoute func(kind string) (proxy string, err error)

type aiQuota struct {
	route quotaRoute

	mu      sync.Mutex
	tokens  map[string]WorkspaceToken
	results map[string]*AIQuota
	paused  map[string]string // 按类型：暂停查询的原因
	kick    chan struct{}
}

func newAIQuota(route quotaRoute) *aiQuota {
	return &aiQuota{route: route, tokens: map[string]WorkspaceToken{}, results: map[string]*AIQuota{}, paused: map[string]string{},
		kick: make(chan struct{}, 1)}
}

// Paused 返回账号类型暂停查询额度的原因；正常查询时为空。
func (q *aiQuota) Paused(kind string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused[kind]
}

func quotaKey(kind, email string) string { return kind + "\x00" + email }

// SetTokens 换成外壳最新读到的令牌；新出现或变了的令牌立即查一次。
func (q *aiQuota) SetTokens(list []WorkspaceToken) {
	q.mu.Lock()
	changed := false
	next := map[string]WorkspaceToken{}
	for _, t := range list {
		k := quotaKey(t.Kind, t.Email)
		if old, ok := q.tokens[k]; !ok || old.Token != t.Token {
			changed = true
		}
		next[k] = t
	}
	for k := range q.results {
		if _, ok := next[k]; !ok {
			delete(q.results, k)
		}
	}
	q.tokens = next
	q.mu.Unlock()
	if changed {
		select {
		case q.kick <- struct{}{}:
		default:
		}
	}
}

// Get 返回账号（email 为空表示默认目录）最近一次的额度。
func (q *aiQuota) Get(kind, email string) *AIQuota {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r := q.results[quotaKey(kind, email)]; r != nil {
		c := *r
		return &c
	}
	return nil
}

// Run 定时查询额度，直到 ctx 结束。
func (q *aiQuota) Run(ctx context.Context) {
	for {
		q.mu.Lock()
		tokens := make([]WorkspaceToken, 0, len(q.tokens))
		for _, t := range q.tokens {
			tokens = append(tokens, t)
		}
		q.mu.Unlock()
		paused := map[string]string{}
		for _, kind := range []string{dist.KindClaude, dist.KindCodex} {
			if _, err := q.proxy(kind); err != nil {
				paused[kind] = err.Error()
			}
		}
		for _, t := range tokens {
			if paused[t.Kind] != "" {
				continue // 没放通：保留上一次的结果，不发请求
			}
			r := q.fetch(ctx, t)
			q.mu.Lock()
			if cur, ok := q.tokens[quotaKey(t.Kind, t.Email)]; ok && cur.Token == t.Token {
				q.results[quotaKey(t.Kind, t.Email)] = &r
			}
			q.mu.Unlock()
		}
		q.mu.Lock()
		q.paused = paused
		q.mu.Unlock()
		wait := aiQuotaEvery
		if len(paused) > 0 {
			wait = aiQuotaPausedRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-q.kick:
		}
	}
}

func (q *aiQuota) proxy(kind string) (string, error) {
	if q.route == nil {
		return "", nil
	}
	return q.route(kind)
}

// client 返回经账号类型的严格组出去的 HTTP 客户端；保护服务没有放通时返回错误（额度、刷新令牌、鉴权测试都不发）。
func (q *aiQuota) client(kind string) (*http.Client, error) {
	p, err := q.proxy(kind)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // 不用系统代理设置：要么经严格组，要么在受信网络上直连
	if p != "" {
		u, err := url.Parse(p)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}, nil
}

func (q *aiQuota) fetch(ctx context.Context, t WorkspaceToken) AIQuota {
	out, _ := q.fetchStatus(ctx, t)
	return out
}

// fetchStatus 查一次额度，同时返回 HTTP 状态码（连不上时为 0），鉴权测试据此决定要不要刷新令牌。
func (q *aiQuota) fetchStatus(ctx context.Context, t WorkspaceToken) (AIQuota, int) {
	out := AIQuota{CheckedAt: time.Now(), Windows: []AIQuotaWindow{}}
	endpoint := aiQuotaEndpoints.Claude
	if t.Kind == dist.KindCodex {
		endpoint = aiQuotaEndpoints.Codex
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		out.Error = err.Error()
		return out, 0
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	if t.Kind == dist.KindCodex {
		if t.AccountID != "" {
			req.Header.Set("ChatGPT-Account-Id", t.AccountID)
		}
	} else {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
	hc, err := q.client(t.Kind)
	if err != nil {
		out.Error = "现在不能查询额度：" + err.Error()
		return out, 0
	}
	resp, err := hc.Do(req)
	if err != nil {
		out.Error = "查询额度失败：" + err.Error()
		return out, 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		cmd := map[string]string{dist.KindClaude: "claude", dist.KindCodex: "codex"}[t.Kind]
		out.Error = "登录已过期：在这个账号的终端里运行一次 " + cmd + " 刷新登录"
		return out, resp.StatusCode
	case resp.StatusCode != http.StatusOK:
		out.Error = fmt.Sprintf("查询额度失败（HTTP %d）", resp.StatusCode)
		return out, resp.StatusCode
	}
	if t.Kind == dist.KindCodex {
		out.Windows, out.Plan, err = parseCodexQuota(body)
	} else {
		out.Windows, err = parseClaudeQuota(body)
	}
	if err != nil {
		out.Error = "额度格式不认识：" + err.Error()
	}
	return out, resp.StatusCode
}

var claudeWindowLabels = map[string]string{
	"five_hour":        "5 小时",
	"seven_day":        "每周",
	"seven_day_opus":   "每周 Opus",
	"seven_day_sonnet": "每周 Sonnet",
}

// parseClaudeQuota 读出每个带 utilization 的窗口。TODO(verify:V-claude-quota) 非公开接口的格式。
func parseClaudeQuota(body []byte) ([]AIQuotaWindow, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := []AIQuotaWindow{}
	for k, v := range raw {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(v, &w) != nil || w.Utilization == nil {
			continue
		}
		label := claudeWindowLabels[k]
		if label == "" {
			label = k
		}
		out = append(out, AIQuotaWindow{Key: k, Label: label, UsedPercent: *w.Utilization, ResetAt: w.ResetsAt})
	}
	sort.Slice(out, func(i, j int) bool { return windowOrder(out[i].Key) < windowOrder(out[j].Key) })
	return out, nil
}

func windowOrder(k string) string {
	switch k {
	case "five_hour", "primary":
		return "0"
	case "seven_day", "secondary":
		return "1"
	}
	return "2" + k
}

// parseCodexQuota 读出 5 小时（primary）和每周（secondary）窗口。TODO(verify:V-codex-quota)
func parseCodexQuota(body []byte) ([]AIQuotaWindow, string, error) {
	var raw struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", err
	}
	out := []AIQuotaWindow{}
	for _, x := range []struct {
		key string
		w   *codexWindow
	}{{"primary", raw.RateLimit.Primary}, {"secondary", raw.RateLimit.Secondary}} {
		if x.w == nil {
			continue
		}
		out = append(out, AIQuotaWindow{Key: x.key, Label: x.w.label(x.key), UsedPercent: x.w.UsedPercent, ResetAt: x.w.resetAt()})
	}
	return out, raw.PlanType, nil
}

type codexWindow struct {
	UsedPercent  float64         `json:"used_percent"`
	WindowSecs   int64           `json:"limit_window_seconds"`
	ResetAt      json.RawMessage `json:"reset_at"`
	ResetAfterSc int64           `json:"reset_after_seconds"`
}

func (w codexWindow) label(key string) string {
	switch {
	case w.WindowSecs == 5*3600:
		return "5 小时"
	case w.WindowSecs == 7*24*3600:
		return "每周"
	case w.WindowSecs > 0 && w.WindowSecs%3600 == 0:
		return strconv.FormatInt(w.WindowSecs/3600, 10) + " 小时"
	case key == "primary":
		return "5 小时"
	}
	return "每周"
}

// resetAt 兼容 Unix 秒和 RFC 3339 两种写法，转成 RFC 3339。
func (w codexWindow) resetAt() string {
	s := strings.Trim(string(w.ResetAt), `"`)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0).UTC().Format(time.RFC3339)
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return s
	}
	if w.ResetAfterSc > 0 {
		return time.Now().Add(time.Duration(w.ResetAfterSc) * time.Second).UTC().Format(time.RFC3339)
	}
	return ""
}
