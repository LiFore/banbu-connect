package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"boxctl/internal/dist"
	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

// ---- 多开：本机工作区（S1.9，ADR-052）----
//
// 每个 Claude / Codex 账号在当前用户的 Banbu Connect 数据目录下有一个隔离工作区
// （workspaces/claude-cli/<账号>：CLAUDE_CONFIG_DIR；workspaces/codex/<账号>：CODEX_HOME）。
// 工作区、凭据、终端都在用户会话里，由托盘外壳（client/app 的 workspaces.rs）管理：
//   - 外壳定期扫描工作区，把状态（登录与否、默认账号、运行中的实例、额度用的访问令牌）报给本机服务；
//   - 页面的操作（Codex 登录、打开终端、设为默认、删除）先在这里检查保护状态（BXC-F0-041），再排队给外壳执行；
//   - Claude 登录沿用一键登录（claude_login.rs，登录到账号的工作区）。
// 访问令牌只放在内存里，只用于查额度（BXC-F0-039、BXC-F0-040），不刷新。

const (
	workspaceActionKeep  = 30 * time.Minute
	workspaceActionClaim = 2 * time.Minute
	maxWorkspaceActions  = 20
)

// Workspace 是外壳报来的一个工作区。
type Workspace struct {
	Kind       string `json:"kind"`  // claude / codex
	Email      string `json:"email"` // 账号（工作区按它命名）
	Dir        string `json:"dir"`
	LoggedIn   bool   `json:"logged_in"`
	LoginEmail string `json:"login_email,omitempty"` // 工作区里实际登录的账号
	Default    bool   `json:"default"`               // 凭据已移到默认目录（设为默认，BXC-F0-046）
	Running    int    `json:"running"`               // 使用这个工作区的进程数；-1 表示无法得知（Windows）
	Error      string `json:"error,omitempty"`
}

// DefaultDir 是 claude / codex 的默认目录（~/.claude、~/.codex）的状态。
type DefaultDir struct {
	Email    string `json:"email,omitempty"` // 默认目录里登录的账号
	Owner    string `json:"owner,omitempty"` // 设为默认的工作区账号；空表示用户自己的
	Running  int    `json:"running"`         // 使用默认目录的进程数；-1 表示无法得知
	LoggedIn bool   `json:"logged_in"`
}

// WorkspaceToken 是外壳读出的工作区凭据（只读，不刷新）：访问令牌用于查额度，
// refresh token 等用于和管理服务托管的凭据对比、上传本机刷新后的凭据（ADR-061）。
type WorkspaceToken struct {
	Kind         string `json:"kind"`
	Email        string `json:"email"` // 工作区账号；默认目录为空
	Token        string `json:"token"`
	AccountID    string `json:"account_id,omitempty"` // Codex：ChatGPT-Account-Id
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // Claude：访问令牌过期时间，Unix 毫秒
	IDToken      string `json:"id_token,omitempty"`   // Codex
}

// WorkspaceReport 是外壳的一次扫描结果。
type WorkspaceReport struct {
	Workspaces []Workspace           `json:"workspaces"`
	Defaults   map[string]DefaultDir `json:"defaults"` // claude / codex
	CLIDir     string                `json:"cli_dir"`  // 命令行入口（banbu-claude / banbu-codex）所在目录（BXC-F0-038）
	Tokens     []WorkspaceToken      `json:"tokens,omitempty"`
	At         time.Time             `json:"at"`
}

// WorkspaceAction 是排队给外壳执行的操作。
type WorkspaceAction struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"` // login（Codex）/ app_login（Codex，用 ChatGPT 桌面客户端登录）/ terminal / launch / set_default / delete
	Kind      string    `json:"kind"`
	Email     string    `json:"email"` // 空表示默认目录（只用于 terminal）
	Cwd       string    `json:"cwd,omitempty"`
	Status    string    `json:"status"` // pending / running / done / failed
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Credential 是 import 要写进工作区的凭据（ADR-061），只在外壳认领时返回。
	Credential *dist.AICredential `json:"credential,omitempty"`
}

