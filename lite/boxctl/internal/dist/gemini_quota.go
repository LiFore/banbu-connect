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
	"runtime"
	"sort"
	"strings"
	"time"
)

// ---- Gemini 额度采集与上报（BXC-F0-022）----
//
// 本机服务每 5 分钟给分配到本机、已有凭据的每个账号查一次额度：用 refresh token 换访问令牌（只在内存里），
// 调 Cloud Code 内部接口 loadCodeAssist 取 project 和套餐，再 retrieveUserQuotaSummary 取 5 小时 / 每周窗口的剩余比例和重置时间，
// 拿不到时退回 fetchAvailableModels 的逐模型额度（cockpit-tools 读出的做法，TODO(verify:V-agy-quota) 未在真机确认）。
// 结果在概览显示，并上报管理服务用于轮换（BXC-F0-023）。
// 每一项是一个 bucket：优先是窗口（claude:5h、gemini:weekly 等），拿不到窗口时退回逐模型额度。
// 请求经 Gemini 的严格组出去，和登录、各客户端的出口一致。

const (
	geminiQuotaEvery = 5 * time.Minute
	// 访问令牌还剩这么久就换新的。
	geminiTokenMargin = 5 * time.Minute
	// Google 访问令牌的最长有效期（expires_in 3599 秒）再加点余量：存的到期时间比这还远就不可信。
	geminiAccessMaxLife = 65 * time.Minute
)

// Antigravity 的 User-Agent（cockpit-tools 读出），Cloud Code 内部接口按它识别客户端。TODO(verify:V-agy-quota)
var (
	geminiModelsUA = fmt.Sprintf("antigravity/2.25.0 (aidev_client; os_type=%s; arch=%s; cl=1003475615)", runtime.GOOS, runtime.GOARCH)
	geminiAssistUA = fmt.Sprintf("antigravity/2.25.0 %s/%s google-api-nodejs-client/10.3.0", runtime.GOOS, runtime.GOARCH)
)

// GeminiQuotaBucket 是额度的一项（一个模型）。
type GeminiQuotaBucket struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	Remaining float64 `json:"remaining"` // 0~1
	ResetAt   string  `json:"reset_at,omitempty"`
}

// GeminiQuota 是一个账号一次采到的额度。
type GeminiQuota struct {
	Buckets   []GeminiQuotaBucket `json:"buckets"`
	Error     string              `json:"error,omitempty"`
	SampledAt time.Time           `json:"sampled_at"`
}

// MinRemaining 是各项里最少的剩余比例；没有数据时为 -1。
func (q GeminiQuota) MinRemaining() float64 {
	m := -1.0
	for _, b := range q.Buckets {
		if m < 0 || b.Remaining < m {
			m = b.Remaining
		}
	}
	return m
}

type cachedAccess struct {
	token   string
	expiry  time.Time
	refresh string // 换这个令牌用的 refresh token：凭据换了就作废
	idToken string // 刷新时 Google 一并返回的 id_token（有 openid scope 时），写进本机凭据
}

// RunGeminiQuota 定期采集额度，直到 ctx 结束。保护服务没有放通时暂停，每 30 秒再看一次。
func (c *Client) RunGeminiQuota(ctx context.Context) {
	for {
		wait := geminiQuotaEvery
		if !c.collectGeminiQuota(ctx) {
			wait = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-c.gemQuotaKick:
		}
	}
}

// KickGeminiQuota 让额度立即重新采集一次（页面上点「刷新额度」）。
func (c *Client) KickGeminiQuota() {
	select {
	case c.gemQuotaKick <- struct{}{}:
	default:
	}
}

// GeminiQuotaSample 是上报给管理服务的一个账号的额度。
type GeminiQuotaSample struct {
	AccountID int64               `json:"account_id"`
	Buckets   []GeminiQuotaBucket `json:"buckets"`
}

