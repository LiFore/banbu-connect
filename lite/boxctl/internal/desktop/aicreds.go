package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"boxctl/internal/dist"
)

// ---- 管理服务托管的 Claude / Codex 凭据：写进工作区、读回上传、鉴权测试、上报额度（ADR-061）----
//
// 对每个允许本机使用、有托管凭据的账号，比较托管的凭据 S（版本 v）和工作区里的凭据 W：
//   - W 的 refresh token 和 S 相同：已同步，记下 v；
//   - 工作区没有登录：把 S 写进工作区（外壳的 import 操作）；
//   - 本机上次同步的就是 v、之后 W 变了：本机的 claude / codex 刷新过，按版本号 v 上传 W；
//   - v 变了、W 没变：管理服务那边更新过（管理员填写或别的客户端刷新），把 S 写进工作区；
//   - 两边都变了或不知道：访问令牌过期时间晚的那份更新（管理员填的没有过期时间，以它为准）。
// 托管凭据缺 Claude 的访问令牌或 Codex 的 id_token 时先用 refresh token 刷新一次并上传，再写进工作区。
// Claude / Codex 的 refresh token 是一次性的：同一账号在多台客户端同时使用时，一台刷新后其他客户端要等
// 同步到新版本，期间可能要重新登录。
//
// 令牌集中续期（ADR-071）后，上面的对比只用于 Codex 的持有者（同步结果 token_only 为 false）。
// 其他情况本机只拿 Access Token（reconcileTokenOnly）：
//   - 把托管的 Access Token 写进工作区，不写 Refresh Token；
//   - 工作区里出现 Refresh Token（本机登录、旧版下发的）：访问令牌比托管的新就上报（本机刚登录），
//     然后重新写入托管的凭据，把本机的 Refresh Token 去掉；
//   - 管理服务发来续期通知时，本机正在用这个账号就弹通知请用户退出或切换账号，并回报忙不忙；
//   - 管理服务把续期任务交给本机时，用任务里的 Refresh Token 刷新，把新令牌交回去（交回失败时留着重试）。

const (
	aiCredsFile    = "ai-creds.json"
	importRetry    = 3 * time.Minute // 写入没生效时多久后重试
	tokenMargin    = time.Minute     // 访问令牌剩多久算过期
	refreshTimeout = 30 * time.Second
)

// 刷新接口和 OAuth 客户端 ID 是 claude / codex 命令行自带的公开值（参照 cockpit-tools 读出的做法）。
// TODO(verify:V-ai-cred-refresh) 未在真机确认。
var aiRefresh = struct {
	ClaudeURL, ClaudeClientID string
	CodexURL, CodexClientID   string
}{
	ClaudeURL:      "https://platform.claude.com/v1/oauth/token",
	ClaudeClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
	CodexURL:       "https://auth.openai.com/oauth/token",
	CodexClientID:  "app_EMoamEEZ73f0CkXaXp7hrann",
}

type appliedCred struct {
	Version int64  `json:"version"`
	Refresh string `json:"refresh"` // refresh token 的摘要，不存原文
}

type aiCreds struct {
	c        *dist.Client
	ws       *workspaces
	quota    *aiQuota
	path     string
	log      *slog.Logger
	kick     chan struct{}
	announce func(title, body string) // 系统通知（续期前提醒正在用这个账号的用户）

	mu        sync.Mutex
	applied   map[int64]appliedCred
	importing map[int64]importMark
	refreshed map[int64]int64                 // 已为哪个版本补刷新过（失败也不重复）
	reported  map[int64]time.Time             // 已上报的额度的查询时间
	noticed   map[string]bool                 // 已处理过的续期通知
	results   map[string]dist.AIRefreshResult // 刷新成功但还没交回的续期结果（新 Refresh Token 只在这里）
}

type importMark struct {
	version int64
	at      time.Time
}

