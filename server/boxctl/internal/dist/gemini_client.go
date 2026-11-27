package dist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"
)

// ---- 客户端：同步 Gemini 账号与凭据（BXC-F0-018）----
//
// 本机服务长轮询管理服务，拿到分配给本机的账号、凭据、建议顺序和切换指令，决定本机当前用哪个账号。
// 凭据要写进当前用户的 ~/.gemini 和钥匙串，服务以 SYSTEM/root 运行做不了：托盘外壳按状态里的
// gemini.apply 取当前账号的凭据写入（BXC-F0-021），并定期读回本机 agy / 应用刷新后的凭据交给服务上传。
// 当前账号不再分配给本机（或解绑）时 apply 变为 0:0，外壳删除本机凭据。

const (
	geminiSyncWait  = 25 * time.Second
	geminiStateFile = "gemini.json"
)

// geminiSaved 是要跨重启保留的本机选择。
type geminiSaved struct {
	Active        int64  `json:"active_account_id"`
	HandledSwitch string `json:"handled_switch"`
}

// GeminiApplied 是外壳回报的写入结果。
type GeminiApplied struct {
	AccountID int64     `json:"account_id"`
	Version   int64     `json:"version"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

type geminiState struct {
	loaded   bool
	synced   bool // 本次启动后同步成功过；之前不知道该持有什么，外壳什么都不做
	sync     GeminiSync
	lastSync time.Time
	lastErr  string
	saved    geminiSaved
	reported bool // 启动后向管理服务回报过当前账号
	applied  *GeminiApplied
	quota    map[int64]GeminiQuota // 本机采到的额度（BXC-F0-022）
	login    *geminiLogin          // 本机最近一次登录（BXC-F0-020）
	rotated  string                // 最近一次自动切换（BXC-F0-023），页面显示
	// quotaPaused 是额度查询暂停的原因（保护服务没有放通）；为空表示正常查询。
	quotaPaused string
	launch      *geminiLaunch // 最近一次「启动」
	// pending 是延后执行的切换：自动轮换、管理员下发的切换不马上关 Antigravity，先通知用户，宽限期后再切。
	pending *geminiPending
}

// geminiSwitchGrace 是自动轮换、管理员下发的切换在 Antigravity 运行时的宽限期：外壳先发通知，到点再强制关闭应用、写入凭据。
// 用户在本机页面上点的切换不等。
const geminiSwitchGrace = time.Minute

// geminiPending 是一次延后执行的切换。
type geminiPending struct {
	AccountID int64     `json:"account_id"` // 要切到的账号；0 表示收回本机凭据
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason"`
}

// deferSwitchLocked 记下一次非用户发起的切换：宽限期内外壳不关正在运行的 Antigravity。
func (c *Client) deferSwitchLocked(reason string) {
	c.gem.pending = &geminiPending{AccountID: c.gem.saved.Active, Until: time.Now().Add(geminiSwitchGrace), Reason: reason}
}

// pendingLocked 返回还在宽限期内、且仍指向当前账号的延后切换。
func (c *Client) pendingLocked() *geminiPending {
	p := c.gem.pending
	if p == nil || p.AccountID != c.gem.saved.Active || !time.Now().Before(p.Until) {
		return nil
	}
	cp := *p
	return &cp
}

// ApplyGeminiNow 结束宽限期，让外壳马上切换（页面上点「立即切换」）。
func (c *Client) ApplyGeminiNow() {
	c.mu.Lock()
	c.gem.pending = nil
	c.mu.Unlock()
}

// geminiLaunch 是一次「启动」：切到这个账号，外壳把凭据写进应用后启动 Antigravity。
type geminiLaunch struct {
	ID        string    `json:"id"`
	AccountID int64     `json:"account_id"`
	Email     string    `json:"email"`
	At        time.Time `json:"at"`
	Done      bool      `json:"done"`
	Error     string    `json:"error,omitempty"`
}

// 外壳在这段时间内没有认领的启动请求作废（例如外壳重启后不再执行旧请求）。
const geminiLaunchClaim = 2 * time.Minute