// collectGeminiQuota 采集一次；保护服务没有放通、没有发请求时返回 false。
func (c *Client) collectGeminiQuota(ctx context.Context) bool {
	c.mu.Lock()
	var accounts []GeminiSyncAccount
	for _, a := range c.gem.sync.Accounts {
		if len(a.Credential) > 0 {
			accounts = append(accounts, a)
		}
	}
	oauth := c.gem.sync.OAuth
	c.mu.Unlock()
	if len(accounts) == 0 || oauth == nil {
		return true
	}
	var samples []GeminiQuotaSample
	for _, a := range accounts {
		q := GeminiQuota{SampledAt: time.Now(), Buckets: []GeminiQuotaBucket{}}
		buckets, err := c.fetchGeminiQuota(ctx, a, *oauth)
		if err != nil {
			q.Error = err.Error()
		} else {
			q.Buckets = buckets
			samples = append(samples, GeminiQuotaSample{AccountID: a.ID, Buckets: buckets})
		}
		c.mu.Lock()
		if c.gem.quota == nil {
			c.gem.quota = map[int64]GeminiQuota{}
		}
		c.gem.quota[a.ID] = q
		c.mu.Unlock()
	}
	if len(samples) > 0 {
		if err := c.doJSON(ctx, http.MethodPost, "/api/v1/client/gemini/quota", map[string]any{"samples": samples}, nil, requestTimeout); err != nil {
			c.log.Warn("上报 Gemini 额度失败", "err", err)
		}
	}
	c.autoRotateGemini(ctx)
	return true
}

// storedAccessUsable 判断凭据里存的访问令牌能不能直接用：没到期，且到期时间可信。
// Google 的访问令牌最长一小时；到期时间比这还远的（外面粘贴来的凭据常见，时区写错或随手填的），
// 说明不可信，一律重新换，否则会一直拿着失效的令牌去请求（401 invalid authentication credentials）。
func storedAccessUsable(info GeminiCredentialInfo, now time.Time) bool {
	return info.AccessToken != "" && info.Expiry.After(now.Add(geminiTokenMargin)) && info.Expiry.Before(now.Add(geminiAccessMaxLife))
}

// geminiAccessToken 返回账号的访问令牌：先用内存里刷新过的，再用凭据里存的，都不行就用 refresh token 换新的（只放在内存里）。
// force 为 true 时不信任已有的，直接换新的（Cloud Code 返回 401 后用）。
// Google 返回了新的 refresh token 时上传新凭据（TODO(verify:V-agy-refresh) 一般不会轮换）。
func (c *Client) geminiAccessToken(ctx context.Context, a GeminiSyncAccount, oauth GeminiOAuth, force bool) (string, error) {
	info, err := ParseGeminiCredential(a.Credential)
	if err != nil {
		return "", err
	}
	now := time.Now()
	c.mu.Lock()
	cached, ok := c.gemTokens[a.ID]
	c.mu.Unlock()
	if !force {
		// 内存里的是我们自己刷新得到的，比凭据里存的新，优先用。
		if ok && cached.refresh == info.RefreshToken && cached.expiry.After(now.Add(geminiTokenMargin)) {
			return cached.token, nil
		}
		if storedAccessUsable(info, now) {
			return info.AccessToken, nil
		}
	}
	t, err := c.googleToken(ctx, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {info.RefreshToken},
		"client_id": {oauth.ClientID}, "client_secret": {oauth.ClientSecret},
	})
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.gemTokens == nil {
		c.gemTokens = map[int64]cachedAccess{}
	}
	c.gemTokens[a.ID] = cachedAccess{token: t.AccessToken, expiry: now.Add(time.Duration(t.ExpiresIn) * time.Second), refresh: info.RefreshToken, idToken: t.IDToken}
	c.mu.Unlock()
	if t.RefreshToken != "" && t.RefreshToken != info.RefreshToken {
		if t.IDToken == "" {
			t.IDToken = idTokenFrom(a.Credential)
		}
		cred := geminiCredentialJSON(t, t.RefreshToken, now)
		var resp struct {
			Version int64 `json:"version"`
		}
		if err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/v1/client/gemini/accounts/%d/credential", a.ID),
			CredentialUpload{BaseVersion: a.Version, Source: "refresh", Credential: cred}, &resp, requestTimeout); err != nil {
			c.log.Warn("上传轮换后的 Gemini 凭据失败", "account", a.Email, "err", err)
		}
	}
	return t.AccessToken, nil
}