func newAICreds(c *dist.Client, ws *workspaces, quota *aiQuota, dataDir string, log *slog.Logger, announce func(title, body string)) *aiCreds {
	a := &aiCreds{c: c, ws: ws, quota: quota, path: filepath.Join(dataDir, aiCredsFile), log: log, kick: make(chan struct{}, 1),
		announce: announce, applied: map[int64]appliedCred{}, importing: map[int64]importMark{}, refreshed: map[int64]int64{},
		reported: map[int64]time.Time{}, noticed: map[string]bool{}, results: map[string]dist.AIRefreshResult{}}
	if b, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(b, &a.applied)
	}
	ws.mu.Lock()
	ws.onReport = a.Kick
	ws.mu.Unlock()
	return a
}

func (a *aiCreds) Kick() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func digest(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func (a *aiCreds) saveLocked() {
	b, _ := json.Marshal(a.applied)
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, a.path)
	}
}

// Run 在同步结果或工作区变化时对比凭据、执行鉴权测试，定期上报额度，直到 ctx 结束。
func (a *aiCreds) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		sync, ok := a.c.AISyncState()
		if ok {
			a.reconcile(ctx, sync)
			a.handleNotices(ctx, sync)
			a.runJobs(ctx, sync)
			a.runTests(ctx, sync)
		}
		a.retryResults(ctx)
		a.feedQuota(sync)
		a.reportQuota(ctx, sync)
		select {
		case <-ctx.Done():
			return
		case <-a.c.AIChanged():
		case <-a.kick:
		case <-tick.C:
		}
	}
}

func tokenKey(kind, email string) string { return kind + "\x00" + email }

// local 返回工作区里的登录状态和凭据。
func (a *aiCreds) local() (map[string]Workspace, map[string]WorkspaceToken) {
	ws := map[string]Workspace{}
	if r := a.ws.Report(); r != nil {
		for _, w := range r.Workspaces {
			ws[tokenKey(w.Kind, w.Email)] = w
		}
	}
	toks := map[string]WorkspaceToken{}
	for _, t := range a.ws.Tokens() {
		if t.Email != "" {
			toks[tokenKey(t.Kind, t.Email)] = t
		}
	}
	return ws, toks
}

func credFromToken(t WorkspaceToken) dist.AICredential {
	c := dist.AICredential{AccessToken: t.Token, RefreshToken: t.RefreshToken, ExpiresAt: t.ExpiresAt, IDToken: t.IDToken, AccountID: t.AccountID}
	if c.ExpiresAt == 0 && t.Kind == dist.KindCodex {
		_, _, c.ExpiresAt = dist.CodexTokenInfo(t.Token)
	}
	return c
}

// complete 表示凭据能直接写进工作区：Claude 要有访问令牌，Codex 还要有 id_token（codex 从它读账号信息）。
func complete(kind string, c dist.AICredential) bool {
	if c.RefreshToken == "" || c.AccessToken == "" {
		return false
	}
	return kind != dist.KindCodex || c.IDToken != ""
}

func (a *aiCreds) reconcile(ctx context.Context, sync dist.AISync) {
	wss, toks := a.local()
	if a.ws.Report() == nil {
		return // 外壳还没报过工作区状态，不知道本机有什么
	}
	for _, acc := range sync.Accounts {
		key := tokenKey(acc.Kind, acc.Email)
		w, hasWS := wss[key]
		logged := hasWS && w.LoggedIn
		var W dist.AICredential
		if t, ok := toks[key]; ok {
			W = credFromToken(t)
		}
		emailOK := !hasWS || w.LoginEmail == "" || w.LoginEmail == acc.Email
		if acc.TokenOnly {
			a.reconcileTokenOnly(ctx, acc, logged, emailOK, W)
			continue
		}
		S, v := acc.Credential, acc.Version

		if S == nil {
			// 从没有托管过凭据：把本机登录的凭据交给管理服务（清除过的不上传）。
			if v == 0 && logged && emailOK && W.RefreshToken != "" {
				a.upload(ctx, acc, W, "login")
			}
			continue
		}
		if logged && W.RefreshToken == S.RefreshToken {
			a.markApplied(acc.ID, v, W.RefreshToken)
			continue
		}
		if !complete(acc.Kind, *S) {
			a.completeCred(ctx, acc)
			continue
		}
		if !logged {
			a.importCred(acc, *S)
			continue
		}
		a.mu.Lock()
		ap, known := a.applied[acc.ID]
		a.mu.Unlock()
		localChanged := !known || ap.Refresh != digest(W.RefreshToken)
		serverChanged := !known || ap.Version != v
		switch {
		case !serverChanged && localChanged:
			if emailOK {
				a.upload(ctx, acc, W, "refresh")
			}
		case serverChanged && !localChanged:
			a.importCred(acc, *S)
		case S.ExpiresAt == 0 || S.ExpiresAt >= W.ExpiresAt || !emailOK:
			a.importCred(acc, *S)
		default:
			a.upload(ctx, acc, W, "refresh")
		}
	}
}