// launch 是一键启动：打开工作区的终端并直接运行 claude / codex。
// adopt_default 是「用本机已登录的」：本机默认目录里登录的正是这个账号（用户自己用 claude / codex 登录过，
// 注册方式不限）时，把默认目录交给这个账号的工作区（打上默认标记，不搬凭据），之后 aiCreds 按「本机登录」上报管理服务。
// app_login 是用 ChatGPT 桌面客户端登录 Codex 账号：外壳给客户端单开一个实例（CODEX_HOME 指向工作区），登录后关掉。
// desktop_app 是打开 Claude Desktop（只用默认登录，不分账号；ADR-088）：和启动一样先过保护检查，外壳直接打开应用。
var workspaceActions = map[string]bool{"login": true, "app_login": true, "terminal": true, "launch": true, "set_default": true, "delete": true, "adopt_default": true, "desktop_app": true}

// workspaces 是本机服务保存的工作区状态和操作队列。
type workspaces struct {
	ks     *killswitch.Engine
	strict *trust.StrictChecker
	quota  *aiQuota
	// bypass 打开时跳过保护检查（信任 Bypass，见 bypass.go）。
	bypass *trustBypass

	mu      sync.Mutex
	report  *WorkspaceReport
	tokens  []WorkspaceToken
	actions []*WorkspaceAction
	// onReport 在外壳报来新的扫描结果时调用（托管凭据的对比，ADR-061）；为空时令牌直接交给额度查询。
	onReport func()
}

func newWorkspaces(ks *killswitch.Engine, strict *trust.StrictChecker, quota *aiQuota) *workspaces {
	return &workspaces{ks: ks, strict: strict, quota: quota}
}

// ---- 保护状态检查（BXC-F0-041，Q-011：不满足时直接阻止）----

// serviceSuffixes 是账号类型对应的受保护服务域名，用来找它走的严格组。
var serviceSuffixes = map[string][]string{
	dist.KindClaude: {"anthropic.com", "claude.ai", "claude.com"},
	dist.KindCodex:  {"openai.com", "chatgpt.com"},
}

// ProtectionError 是保护状态不满足，操作被阻止。Fix 是页面上去处理的地址。
type ProtectionError struct {
	Reason string
	Fix    string
}

func (e *ProtectionError) Error() string { return e.Reason }

// groupFor 找账号类型的受保护服务和它的代理组。
func groupFor(cfg killswitch.Config, kind string) (service, group string) {
	for _, svc := range cfg.Services {
		if !svc.Enabled {
			continue
		}
		for _, suf := range svc.Suffixes {
			s := strings.ToLower(strings.TrimPrefix(suf, "."))
			for _, want := range serviceSuffixes[kind] {
				if s == want || strings.HasSuffix(s, "."+want) {
					return svc.Name, svc.Group
				}
			}
		}
	}
	return "", ""
}

// checkProtection 检查账号类型对应的保护状态：保护服务已开启并在起作用，对应的严格组出口符合要求。
// 客户端本地判断，只防误操作（不是安全边界）。
func checkProtection(st killswitch.Status, cfg killswitch.Config, strict trust.StrictStatus, kind string) error {
	name := map[string]string{dist.KindClaude: "Claude", dist.KindCodex: "ChatGPT"}[kind]
	svc, group := groupFor(cfg, kind)
	if svc == "" {
		return &ProtectionError{Reason: "保护服务里没有启用 " + name + " 的受保护服务，登录和使用账号前请先开启", Fix: "/advanced/protection"}
	}
	if !st.Enabled {
		return &ProtectionError{Reason: "保护服务没有开启：" + name + " 的流量可能不经代理，先开启保护服务", Fix: "/"}
	}
	switch st.State {
	case killswitch.StateProtecting, killswitch.StateDirect:
	default:
		reason := st.Verdict.Reason
		if reason == "" {
			reason = "保护服务没有在正常保护（" + st.State + "）"
		}
		return &ProtectionError{Reason: reason, Fix: "/"}
	}
	if st.State == killswitch.StateDirect {
		return nil // 受信网络上由路由盒负责出口
	}
	for _, g := range strict.Groups {
		if g.Group == group && !g.Allowed {
			reason := g.Reason
			if reason == "" {
				reason = "出口不符合要求"
			}
			return &ProtectionError{Reason: fmt.Sprintf("「%s」的严格线路%s", group, reason), Fix: "/advanced/egress"}
		}
	}
	return nil
}