// LocalGeminiAccount 是页面上显示的账号。
type LocalGeminiAccount struct {
	ID            int64        `json:"id"`
	Email         string       `json:"email"`
	Note          string       `json:"note"`
	HasCredential bool         `json:"has_credential"`
	Version       int64        `json:"version"`
	Quota         *GeminiQuota `json:"quota,omitempty"`
}

// GeminiStatus 是页面用的 Gemini 状态。
type GeminiStatus struct {
	Bound     bool                 `json:"bound"`
	Synced    bool                 `json:"synced"`
	LastSync  *time.Time           `json:"last_sync,omitempty"`
	LastError string               `json:"last_error"`
	Accounts  []LocalGeminiAccount `json:"accounts"`
	ActiveID  int64                `json:"active_account_id"`
	Order     []int64              `json:"order"`
	Applied   *GeminiApplied       `json:"applied,omitempty"`
	Login     *LocalGeminiLogin    `json:"login,omitempty"`
	Rotated   string               `json:"rotated,omitempty"` // 最近一次自动切换
	// QuotaPaused 是额度查询暂停的原因：保护服务没有放通时不查（不直连出去）。
	QuotaPaused string        `json:"quota_paused,omitempty"`
	Launch      *geminiLaunch `json:"launch,omitempty"`
	// Pending 是等待中的切换（自动轮换、管理员下发）：Antigravity 在运行时宽限期结束才关闭应用切过去。
	Pending *geminiPending `json:"pending,omitempty"`
}