// 只拿 Access Token 的账号这一轮要做的事。
const (
	tokNone    = ""
	tokUpload  = "upload"  // 上报本机登录的凭据
	tokImport  = "import"  // 把托管的 Access Token 写进工作区（同时去掉本机的 Refresh Token）
	tokApplied = "applied" // 已一致
)

// tokenOnlyAction 决定只拿 Access Token 的账号怎么处理（ADR-071）。S 是托管的凭据（可能为空），W 是工作区里的。
func tokenOnlyAction(S *dist.AICredential, version int64, logged, emailOK bool, W dist.AICredential) string {
	if S == nil {
		// 从没有托管过凭据：把本机登录的凭据交给管理服务（清除过的不上传）。
		if version == 0 && logged && emailOK && W.RefreshToken != "" {
			return tokUpload
		}
		return tokNone
	}
	if logged && W.RefreshToken != "" {
		// 访问令牌比托管的新：本机刚登录过，上报后下一轮再去掉本机的 Refresh Token。
		// 旧的（比如以前完整下发、已被续期顶掉的）不上报，免得用失效的 Refresh Token 覆盖管理服务的。
		if emailOK && W.AccessToken != S.AccessToken && W.ExpiresAt > S.ExpiresAt+tokenMargin.Milliseconds() {
			return tokUpload
		}
		return tokImport
	}
	switch {
	case S.AccessToken == "":
		return tokNone
	case logged && W.AccessToken == S.AccessToken:
		return tokApplied
	}
	return tokImport
}

// reconcileTokenOnly 处理只拿 Access Token 的账号。
func (a *aiCreds) reconcileTokenOnly(ctx context.Context, acc dist.AISyncAccount, logged, emailOK bool, W dist.AICredential) {
	switch tokenOnlyAction(acc.Credential, acc.Version, logged, emailOK, W) {
	case tokUpload:
		a.upload(ctx, acc, W, "login")
	case tokImport:
		served := *acc.Credential
		served.RefreshToken = "" // 上传后同步结果的本地副本会带着本机的 Refresh Token，写回工作区时去掉
		a.importCred(acc, served)
	case tokApplied:
		a.markApplied(acc.ID, acc.Version, "")
	}
}

// ---- 续期（ADR-071）----

// handleNotices 处理续期通知：本机正在用这个账号时弹通知，回报忙不忙。每个通知只处理一次。
func (a *aiCreds) handleNotices(ctx context.Context, sync dist.AISync) {
	wss, _ := a.local()
	report := a.ws.Report()
	for _, acc := range sync.Accounts {
		n := acc.Refresh
		if n == nil || n.Phase != dist.RefreshNotice {
			continue
		}
		a.mu.Lock()
		seen := a.noticed[n.ID]
		if len(a.noticed) > 200 {
			clear(a.noticed)
		}
		a.noticed[n.ID] = true
		a.mu.Unlock()
		if seen {
			continue
		}
		busy := false
		if w, ok := wss[tokenKey(acc.Kind, acc.Email)]; ok {
			running := w.Running
			if w.Default && report != nil {
				running = report.Defaults[acc.Kind].Running
			}
			busy = running != 0 // -1 表示无法得知（Windows），按正在用处理
		}
		if busy && a.announce != nil {
			wait := "马上"
			if t, err := time.Parse(time.RFC3339, n.Deadline); err == nil && time.Until(t) > 5*time.Second {
				wait = fmt.Sprintf("约 %d 秒后", int(time.Until(t).Seconds()))
			}
			a.announce(fmt.Sprintf("%s 账号即将续期：%s", kindTitle(acc.Kind), acc.Email),
				fmt.Sprintf("管理服务%s续期这个账号的令牌。正在运行的 %s 请先保存并退出，或切换到别的账号；续期完成后重新打开即可。", wait, acc.Kind))
		}
		go func(id string, busy bool) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := a.c.AckAIRefresh(ctx, id, busy); err != nil {
				a.log.Warn("回报续期通知失败", "err", err)
			}
		}(n.ID, busy)
	}
}