// Check 用当前状态检查账号类型的保护状态。
func (w *workspaces) Check(kind string) error {
	if w.ks == nil || w.bypass.Enabled() {
		return nil
	}
	var ss trust.StrictStatus
	if w.strict != nil {
		ss = w.strict.Status()
	}
	return checkProtection(w.ks.Status(), w.ks.Config(), ss, kind)
}

// ---- 外壳报告与操作队列 ----

// SetReport 保存外壳的扫描结果，令牌交给额度查询。
func (w *workspaces) SetReport(r WorkspaceReport) {
	r.At = time.Now()
	tokens := r.Tokens
	r.Tokens = nil
	w.mu.Lock()
	w.report = &r
	w.tokens = tokens
	hook := w.onReport
	w.mu.Unlock()
	switch {
	case hook != nil:
		hook()
	case w.quota != nil:
		w.quota.SetTokens(tokens)
	}
}

// Tokens 返回最近一次扫描读出的工作区凭据。
func (w *workspaces) Tokens() []WorkspaceToken {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.tokens)
}

// EnqueueImport 排队把托管的凭据写进账号的工作区（ADR-061）。不需要联网，不做保护状态检查；
// 同一账号还有没做完的 import 时用新凭据替换。
func (w *workspaces) EnqueueImport(kind, email string, cred dist.AICredential) WorkspaceAction {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	for _, x := range w.actions {
		if x.Status == "pending" && x.Action == "import" && x.Kind == kind && x.Email == email {
			x.Credential = &cred
			return *x
		}
	}
	a := &WorkspaceAction{ID: newActionID(), Action: "import", Kind: kind, Email: email, Status: "pending", CreatedAt: time.Now(),
		Credential: &cred}
	w.actions = append(w.actions, a)
	return *a
}

// Report 返回最近一次扫描结果；外壳没在运行时为空。
func (w *workspaces) Report() *WorkspaceReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.report == nil {
		return nil
	}
	r := *w.report
	return &r
}

func newActionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Enqueue 检查后把操作排队给外壳。登录、打开终端、启动、设为默认要先通过保护状态检查。
func (w *workspaces) Enqueue(a WorkspaceAction) (WorkspaceAction, error) {
	if !workspaceActions[a.Action] {
		return a, errors.New("未知的操作")
	}
	if a.Kind != dist.KindClaude && a.Kind != dist.KindCodex {
		return a, errors.New("账号类型只能是 claude 或 codex")
	}
	if (a.Action == "login" || a.Action == "app_login") && a.Kind != dist.KindCodex {
		return a, errors.New("Claude 账号用一键登录")
	}
	if a.Action == "desktop_app" {
		if a.Kind != dist.KindClaude {
			return a, errors.New("只有 Claude 有桌面客户端")
		}
		a.Email, a.Cwd = "", "" // 用 Claude Desktop 自己的登录，不分账号
	}
	if a.Email == "" && a.Action != "terminal" && a.Action != "launch" && a.Action != "desktop_app" {
		return a, errors.New("缺少账号")
	}
	// 删除、接管默认目录只动本机文件，不联网，不检查保护状态。
	if a.Action != "delete" && a.Action != "adopt_default" {
		if err := w.Check(a.Kind); err != nil {
			return a, err
		}
	}
	a.ID, a.Status, a.Error, a.CreatedAt, a.Credential = newActionID(), "pending", "", time.Now(), nil
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	for _, x := range w.actions {
		if (x.Status == "pending" || x.Status == "running") && x.Action == a.Action && x.Kind == a.Kind && x.Email == a.Email {
			return *x, nil // 同一个操作还没做完
		}
	}
	w.actions = append(w.actions, &a)
	return a, nil
}