// GeminiApply 是外壳要写进本机的凭据；AccountID 为 0 表示删除本机凭据。
type GeminiApply struct {
	AccountID  int64           `json:"account_id"`
	Email      string          `json:"email"`
	Version    int64           `json:"version"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

// loadGeminiLocked 读取保存的本机选择。调用方持有 c.mu。
func (c *Client) loadGeminiLocked() {
	if c.gem.loaded {
		return
	}
	c.gem.loaded = true
	if err := readJSON(filepath.Join(c.dir, geminiStateFile), &c.gem.saved); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.log.Warn("读取 Gemini 本机选择失败", "err", err)
	}
}

func (c *Client) saveGeminiLocked() {
	if err := writeJSON0600(filepath.Join(c.dir, geminiStateFile), c.gem.saved); err != nil {
		c.log.Warn("保存 Gemini 本机选择失败", "err", err)
	}
}

func findGemini(s GeminiSync, id int64) *GeminiSyncAccount {
	for i := range s.Accounts {
		if s.Accounts[i].ID == id {
			return &s.Accounts[i]
		}
	}
	return nil
}

func hasCredential(s GeminiSync, id int64) bool {
	a := findGemini(s, id)
	return a != nil && len(a.Credential) > 0
}

// RunGemini 持续同步 Gemini 账号，直到 ctx 结束。没有绑定时清空并等待。
func (c *Client) RunGemini(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := c.binding0(); err != nil {
			c.resetGemini()
			sleepCtx(ctx, retryInterval)
			continue
		}
		if err := c.syncGemini(ctx, geminiSyncWait); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := retryInterval
			var he *HTTPError
			if errors.As(err, &he) && he.Status == http.StatusNotFound {
				wait = syncInterval // 管理服务还是旧版本，没有这个接口
			}
			sleepCtx(ctx, wait)
		}
	}
}

// syncGemini 同步一次：最多等 wait，管理服务那边有变化时立即返回。
func (c *Client) syncGemini(ctx context.Context, wait time.Duration) error {
	c.mu.Lock()
	c.loadGeminiLocked()
	since := c.gem.sync.Hash
	c.mu.Unlock()

	var out GeminiSync
	q := url.Values{"since": {since}, "wait": {strconv.Itoa(int(wait / time.Second))}}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/client/gemini/sync?"+q.Encode(), nil, &out, wait+20*time.Second); err != nil {
		c.mu.Lock()
		c.gem.lastErr = err.Error()
		c.mu.Unlock()
		return err
	}
	if report := c.applyGeminiSync(out); report != nil {
		c.reportGeminiState(ctx, *report)
	}
	c.runGeminiTests(ctx, out)
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// resetGemini 在解绑后清空：apply 变为 0:0，外壳删除本机凭据。
func (c *Client) resetGemini() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadGeminiLocked()
	if !c.gem.synced && c.gem.saved.Active == 0 && len(c.gem.sync.Accounts) == 0 {
		return
	}
	c.gem.sync = GeminiSync{}
	c.gem.synced, c.gem.reported, c.gem.lastErr = true, false, ""
	c.gem.quota = nil
	if c.gem.saved.Active != 0 {
		c.gem.saved.Active = 0
		c.saveGeminiLocked()
	}
}

// applyGeminiSync 保存同步结果，决定本机当前账号：先执行新的切换指令，当前账号不可用时换成建议顺序里的第一个。
// 需要向管理服务回报时返回回报内容。
func (c *Client) applyGeminiSync(out GeminiSync) *GeminiStateReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadGeminiLocked()
	c.gem.sync, c.gem.synced, c.gem.lastSync, c.gem.lastErr = out, true, time.Now(), ""
	prev := c.gem.saved
	var done string
	if sw := out.Switch; sw != nil && sw.ID != c.gem.saved.HandledSwitch {
		c.gem.saved.HandledSwitch, done = sw.ID, sw.ID
		if hasCredential(out, sw.AccountID) {
			c.gem.saved.Active = sw.AccountID
		}
	}
	if !hasCredential(out, c.gem.saved.Active) {
		c.gem.saved.Active = 0
		if len(out.Order) > 0 {
			c.gem.saved.Active = out.Order[0]
		}
	}
	// 管理员下发的切换、当前账号被收回：不是本机用户点的，正在用的 Antigravity 给宽限期。
	if c.gem.saved.Active != prev.Active && prev.Active != 0 {
		switch {
		case done != "":
			c.deferSwitchLocked("管理员切换了本机的 Gemini 账号")
		case c.gem.saved.Active == 0:
			c.deferSwitchLocked("本机的 Gemini 账号已被管理员收回")
		default:
			c.deferSwitchLocked("本机的 Gemini 账号已不可用，换成下一个账号")
		}
	}
	if c.gem.saved != prev {
		c.saveGeminiLocked()
	}
	if c.gem.saved.Active == prev.Active && done == "" && c.gem.reported {
		return nil
	}
	c.gem.reported = true
	return &GeminiStateReport{ActiveAccountID: c.gem.saved.Active, SwitchDone: done}
}

func (c *Client) reportGeminiState(ctx context.Context, r GeminiStateReport) {
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/client/gemini/state", r, nil, requestTimeout); err != nil {
		c.log.Warn("回报 Gemini 当前账号失败", "err", err)
		c.mu.Lock()
		c.gem.reported = false // 下次同步时再回报
		c.mu.Unlock()
	}
}

// GeminiStatus 返回页面用的状态。
func (c *Client) GeminiStatus() GeminiStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadGeminiLocked()
	st := GeminiStatus{Bound: c.binding != nil, Synced: c.gem.synced, LastError: c.gem.lastErr,
		Accounts: []LocalGeminiAccount{}, ActiveID: c.gem.saved.Active, Order: c.gem.sync.Order, Applied: c.gem.applied,
		Rotated: c.gem.rotated, QuotaPaused: c.gem.quotaPaused, Pending: c.pendingLocked()}
	if l := c.gem.launch; l != nil {
		cp := *l
		st.Launch = &cp
	}
	if st.Order == nil {
		st.Order = []int64{}
	}
	if !c.gem.lastSync.IsZero() {
		t := c.gem.lastSync
		st.LastSync = &t
	}
	for _, a := range c.gem.sync.Accounts {
		la := LocalGeminiAccount{ID: a.ID, Email: a.Email, Note: a.Note, HasCredential: len(a.Credential) > 0, Version: a.Version}
		if q, ok := c.gem.quota[a.ID]; ok {
			la.Quota = &q
		}
		st.Accounts = append(st.Accounts, la)
	}
	st.Login = c.geminiLoginLocked()
	return st
}

// GeminiBrief 放在状态里给托盘外壳（每 3 秒读一次）。
type GeminiBrief struct {
	// Apply 是"账号:版本"，变化时外壳取凭据写入；"0:0" 表示删除本机凭据；还没同步过时为空，外壳什么都不做。
	Apply string `json:"apply"`
	// Browser 是待外壳打开的浏览器档案（登录或查看档案，BXC-F0-019、BXC-F0-020）。
	Browser *PendingBrowser `json:"browser,omitempty"`
	// Launch 是待外壳执行的「启动」：等 Apply 写到 Launch 的账号后启动（或切到前台）Antigravity 应用。
	Launch *GeminiLaunchBrief `json:"launch,omitempty"`
	// Defer 是延后执行的切换：Antigravity 在运行时，外壳发一次通知，到 Until（Unix 秒）前不关应用、不写入。
	Defer *GeminiDeferBrief `json:"defer,omitempty"`
}

// GeminiDeferBrief 是给外壳的延后切换。
type GeminiDeferBrief struct {
	Until  int64  `json:"until"`
	Reason string `json:"reason"`
}

// GeminiLaunchBrief 是给外壳的启动请求。
type GeminiLaunchBrief struct {
	ID        string `json:"id"`
	AccountID int64  `json:"account_id"`
}

// GeminiBrief 返回给外壳的摘要。
func (c *Client) GeminiBrief() *GeminiBrief {
	b := &GeminiBrief{Apply: c.GeminiApplyRev(), Browser: c.PendingBrowser()}
	c.mu.Lock()
	if l := c.gem.launch; l != nil && !l.Done && time.Since(l.At) < geminiLaunchClaim {
		b.Launch = &GeminiLaunchBrief{ID: l.ID, AccountID: l.AccountID}
	}
	if p := c.pendingLocked(); p != nil {
		b.Defer = &GeminiDeferBrief{Until: p.Until.Unix(), Reason: p.Reason}
	}
	c.mu.Unlock()
	return b
}

// GeminiApplyRev 放在状态里给外壳："账号:版本"，变化时外壳取凭据写入；还没同步过时为空，外壳什么都不做。
func (c *Client) GeminiApplyRev() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.gem.synced {
		return ""
	}
	a := findGemini(c.gem.sync, c.gem.saved.Active)
	if a == nil || len(a.Credential) == 0 {
		return "0:0"
	}
	return fmt.Sprintf("%d:%d", a.ID, a.Version)
}

// GeminiApplyCredential 返回外壳要写入的凭据。还没同步过时返回错误。
func (c *Client) GeminiApplyCredential() (GeminiApply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.gem.synced {
		return GeminiApply{}, &InputError{"还没有和管理服务同步过 Gemini 账号"}
	}
	a := findGemini(c.gem.sync, c.gem.saved.Active)
	if a == nil || len(a.Credential) == 0 {
		return GeminiApply{}, nil
	}
	return GeminiApply{AccountID: a.ID, Email: a.Email, Version: a.Version, Credential: a.Credential}, nil
}

// GeminiApplyFresh 和 GeminiApplyCredential 一样，但访问令牌过期或缺失时先刷新（保护服务放通时才发请求，ADR-063）。
// 刷新失败时返回存的凭据，由 Antigravity 自己再刷新。
func (c *Client) GeminiApplyFresh(ctx context.Context) (GeminiApply, error) {
	ap, err := c.GeminiApplyCredential()
	if err != nil || ap.AccountID == 0 {
		return ap, err
	}
	c.mu.Lock()
	a := findGemini(c.gem.sync, ap.AccountID)
	var acc GeminiSyncAccount
	if a != nil {
		acc = *a
	}
	oauth := c.gem.sync.OAuth
	c.mu.Unlock()
	if a == nil || oauth == nil || c.GeminiAllowed() != nil {
		return ap, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cred, err := c.freshGeminiCredential(ctx, acc, *oauth)
	if err != nil {
		c.log.Warn("写入本机前刷新 Gemini 凭据失败，写入存的凭据", "account", acc.Email, "err", err)
		return ap, nil
	}
	ap.Credential = cred
	return ap, nil
}

// SetGeminiApplied 记录外壳的写入结果。
func (c *Client) SetGeminiApplied(r GeminiApplied) {
	r.At = time.Now()
	c.mu.Lock()
	c.gem.applied = &r
	c.mu.Unlock()
}

// SetGeminiActive 手动切换本机当前账号（页面上点「切换」）。
func (c *Client) SetGeminiActive(ctx context.Context, id int64) error {
	c.mu.Lock()
	c.loadGeminiLocked()
	if !hasCredential(c.gem.sync, id) {
		c.mu.Unlock()
		return &InputError{"这个账号还没有登录，不能切换"}
	}
	changed := c.gem.saved.Active != id
	c.gem.saved.Active = id
	c.gem.pending = nil // 用户自己点的切换马上执行
	if changed {
		c.saveGeminiLocked()
	}
	c.mu.Unlock()
	if changed {
		c.reportGeminiState(ctx, GeminiStateReport{ActiveAccountID: id})
	}
	return nil
}

// LaunchGemini 一键启动（参考 cockpit-tools 的「切换并启动」）：切到这个账号，外壳写好凭据后启动 Antigravity 应用。
// 保护服务没有放通时不启动。
func (c *Client) LaunchGemini(ctx context.Context, id int64) (*geminiLaunch, error) {
	if err := c.GeminiAllowed(); err != nil {
		return nil, &GateError{Reason: err.Error()}
	}
	if err := c.SetGeminiActive(ctx, id); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a := findGemini(c.gem.sync, id)
	if a == nil {
		return nil, ErrNotFound
	}
	c.gem.launch = &geminiLaunch{ID: strconv.FormatInt(time.Now().UnixNano(), 36), AccountID: id, Email: a.Email, At: time.Now()}
	cp := *c.gem.launch
	return &cp, nil
}

// FinishGeminiLaunch 记录外壳执行启动的结果。
func (c *Client) FinishGeminiLaunch(id, errMsg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.gem.launch
	if l == nil || l.ID != id {
		return ErrNotFound
	}
	l.Done, l.Error = true, errMsg
	return nil
}

// GateError 是保护服务没有放通，请求被拒绝。
type GateError struct{ Reason string }

func (e *GateError) Error() string { return e.Reason }

// UploadLocalGemini 接收外壳读回的本机凭据（agy / 应用自己刷新过）。和已同步的一样时什么都不做；
// 是同一账号的新凭据时按版本号上传，版本号过期（别处更新过）时放弃，等下次同步拿到最新的。
// 判断是同一账号：id_token 里的邮箱一致，或 refresh token 没变。
func (c *Client) UploadLocalGemini(ctx context.Context, accountID int64, raw []byte) (bool, error) {
	info, err := ParseGeminiCredential(raw)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	a := findGemini(c.gem.sync, accountID)
	if a == nil || accountID != c.gem.saved.Active || len(a.Credential) == 0 {
		c.mu.Unlock()
		return false, &InputError{"不是本机当前的 Gemini 账号"}
	}
	acc := *a
	c.mu.Unlock()

	old, _ := ParseGeminiCredential(acc.Credential)
	if info.RefreshToken == old.RefreshToken && info.AccessToken == old.AccessToken {
		return false, nil
	}
	// TODO(verify:V-agy-refresh) 没有 id_token、refresh token 又变了时无法确认是同一账号，不上传。
	if info.Email != acc.Email && (info.Email != "" || info.RefreshToken != old.RefreshToken) {
		return false, &InputError{"本机的 Antigravity 凭据不是 " + acc.Email + "，没有上传"}
	}
	if info.RefreshToken == old.RefreshToken && !info.Expiry.After(old.Expiry) {
		return false, nil // 比已同步的还旧
	}
	var resp struct {
		Version int64 `json:"version"`
	}
	err = c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/api/v1/client/gemini/accounts/%d/credential", accountID),
		CredentialUpload{BaseVersion: acc.Version, Source: "refresh", Credential: raw}, &resp, requestTimeout)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	if a := findGemini(c.gem.sync, accountID); a != nil && a.Version == acc.Version {
		a.Credential, a.Version = append([]byte(nil), raw...), resp.Version
	}
	c.mu.Unlock()
	return true, nil
}