func kindTitle(kind string) string {
	if kind == dist.KindCodex {
		return "Codex"
	}
	return "Claude"
}

// runJobs 执行交给本机的续期：用任务里的 Refresh Token 刷新，交回新令牌。
func (a *aiCreds) runJobs(ctx context.Context, sync dist.AISync) {
	for _, j := range sync.Jobs {
		if !a.c.ClaimAuthTest("refresh:" + j.ID) {
			continue
		}
		go func(j dist.AIRefreshJob) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			r := dist.AIRefreshResult{AccountID: j.AccountID, BaseVersion: j.BaseVersion}
			cred, err := a.refresh(ctx, j.Kind, j.Credential)
			if err != nil {
				var re *refreshError
				r.Error, r.Permanent = err.Error(), errors.As(err, &re) && re.permanent
				a.log.Warn("续期失败", "kind", j.Kind, "account", j.AccountID, "err", err)
			} else {
				r.OK, r.Credential = true, &cred
			}
			a.finishJob(ctx, j.ID, r)
			a.Kick()
		}(j)
	}
}

// finishJob 交回续期结果。刷新成功但交不回去时留着，之后重试：旧的 Refresh Token 已经作废，新的只在这里。
func (a *aiCreds) finishJob(ctx context.Context, id string, r dist.AIRefreshResult) {
	err := a.c.FinishAIRefresh(ctx, id, r)
	var he *dist.HTTPError
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err == nil:
		delete(a.results, id)
		if r.OK {
			a.log.Info("已续期并交回新令牌", "account", r.AccountID)
		}
	case errors.As(err, &he) && he.Status >= 400 && he.Status < 500:
		delete(a.results, id) // 管理服务拒绝（版本冲突等），重试没有意义
		a.log.Warn("管理服务没有接受续期结果", "account", r.AccountID, "err", err)
	case r.OK:
		a.results[id] = r
		a.log.Error("续期结果交不回管理服务，稍后重试", "account", r.AccountID, "err", err)
	}
}

func (a *aiCreds) retryResults(ctx context.Context) {
	a.mu.Lock()
	pending := maps.Clone(a.results)
	a.mu.Unlock()
	for id, r := range pending {
		a.finishJob(ctx, id, r)
	}
}

func (a *aiCreds) markApplied(id, version int64, refresh string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.importing, id)
	ap := appliedCred{Version: version, Refresh: digest(refresh)}
	if a.applied[id] != ap {
		a.applied[id] = ap
		a.saveLocked()
	}
}

func (a *aiCreds) importCred(acc dist.AISyncAccount, cred dist.AICredential) {
	a.mu.Lock()
	m, ok := a.importing[acc.ID]
	if ok && m.version == acc.Version && time.Since(m.at) < importRetry {
		a.mu.Unlock()
		return
	}
	a.importing[acc.ID] = importMark{version: acc.Version, at: time.Now()}
	a.mu.Unlock()
	a.ws.EnqueueImport(acc.Kind, acc.Email, cred)
	a.log.Info("把托管的凭据写进工作区", "kind", acc.Kind, "account", acc.Email, "version", acc.Version)
}

func (a *aiCreds) upload(ctx context.Context, acc dist.AISyncAccount, cred dist.AICredential, source string) {
	v, err := a.c.UploadAICredential(ctx, acc.ID, dist.AICredentialUpload{BaseVersion: acc.Version, Source: source, Credential: cred})
	switch {
	case errors.Is(err, dist.ErrCredentialConflict):
		return // 别处更新过，等下次同步
	case err != nil:
		a.log.Warn("上传本机凭据失败", "account", acc.Email, "err", err)
		return
	}
	a.markApplied(acc.ID, v, cred.RefreshToken)
	a.log.Info("已上传本机凭据", "kind", acc.Kind, "account", acc.Email, "version", v)
}