// freshGeminiCredential 返回要写进本机的凭据：存的访问令牌还有效时原样返回；否则用 refresh token 换新的，
// 拼成带有效 access_token、expiry 的凭据（参考 cockpit-tools 切号前的 ensure_fresh_token）。
// 管理员只填了 refresh token、或凭据放久了时，原样写进去 Antigravity 应用会显示未登录。
// 换到的令牌缓存到快过期，同一账号反复调用得到同一份凭据（外壳按指纹判断是否要重写）。
func (c *Client) freshGeminiCredential(ctx context.Context, a GeminiSyncAccount, oauth GeminiOAuth) ([]byte, error) {
	info, err := ParseGeminiCredential(a.Credential)
	if err != nil {
		return nil, err
	}
	token, err := c.geminiAccessToken(ctx, a, oauth, false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cached, ok := c.gemTokens[a.ID]
	c.mu.Unlock()
	if !ok || cached.refresh != info.RefreshToken || cached.token != token {
		if token == info.AccessToken {
			return a.Credential, nil // 用的就是凭据里存的，原样写入
		}
		return nil, errors.New("刷新后找不到访问令牌")
	}
	idToken := cached.idToken
	if idToken == "" {
		idToken = idTokenFrom(a.Credential)
	}
	email := idTokenEmail(idToken)
	if email == "" {
		email = strings.ToLower(a.Email)
	}
	b, _ := json.Marshal(map[string]any{
		"token": map[string]any{
			"access_token":  cached.token,
			"token_type":    "Bearer",
			"refresh_token": info.RefreshToken,
			"expiry":        cached.expiry.UTC().Format("2006-01-02T15:04:05.000000Z"),
		},
		"auth_method":    "consumer",
		"id_token":       idToken,
		"id_token_email": email, // Antigravity 2.3x 自己写的凭据里有这个字段（本机钥匙串实测，2026-11-23）
	})
	return b, nil
}

func idTokenFrom(raw []byte) string {
	var c struct {
		IDToken string `json:"id_token"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.IDToken
}

// cloudCode 调 Cloud Code 内部接口。
func (c *Client) cloudCode(ctx context.Context, method, token, ua string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiEndpoints.CloudCode+"/v1internal:"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	resp, err := c.googleHTTP(30 * time.Second).Do(req)
	if err != nil {
		return fmt.Errorf("连接 Google 失败：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &cloudCodeError{Method: method, Status: resp.StatusCode, Message: e.Error.Message}
	}
	return json.Unmarshal(data, out)
}

// cloudCodeError 是 Cloud Code 接口返回的非 200。
type cloudCodeError struct {
	Method  string
	Status  int
	Message string
}

func (e *cloudCodeError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return fmt.Sprintf("%s 失败：访问令牌无效（HTTP 401），刷新后仍被拒绝，请在 Antigravity 里重新登录这个账号", e.Method)
	}
	return fmt.Sprintf("%s 失败（HTTP %d %s）", e.Method, e.Status, e.Message)
}

func isCloudCode401(err error) bool {
	var ce *cloudCodeError
	return errors.As(err, &ce) && ce.Status == http.StatusUnauthorized
}

func (c *Client) fetchGeminiQuota(ctx context.Context, a GeminiSyncAccount, oauth GeminiOAuth) ([]GeminiQuotaBucket, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	token, err := c.geminiAccessToken(ctx, a, oauth, false)
	if err != nil {
		return nil, err
	}
	buckets, err := c.fetchGeminiQuotaWith(ctx, token)
	if !isCloudCode401(err) {
		return buckets, err
	}
	// 401：手里的访问令牌失效了（存的到期时间不准、被撤销等）。用 refresh token 换新的再试一次，
	// 换到的放进内存，之后都用它，不再用凭据里那个。
	if token, err = c.geminiAccessToken(ctx, a, oauth, true); err != nil {
		return nil, err
	}
	return c.fetchGeminiQuotaWith(ctx, token)
}

// geminiTier 是 loadCodeAssist 里的套餐，id 如 free-tier、g1-pro-tier、g1-ultra-tier。
type geminiTier struct {
	ID string `json:"id"`
}

func (c *Client) fetchGeminiQuotaWith(ctx context.Context, token string) ([]GeminiQuotaBucket, error) {
	var assist struct {
		Project     json.RawMessage `json:"cloudaicompanionProject"`
		CurrentTier *geminiTier     `json:"currentTier"`
		PaidTier    *geminiTier     `json:"paidTier"`
	}
	if err := c.cloudCode(ctx, "loadCodeAssist", token, geminiAssistUA,
		map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY", "platform": "PLATFORM_UNSPECIFIED", "pluginType": "GEMINI"}}, &assist); err != nil {
		return nil, err
	}
	project := parseProject(assist.Project)
	if project == "" {
		return nil, errors.New("这个账号还没有开通 Antigravity：先在 Antigravity 应用里用它登录一次")
	}
	tier := ""
	for _, t := range []*geminiTier{assist.PaidTier, assist.CurrentTier} {
		if t != nil && t.ID != "" {
			tier = t.ID
			break
		}
	}
	// 和 cockpit-tools 一致：额度以 retrieveUserQuotaSummary 的时间窗口为准（Claude / Gemini 各有 5 小时、每周），
	// 拿不到时才退回 fetchAvailableModels 的逐模型额度。
	var summary json.RawMessage
	if err := c.cloudCode(ctx, "retrieveUserQuotaSummary", token, geminiModelsUA, map[string]string{"project": project}, &summary); err == nil {
		if b := parseQuotaSummary(summary, isFreeTier(tier)); len(b) > 0 {
			return b, nil
		}
	} else if isCloudCode401(err) {
		return nil, err
	} else {
		c.log.Debug("retrieveUserQuotaSummary 失败，改用逐模型额度", "err", err)
	}
	var models struct {
		Models json.RawMessage `json:"models"`
	}
	if err := c.cloudCode(ctx, "fetchAvailableModels", token, geminiModelsUA, map[string]string{"project": project}, &models); err != nil {
		return nil, err
	}
	return parseQuotaModels(models.Models), nil
}

// isFreeTier 和 cockpit-tools 的 getSubscriptionTier 一致：含 ultra / pro 的是付费，其余（有值时）算免费。
func isFreeTier(id string) bool {
	id = strings.ToLower(id)
	return id != "" && !strings.Contains(id, "ultra") && !strings.Contains(id, "pro")
}

// quotaWindow 是一个额度窗口的固定名称和排序（cockpit-tools 的 getAntigravityQuotaDisplayItems）。
type quotaWindow struct {
	key, label string
	rank       int
}

// summaryWindow 把 retrieveUserQuotaSummary 的 bucketId 归到固定的窗口：
// 3p-5h / 3p-weekly 是 Claude 等第三方模型，gemini-5h / gemini-weekly 是共享的 Gemini 额度，
// gemini-argon-*-weekly、gemini-carbon-wtu-weekly 是 Gemini 4 Argon / Carbon 单独的每周额度。
// 也接受 claude:5h 这种写法。认不出的返回 false，由调用方按接口给的名称显示。
func summaryWindow(id string) (quotaWindow, bool) {
	switch {
	case id == "3p-5h" || id == "claude:5h":
		return quotaWindow{"claude:5h", "Claude（5 小时）", 0}, true
	case id == "3p-weekly" || id == "claude:weekly":
		return quotaWindow{"claude:weekly", "Claude（每周）", 1}, true
	case id == "gemini-5h" || id == "gemini:5h":
		return quotaWindow{"gemini:5h", "Gemini（5 小时）", 2}, true
	case id == "gemini-weekly" || id == "gemini:weekly":
		return quotaWindow{"gemini:weekly", "Gemini（每周）", 3}, true
	case id == "argon:weekly" || (strings.HasPrefix(id, "gemini-argon-") && strings.HasSuffix(id, "-weekly")):
		return quotaWindow{"argon:weekly", "Gemini 4 Argon（每周）", 4}, true
	case id == "carbon:weekly" || id == "gemini-carbon-wtu-weekly":
		return quotaWindow{"carbon:weekly", "Gemini 4 Carbon（每周）", 5}, true
	}
	return quotaWindow{}, false
}

// parseQuotaSummary 读 retrieveUserQuotaSummary：{"groups":[{"displayName","buckets":[{"bucketId","window","remainingFraction","resetTime"}]}]}。
// 免费账号只有每周额度，5 小时窗口不显示（和 cockpit-tools 一致）。
func parseQuotaSummary(raw json.RawMessage, free bool) []GeminiQuotaBucket {
	var s struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				BucketID          string   `json:"bucketId"`
				DisplayName       string   `json:"displayName"`
				Window            string   `json:"window"`
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	type ranked struct {
		b    GeminiQuotaBucket
		rank int
	}
	var list []ranked
	seen := map[string]bool{}
	for _, g := range s.Groups {
		for _, b := range g.Buckets {
			id := strings.TrimSpace(b.BucketID)
			if id == "" {
				continue
			}
			w, ok := summaryWindow(id)
			if !ok {
				period := b.Window
				if period == "" && strings.HasSuffix(id, "weekly") {
					period = "weekly"
				} else if period == "" && strings.HasSuffix(id, "5h") {
					period = "5h"
				}
				name := strings.TrimSpace(g.DisplayName)
				if name == "" {
					name = strings.TrimSpace(b.DisplayName)
				}
				if name == "" {
					name = id
				}
				w = quotaWindow{id, name + periodLabel(period), 100}
			}
			if seen[w.key] || (free && strings.HasSuffix(w.key, "5h")) {
				continue
			}
			seen[w.key] = true
			// remainingFraction 为 0 时 protobuf 转 JSON 会省略。
			rem := 0.0
			if b.RemainingFraction != nil {
				rem = min(max(*b.RemainingFraction, 0), 1)
			}
			list = append(list, ranked{GeminiQuotaBucket{Key: w.key, Label: w.label, Remaining: rem, ResetAt: b.ResetTime}, w.rank})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].rank != list[j].rank {
			return list[i].rank < list[j].rank
		}
		return list[i].b.Key < list[j].b.Key
	})
	out := make([]GeminiQuotaBucket, len(list))
	for i, r := range list {
		out[i] = r.b
	}
	return out
}

// isQuotaWindow 判断 bucket 是时间窗口（retrieveUserQuotaSummary）还是逐模型额度（fetchAvailableModels，旧版客户端
// 或拿不到窗口时）。认得的窗口 key 见 summaryWindow；认不出的窗口用接口的 bucketId，以 5h / weekly 结尾，模型名不会这样结尾。
func isQuotaWindow(key string) bool {
	return strings.HasSuffix(key, "5h") || strings.HasSuffix(key, "weekly")
}

// quotaRank 是 bucket 的显示顺序：认得的窗口按 summaryWindow 的顺序，其他窗口其次，逐模型最后。
func quotaRank(key string) int {
	for _, id := range []string{"claude:5h", "claude:weekly", "gemini:5h", "gemini:weekly", "argon:weekly", "carbon:weekly"} {
		if key == id {
			w, _ := summaryWindow(id)
			return w.rank
		}
	}
	if isQuotaWindow(key) {
		return 100
	}
	return 200
}

func periodLabel(p string) string {
	switch p {
	case "5h":
		return "（5 小时）"
	case "weekly":
		return "（每周）"
	case "":
		return ""
	}
	return "（" + p + "）"
}

// parseProject 兼容字符串和 {"id": …} 两种写法。
func parseProject(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.ID
}

type quotaModel struct {
	Name        string `json:"name"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	QuotaInfo   *struct {
		RemainingFraction *float64 `json:"remainingFraction"`
		ResetTime         string   `json:"resetTime"`
	} `json:"quotaInfo"`
}

// parseQuotaModels 读出每个模型的额度：models 可能是以模型名为键的对象，也可能是数组。
// 只取带 quotaInfo 的；protobuf 转 JSON 时 0 会被省略，所以有 quotaInfo 而没有 remainingFraction 表示用完了。
func parseQuotaModels(raw json.RawMessage) []GeminiQuotaBucket {
	all := map[string]quotaModel{}
	if json.Unmarshal(raw, &all) != nil {
		var list []quotaModel
		_ = json.Unmarshal(raw, &list)
		for _, m := range list {
			key := m.Model
			if key == "" {
				key = m.Name
			}
			all[key] = m
		}
	}
	out := []GeminiQuotaBucket{}
	for key, m := range all {
		if m.QuotaInfo == nil || key == "" {
			continue
		}
		b := GeminiQuotaBucket{Key: key, Label: m.DisplayName, ResetAt: m.QuotaInfo.ResetTime}
		if b.Label == "" {
			b.Label = key
		}
		if f := m.QuotaInfo.RemainingFraction; f != nil {
			b.Remaining = min(max(*f, 0), 1)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// autoRotateGemini：当前账号最紧的一项低于阈值时，按管理服务给的建议顺序换成第一个额度还够的账号（BXC-F0-023）。
// 采集失败的账号不算耗尽，也不当作可用的去处（不知道额度）。
func (c *Client) autoRotateGemini(ctx context.Context) {
	c.mu.Lock()
	active := c.gem.saved.Active
	cur, ok := c.gem.quota[active]
	if active == 0 || !ok || cur.Error != "" || cur.MinRemaining() < 0 || cur.MinRemaining() > GeminiExhausted {
		c.mu.Unlock()
		return
	}
	next := int64(0)
	for _, id := range c.gem.sync.Order {
		q, ok := c.gem.quota[id]
		if id != active && hasCredential(c.gem.sync, id) && ok && q.Error == "" && q.MinRemaining() > GeminiExhausted {
			next = id
			break
		}
	}
	if next == 0 {
		c.mu.Unlock()
		return
	}
	from, to := findGemini(c.gem.sync, active).Email, findGemini(c.gem.sync, next).Email
	c.gem.saved.Active = next
	c.gem.rotated = fmt.Sprintf("%s 的额度将尽，已自动切换到 %s（%s）", from, to, time.Now().Format("01-02 15:04"))
	c.deferSwitchLocked(fmt.Sprintf("%s 的额度将尽，自动切换到 %s", from, to))
	c.saveGeminiLocked()
	c.mu.Unlock()
	c.log.Info("Gemini 额度将尽，自动切换账号", "from", from, "to", to)
	c.reportGeminiState(ctx, GeminiStateReport{ActiveAccountID: next})
}