func (w *workspaces) pruneLocked() {
	now := time.Now()
	w.actions = slices.DeleteFunc(w.actions, func(x *WorkspaceAction) bool {
		if x.Status == "pending" && now.Sub(x.CreatedAt) > workspaceActionClaim {
			x.Status, x.Error = "failed", "托盘程序没有处理，请确认 Banbu Connect 托盘在运行"
		}
		return now.Sub(x.CreatedAt) > workspaceActionKeep
	})
	if len(w.actions) > maxWorkspaceActions {
		w.actions = w.actions[len(w.actions)-maxWorkspaceActions:]
	}
}

// Pending 返回第一个待外壳执行的操作 ID（放在状态里）。
func (w *workspaces) Pending() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	for _, x := range w.actions {
		if x.Status == "pending" {
			return x.ID
		}
	}
	return ""
}

// Claim 由外壳调用：认领一个操作。
func (w *workspaces) Claim(id string) (WorkspaceAction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, x := range w.actions {
		if x.ID == id && x.Status == "pending" {
			x.Status = "running"
			return *x, nil
		}
	}
	return WorkspaceAction{}, dist.ErrNotFound
}

// Finish 由外壳调用：回报结果。
func (w *workspaces) Finish(id string, ok bool, msg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, x := range w.actions {
		if x.ID == id && x.Status == "running" {
			x.Status, x.Error = "done", ""
			if !ok {
				x.Status, x.Error = "failed", msg
				if x.Error == "" {
					x.Error = "操作没有完成"
				}
			}
			return nil
		}
	}
	return dist.ErrNotFound
}

// Actions 返回最近的操作，新的在前。
func (w *workspaces) Actions() []WorkspaceAction {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked()
	out := make([]WorkspaceAction, 0, len(w.actions))
	for i := len(w.actions) - 1; i >= 0; i-- {
		a := *w.actions[i]
		a.Credential = nil // 凭据只交给外壳
		out = append(out, a)
	}
	return out
}

// ---- 页面用的账号视图（BXC-F0-042）----

// AccountView 是账号页上的一个账号：管理服务登记的账号 + 本机工作区 + 额度。
type AccountView struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	Email     string     `json:"email"`
	Note      string     `json:"note"`
	Workspace *Workspace `json:"workspace,omitempty"` // 还没登录过时为空
	Quota     *AIQuota   `json:"quota,omitempty"`
	// Mail 表示管理服务授权了这个账号的 Gmail（Claude 一键登录、看登录邮件）；没有时用 claude / codex 自己的登录。
	Mail bool `json:"mail"`
}

// AccountsView 是账号页的数据。
type AccountsView struct {
	Bound    bool                  `json:"bound"`
	Error    string                `json:"error,omitempty"`
	Shell    bool                  `json:"shell"` // 托盘外壳报过工作区状态（没在运行时操作不会执行）
	Accounts []AccountView         `json:"accounts"`
	Defaults map[string]DefaultDir `json:"defaults"`
	// DefaultQuota 是默认目录（用户自己登录的账号）的额度，按类型。
	DefaultQuota map[string]*AIQuota `json:"default_quota"`
	CLIDir       string              `json:"cli_dir"`
	Actions      []WorkspaceAction   `json:"actions"`
	Protection   map[string]string   `json:"protection"` // 按类型：不满足时的原因，满足时没有这一项
	// QuotaPaused 按类型：额度查询暂停的原因（保护服务没有放通时不查），正常查询时没有这一项。
	QuotaPaused map[string]string `json:"quota_paused"`
}