// completeCred 用 refresh token 换出完整的凭据并上传（每个版本只试一次）。
func (a *aiCreds) completeCred(ctx context.Context, acc dist.AISyncAccount) {
	a.mu.Lock()
	if a.refreshed[acc.ID] == acc.Version {
		a.mu.Unlock()
		return
	}
	a.refreshed[acc.ID] = acc.Version
	a.mu.Unlock()
	if acc.Credential.RefreshToken == "" {
		return // 只有访问令牌：只能查额度，不能写进工作区
	}
	cred, err := a.refresh(ctx, acc.Kind, *acc.Credential)
	if err != nil {
		a.log.Warn("刷新托管的凭据失败", "account", acc.Email, "err", err)
		return
	}
	if _, err := a.c.UploadAICredential(ctx, acc.ID, dist.AICredentialUpload{BaseVersion: acc.Version, Source: "refresh", Credential: cred}); err != nil {
		a.log.Warn("上传刷新后的凭据失败", "account", acc.Email, "err", err)
	}
}

// refreshError 是刷新被拒绝；permanent 表示 Refresh Token 本身无效。
type refreshError struct {
	msg       string
	permanent bool
}

func (e *refreshError) Error() string { return e.msg }

// refresh 用 refresh token 换新的访问令牌。refresh token 会轮换，调用方必须上传新凭据。
func (a *aiCreds) refresh(ctx context.Context, kind string, c dist.AICredential) (dist.AICredential, error) {
	if c.RefreshToken == "" {
		return c, errors.New("没有 Refresh Token，无法刷新")
	}
	endpoint := aiRefresh.ClaudeURL
	body := map[string]string{"grant_type": "refresh_token", "refresh_token": c.RefreshToken, "client_id": aiRefresh.ClaudeClientID}
	if kind == dist.KindCodex {
		endpoint = aiRefresh.CodexURL
		body = map[string]string{"grant_type": "refresh_token", "refresh_token": c.RefreshToken, "client_id": aiRefresh.CodexClientID,
			"scope": "openid profile email"}
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return c, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc, err := a.quota.client(kind)
	if err != nil {
		return c, fmt.Errorf("现在不能刷新令牌：%w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return c, fmt.Errorf("刷新令牌失败：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        any    `json:"error"`
		Desc         string `json:"error_description"`
	}
	_ = json.Unmarshal(data, &t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		msg := t.Desc
		if msg == "" && t.Error != nil {
			msg = fmt.Sprint(t.Error)
		}
		// 400 / 401：Refresh Token 被拒绝（失效或已被使用），重试没有意义；其他状态（限流、服务端错误）可以稍后再试。
		bad := resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized
		if !bad {
			return c, &refreshError{msg: fmt.Sprintf("刷新令牌失败（HTTP %d %s）", resp.StatusCode, msg)}
		}
		return c, &refreshError{msg: fmt.Sprintf("Refresh Token 无效或已被使用（HTTP %d %s）", resp.StatusCode, msg), permanent: true}
	}
	out := c
	out.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		out.RefreshToken = t.RefreshToken
	}
	if t.IDToken != "" {
		out.IDToken = t.IDToken
	}
	out.ExpiresAt = 0
	if t.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli()
	}
	if err := out.Normalize(kind); err != nil {
		return c, err
	}
	return out, nil
}

// ---- 鉴权测试 ----

func (a *aiCreds) runTests(ctx context.Context, sync dist.AISync) {
	for _, t := range sync.Tests {
		if !a.c.ClaimAuthTest(t.ID) {
			continue
		}
		var acc *dist.AISyncAccount
		for i := range sync.Accounts {
			if sync.Accounts[i].ID == t.AccountID {
				acc = &sync.Accounts[i]
			}
		}
		go func(t dist.AuthTest) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			r := a.test(ctx, acc)
			if err := a.c.FinishAuthTest(ctx, t.ID, r); err != nil {
				a.log.Warn("回报鉴权测试结果失败", "err", err)
			}
			a.Kick()
		}(t)
	}
}

// test 用托管的凭据查一次额度；访问令牌没有、过期或被拒绝时用 refresh token 刷新后再查，并上传新凭据。
func (a *aiCreds) test(ctx context.Context, acc *dist.AISyncAccount) dist.AuthTestResult {
	if acc == nil || acc.Credential == nil {
		return dist.AuthTestResult{Error: "本机没有同步到这个账号的凭据"}
	}
	cred := *acc.Credential
	fresh := cred.AccessToken != "" && (cred.ExpiresAt == 0 || time.UnixMilli(cred.ExpiresAt).After(time.Now().Add(tokenMargin)))
	if fresh {
		q, status := a.quota.fetchStatus(ctx, WorkspaceToken{Kind: acc.Kind, Email: acc.Email, Token: cred.AccessToken, AccountID: cred.AccountID})
		if q.Error == "" {
			return dist.AuthTestResult{OK: true, Quota: &q}
		}
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			return dist.AuthTestResult{Error: q.Error, Quota: &q}
		}
	}
	if cred.RefreshToken == "" {
		if acc.TokenOnly {
			return dist.AuthTestResult{Error: "Access Token 无效或已过期：本机只拿 Access Token，续期由管理服务安排（可在管理服务点「立即续期」）"}
		}
		return dist.AuthTestResult{Error: "Access Token 无效或已过期，并且没有 Refresh Token"}
	}
	next, err := a.refresh(ctx, acc.Kind, cred)
	if err != nil {
		return dist.AuthTestResult{Error: err.Error()}
	}
	note := ""
	if _, err := a.c.UploadAICredential(ctx, acc.ID, dist.AICredentialUpload{BaseVersion: acc.Version, Source: "refresh", Credential: next}); err != nil {
		note = "（刷新后的凭据上传失败：" + err.Error() + "）"
	}
	q, _ := a.quota.fetchStatus(ctx, WorkspaceToken{Kind: acc.Kind, Email: acc.Email, Token: next.AccessToken, AccountID: next.AccountID})
	if q.Error != "" {
		return dist.AuthTestResult{Error: "刷新成功，但查询额度失败：" + q.Error + note, Quota: &q}
	}
	return dist.AuthTestResult{OK: note == "", Error: note, Quota: &q}
}

// ---- 额度 ----

// feedQuota 把工作区的访问令牌，以及本机没有工作区、但托管凭据里访问令牌还有效的账号交给额度查询。
func (a *aiCreds) feedQuota(sync dist.AISync) {
	list := a.ws.Tokens()
	have := map[string]bool{}
	for _, t := range list {
		have[tokenKey(t.Kind, t.Email)] = true
	}
	now := time.Now()
	for _, acc := range sync.Accounts {
		c := acc.Credential
		if c == nil || c.AccessToken == "" || have[tokenKey(acc.Kind, acc.Email)] {
			continue
		}
		if c.ExpiresAt != 0 && !time.UnixMilli(c.ExpiresAt).After(now.Add(tokenMargin)) {
			continue
		}
		list = append(list, WorkspaceToken{Kind: acc.Kind, Email: acc.Email, Token: c.AccessToken, AccountID: c.AccountID})
	}
	a.quota.SetTokens(list)
}

// reportQuota 上报有新结果的账号的额度。
func (a *aiCreds) reportQuota(ctx context.Context, sync dist.AISync) {
	var samples []dist.AIQuotaSample
	a.mu.Lock()
	for _, acc := range sync.Accounts {
		q := a.quota.Get(acc.Kind, acc.Email)
		if q == nil || !q.CheckedAt.After(a.reported[acc.ID]) {
			continue
		}
		samples = append(samples, dist.AIQuotaSample{AccountID: acc.ID, Quota: *q})
	}
	a.mu.Unlock()
	if len(samples) == 0 {
		return
	}
	if err := a.c.ReportAIQuota(ctx, samples); err != nil {
		a.log.Warn("上报额度失败", "err", err)
		return
	}
	a.mu.Lock()
	for _, s := range samples {
		a.reported[s.AccountID] = s.Quota.CheckedAt
	}
	a.mu.Unlock()
}