func (w *workspaces) View(ctx context.Context, c *dist.Client) AccountsView {
	v := AccountsView{Accounts: []AccountView{}, Defaults: map[string]DefaultDir{}, DefaultQuota: map[string]*AIQuota{},
		Actions: w.Actions(), Protection: map[string]string{}, QuotaPaused: map[string]string{}}
	v.Bound = c.Status().Bound
	var list []dist.ClientClaudeAccount
	mail := map[int64]bool{}
	if sync, ok := c.AISyncState(); v.Bound && ok {
		// 同步结果包含所有允许本机使用的账号（含没有授权 Gmail、只有托管凭据的，ADR-061）。
		for _, a := range sync.Accounts {
			list = append(list, dist.ClientClaudeAccount{ID: a.ID, Kind: a.Kind, Email: a.Email, Note: a.Note})
			mail[a.ID] = a.Mail
		}
	} else if v.Bound {
		var err error
		if list, err = c.ClaudeAccounts(ctx); err != nil {
			v.Error = err.Error()
		}
		for _, a := range list {
			mail[a.ID] = true // 旧接口只返回授权了 Gmail 的账号
		}
	}
	r := w.Report()
	byKey := map[string]Workspace{}
	if r != nil {
		v.Shell, v.CLIDir = true, r.CLIDir
		for k, d := range r.Defaults {
			v.Defaults[k] = d
		}
		for _, ws := range r.Workspaces {
			byKey[ws.Kind+"\x00"+ws.Email] = ws
		}
	}
	for _, a := range list {
		kind := a.Kind
		if kind == "" {
			kind = dist.KindClaude
		}
		av := AccountView{ID: a.ID, Kind: kind, Email: a.Email, Note: a.Note, Mail: mail[a.ID]}
		if ws, ok := byKey[kind+"\x00"+a.Email]; ok {
			ws := ws
			av.Workspace = &ws
		}
		if w.quota != nil {
			av.Quota = w.quota.Get(kind, a.Email)
		}
		v.Accounts = append(v.Accounts, av)
	}
	for _, kind := range []string{dist.KindClaude, dist.KindCodex} {
		if w.quota != nil {
			if q := w.quota.Get(kind, ""); q != nil {
				v.DefaultQuota[kind] = q
			}
		}
		if err := w.Check(kind); err != nil {
			v.Protection[kind] = err.Error()
		}
		if w.quota != nil {
			if p := w.quota.Paused(kind); p != "" {
				v.QuotaPaused[kind] = p
			}
		}
	}
	return v
}

// ---- 接口 ----

func workspaceRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, w *workspaces, c *dist.Client) {
	decode := func(rw http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(v); err != nil {
			writeJSON(rw, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return false
		}
		return true
	}
	fail := func(rw http.ResponseWriter, err error) {
		var pe *ProtectionError
		switch {
		case errors.As(err, &pe):
			writeJSON(rw, http.StatusConflict, map[string]string{"code": "protection", "message": pe.Reason, "fix": pe.Fix})
		case errors.Is(err, dist.ErrNotFound):
			writeJSON(rw, http.StatusNotFound, apiErr{"not_found", "操作不存在或已结束"})
		default:
			writeJSON(rw, http.StatusBadRequest, apiErr{"invalid", err.Error()})
		}
	}

	// 页面。
	mux.Handle("GET /api/v1/desktop/accounts", authed(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		writeJSON(rw, http.StatusOK, w.View(r.Context(), c))
	}))
	mux.Handle("POST /api/v1/desktop/workspaces/actions", authed(func(rw http.ResponseWriter, r *http.Request) {
		var in WorkspaceAction
		if !decode(rw, r, &in) {
			return
		}
		a, err := w.Enqueue(in)
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, http.StatusOK, a)
	}))

	// 托盘外壳。
	mux.Handle("PUT /api/v1/desktop/workspaces/report", authed(func(rw http.ResponseWriter, r *http.Request) {
		var in WorkspaceReport
		if !decode(rw, r, &in) {
			return
		}
		w.SetReport(in)
		writeJSON(rw, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/desktop/workspaces/actions/{id}/claim", authed(func(rw http.ResponseWriter, r *http.Request) {
		a, err := w.Claim(r.PathValue("id"))
		if err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, http.StatusOK, a)
	}))
	mux.Handle("POST /api/v1/desktop/workspaces/actions/{id}/result", authed(func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if !decode(rw, r, &in) {
			return
		}
		if err := w.Finish(r.PathValue("id"), in.OK, in.Error); err != nil {
			fail(rw, err)
			return
		}
		writeJSON(rw, http.StatusOK, map[string]bool{"ok": true})
	}))
}
