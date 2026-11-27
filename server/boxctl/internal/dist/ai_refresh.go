package dist

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"boxctl/internal/notify"
)

// ---- Claude / Codex 令牌集中续期（ADR-071）----
//
// Refresh Token 只留在管理服务（Codex 另有一台持有者客户端），其他客户端只拿 Access Token：
// Refresh Token 一次性，多台各拿一份会互相顶掉；只在一处也就只有一处会泄露。
//
//   - Claude：访问令牌（约 8 小时）过期前 claudeRefreshLead，比 claude 自己的刷新（过期前几分钟）早，管理服务发起续期：
//     先进入「通知」阶段（refreshNotice），所有分配到的客户端收到后，本机正在用这个账号的弹通知请用户退出或切换账号，
//     并回报自己忙不忙；全部在线客户端都回报空闲时提前结束等待。然后挑一台在线客户端，把 Refresh Token 作为
//     一次性任务交给它刷新，它把新的令牌交回来，管理服务保存后马上同步给所有客户端。
//   - Codex：持有者客户端拿完整凭据（codex 自己也会刷新，刷新后上传）；其他客户端只拿 Access Token 和 id_token。
//     访问令牌（约 10 天）过期前 codexRefreshLead，赶在 codex 按 last_refresh 8 天自己刷新之前，管理服务把续期交给
//     持有者（不在线时交给别的在线客户端）执行，不需要通知。
//
// 管理服务自己仍不连 Anthropic / OpenAI（ADR-061）：刷新由客户端经本机的代理和保护执行。
// 状态只在内存：重启后按凭据的过期时间重新判断；任务结果带着基准版本号，重启后交回来的结果照样能保存。

const (
	claudeRefreshLead  = 30 * time.Minute
	codexRefreshLead   = 3 * 24 * time.Hour
	refreshNotice      = 60 * time.Second // Claude 续期前给正在用的客户端的宽限期
	refreshRunTimeout  = 90 * time.Second // 执行续期的客户端多久没交回结果算失败
	refreshRetry       = 5 * time.Minute  // 失败或没有客户端可用时多久后再试
	refreshMinGap      = 10 * time.Minute // 续期成功后至少隔多久才自动续期下一次（防止过期时间读错时反复续期）
	refreshCheckEvery  = 5 * time.Second
	refreshCapOnline   = 2 * time.Minute // 多久内带续期能力同步过的客户端算在线
	maxRefreshErrorLen = 300
)

// 续期阶段。
const (
	RefreshNotice  = "notice"  // 通知客户端，等宽限期
	RefreshRunning = "running" // 已交给一台客户端执行
	RefreshWaiting = "waiting" // 没有在线的客户端可以执行，稍后再试
	RefreshFailed  = "failed"  // 失败；Permanent 时 Refresh Token 已失效，等重新登录或填写
)

// AIRefreshState 是一个账号进行中或最近失败的续期。
type AIRefreshState struct {
	ID         string `json:"id"`
	Phase      string `json:"phase"`
	DeviceID   int64  `json:"device_id,omitempty"` // 执行的客户端
	DeviceName string `json:"device_name,omitempty"`
	Message    string `json:"message,omitempty"`
	Deadline   string `json:"deadline,omitempty"` // 通知阶段结束 / 执行超时的时间
	Permanent  bool   `json:"permanent,omitempty"`
	Manual     bool   `json:"manual,omitempty"` // 管理员点的「立即续期」

	kind     string
	base     int64 // 发起时的凭据版本；凭据换了版本（管理员填写、客户端登录）就作废
	deadline time.Time
	next     time.Time      // 等待或失败后下次再试的时间
	acks     map[int64]bool // 通知阶段客户端的回报：true 表示本机正在用
}

// AIRefreshNoticeView 是下发给客户端的续期状态（不含任务内容）。
type AIRefreshNoticeView struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Deadline string `json:"deadline"`
}

// AIRefreshJob 是交给一台客户端执行的续期：用 Credential 里的 Refresh Token 换新令牌，交回结果。
type AIRefreshJob struct {
	ID          string       `json:"id"`
	AccountID   int64        `json:"account_id"`
	Kind        string       `json:"kind"`
	BaseVersion int64        `json:"base_version"`
	Credential  AICredential `json:"credential"`
}

// AIRefreshResult 是客户端交回的续期结果。Permanent 表示 Refresh Token 被拒绝（失效或已被使用），重试没有意义。
type AIRefreshResult struct {
	AccountID   int64         `json:"account_id"`
	BaseVersion int64         `json:"base_version"`
	OK          bool          `json:"ok"`
	Error       string        `json:"error,omitempty"`
	Permanent   bool          `json:"permanent,omitempty"`
	Credential  *AICredential `json:"credential,omitempty"`
}

type refreshBook struct {
	states map[int64]*AIRefreshState // 按账号
	caps   map[int64]time.Time       // 带续期能力（新版客户端）最近一次同步的时间，按客户端
	done   map[int64]time.Time       // 最近一次续期成功的时间，按账号
}

func (b *refreshBook) init() {
	if b.states == nil {
		b.states, b.caps, b.done = map[int64]*AIRefreshState{}, map[int64]time.Time{}, map[int64]time.Time{}
	}
}

// holderOf 是 Codex 账号的持有者：指定的（必须在分配列表里），否则分配列表里的第一台。Claude 没有持有者。
func holderOf(a ClaudeAccount) int64 {
	if a.Kind != KindCodex || len(a.Devices) == 0 {
		return 0
	}
	if a.HolderDevice != 0 && slices.Contains(a.Devices, a.HolderDevice) {
		return a.HolderDevice
	}
	return a.Devices[0]
}

// tokenOnly 表示这台客户端只拿 Access Token。
func tokenOnly(a ClaudeAccount, deviceID int64) bool {
	return a.Kind == KindClaude || holderOf(a) != deviceID
}

// stripRefresh 去掉下发给只拿 Access Token 的客户端的 Refresh Token。
func stripRefresh(c *AICredential) *AICredential {
	if c == nil {
		return nil
	}
	out := *c
	out.RefreshToken = ""
	return &out
}

// refreshLead 是访问令牌过期前多久续期。Codex 能从 JWT 读出签发时间时，不超过有效期的三分之一。
func refreshLead(kind string, c AICredential) time.Duration {
	if kind != KindCodex {
		return claudeRefreshLead
	}
	lead := codexRefreshLead
	if claims := jwtClaims(c.AccessToken); claims != nil {
		iat, _ := claims["iat"].(float64)
		exp, _ := claims["exp"].(float64)
		if iat > 0 && exp > iat {
			lead = min(lead, time.Duration(exp-iat)*time.Second/3)
		}
	}
	return lead
}

// refreshDue 表示凭据该续期了：有 Refresh Token，并且访问令牌没有、不知道什么时候过期或快过期。
func refreshDue(kind string, c AICredential, now time.Time) bool {
	if c.RefreshToken == "" {
		return false
	}
	if c.AccessToken == "" || c.ExpiresAt == 0 {
		return true
	}
	return time.UnixMilli(c.ExpiresAt).Sub(now) < refreshLead(kind, c)
}

func (s *Server) refreshState(id int64) *AIRefreshState {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if st := s.refresh.states[id]; st != nil {
		c := *st
		c.acks = nil
		return &c
	}
	return nil
}

// noteRefreshCap 记下这台客户端支持续期任务（同步时带了 caps=refresh）。
func (s *Server) noteRefreshCap(deviceID int64) {
	s.refreshMu.Lock()
	s.refresh.init()
	s.refresh.caps[deviceID] = time.Now()
	s.refreshMu.Unlock()
}

// refreshView 返回账号的续期通知（给客户端）和交给这台客户端的任务（不含凭据，凭据由调用方补上）。
func (s *Server) refreshView(accountID, deviceID int64) (*AIRefreshNoticeView, *AIRefreshState) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	st := s.refresh.states[accountID]
	if st == nil || (st.Phase != RefreshNotice && st.Phase != RefreshRunning) {
		return nil, nil
	}
	v := &AIRefreshNoticeView{ID: st.ID, Phase: st.Phase, Deadline: st.Deadline}
	if st.Phase == RefreshRunning && st.DeviceID == deviceID {
		c := *st
		return v, &c
	}
	return v, nil
}

// ---- 调度 ----

// RunAIRefresh 定时检查托管的凭据，到期的发起续期，推进进行中的续期，直到 ctx 结束。
func (s *Server) RunAIRefresh(ctx context.Context, log *slog.Logger) {
	tick := time.NewTicker(refreshCheckEvery)
	defer tick.Stop()
	for {
		if err := s.stepAIRefresh(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Warn("检查令牌续期失败", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// stepAIRefresh 检查一遍所有账号。
func (s *Server) stepAIRefresh(ctx context.Context, now time.Time) error {
	accounts, err := s.ClaudeAccounts(ctx)
	if err != nil {
		return err
	}
	live := map[int64]bool{}
	for _, a := range accounts {
		live[a.ID] = true
		var cred *AICredential
		if a.HasCredential {
			if cred, _, err = s.aiCredential(ctx, a.ID); err != nil {
				return err
			}
		}
		s.stepAccount(ctx, a, cred, now)
	}
	s.refreshMu.Lock()
	for id := range s.refresh.states {
		if !live[id] {
			delete(s.refresh.states, id)
		}
	}
	s.refreshMu.Unlock()
	return nil
}

func (s *Server) stepAccount(ctx context.Context, a ClaudeAccount, cred *AICredential, now time.Time) {
	s.refreshMu.Lock()
	s.refresh.init()
	st := s.refresh.states[a.ID]
	// 凭据换了版本（管理员填写、客户端登录上报）或被清除：旧的续期作废。执行中的等它交回结果（结果按版本号保存）。
	if st != nil && st.Phase != RefreshRunning && (cred == nil || st.base != a.CredVersion) {
		delete(s.refresh.states, a.ID)
		st = nil
	}
	if cred == nil || cred.RefreshToken == "" {
		s.refreshMu.Unlock()
		return
	}
	changed := false
	switch {
	case st == nil:
		recent := now.Sub(s.refresh.done[a.ID]) < refreshMinGap
		if !recent && refreshDue(a.Kind, *cred, now) {
			st = s.startRefreshLocked(a, now, false)
			changed = true
		}
	case st.Phase == RefreshNotice:
		if !now.Before(st.deadline) || s.allIdleLocked(a, st, now) {
			s.dispatchLocked(a, st, now)
			changed = true
		}
	case st.Phase == RefreshRunning:
		if now.After(st.deadline) {
			st.Phase, st.Message = RefreshFailed, fmt.Sprintf("「%s」没有交回续期结果：可能离线或客户端版本太旧", st.DeviceName)
			st.next = now.Add(refreshRetry)
			changed = true
		}
	case st.Phase == RefreshWaiting || (st.Phase == RefreshFailed && !st.Permanent):
		if !now.Before(st.next) {
			manual := st.Manual
			delete(s.refresh.states, a.ID)
			st = s.startRefreshLocked(a, now, manual)
			changed = true
		}
	}
	var alert *notify.Message
	var alertKey string
	if st != nil && (st.Phase == RefreshWaiting || st.Phase == RefreshFailed) && cred.ExpiresAt > 0 && now.UnixMilli() > cred.ExpiresAt {
		alertKey = fmt.Sprintf("ai-refresh-expired:%d:%d", a.ID, a.CredVersion)
		alert = &notify.Message{Title: fmt.Sprintf("%s 令牌已过期且没能续期：%s", kindLabel(a.Kind), a.Email), Body: st.Message, Urgent: true}
	}
	s.refreshMu.Unlock()
	if alert != nil && s.fire(ctx, alertKey) {
		s.Send(ctx, "ai_refresh", *alert)
	}
	if changed {
		s.geminiChanged()
	}
}

func kindLabel(kind string) string {
	if kind == KindCodex {
		return "Codex"
	}
	return "Claude"
}

// startRefreshLocked 发起续期：Claude 先通知，Codex 直接交给客户端。调用方持有 refreshMu。
func (s *Server) startRefreshLocked(a ClaudeAccount, now time.Time, manual bool) *AIRefreshState {
	st := &AIRefreshState{ID: newLoginID(), kind: a.Kind, base: a.CredVersion, Manual: manual, acks: map[int64]bool{}}
	s.refresh.states[a.ID] = st
	if a.Kind == KindClaude && len(a.Devices) > 0 {
		st.Phase, st.deadline = RefreshNotice, now.Add(refreshNotice)
		st.Deadline = st.deadline.UTC().Format(time.RFC3339)
		st.Message = "已通知客户端，宽限期后续期"
		return st
	}
	s.dispatchLocked(a, st, now)
	return st
}

// onlineCapLocked 返回分配到、并且最近带续期能力同步过的客户端。
func (s *Server) onlineCapLocked(a ClaudeAccount, now time.Time) []int64 {
	out := []int64{}
	for _, d := range a.Devices {
		if t, ok := s.refresh.caps[d]; ok && now.Sub(t) < refreshCapOnline {
			out = append(out, d)
		}
	}
	return out
}

// allIdleLocked 表示所有在线的客户端都回报了、并且都没有在用这个账号。
func (s *Server) allIdleLocked(a ClaudeAccount, st *AIRefreshState, now time.Time) bool {
	online := s.onlineCapLocked(a, now)
	if len(online) == 0 {
		return false
	}
	for _, d := range online {
		busy, ok := st.acks[d]
		if !ok || busy {
			return false
		}
	}
	return true
}

// dispatchLocked 挑一台在线客户端执行：Codex 优先持有者；其次是回报空闲的、最近同步过的。
func (s *Server) dispatchLocked(a ClaudeAccount, st *AIRefreshState, now time.Time) {
	online := s.onlineCapLocked(a, now)
	if len(online) == 0 {
		st.Phase, st.next = RefreshWaiting, now.Add(time.Minute)
		st.Message = "没有在线的新版客户端可以执行续期，稍后重试"
		return
	}
	score := func(d int64) int {
		n := 0
		if d == holderOf(a) {
			n += 4
		}
		if busy, ok := st.acks[d]; ok && !busy {
			n += 2
		}
		return n
	}
	best := online[0]
	for _, d := range online[1:] {
		if score(d) > score(best) || (score(d) == score(best) && s.refresh.caps[d].After(s.refresh.caps[best])) {
			best = d
		}
	}
	st.Phase, st.DeviceID, st.DeviceName = RefreshRunning, best, s.deviceName(best)
	st.deadline = now.Add(refreshRunTimeout)
	st.Deadline = st.deadline.UTC().Format(time.RFC3339)
	st.Message = ""
}

func (s *Server) deviceName(id int64) string {
	var name string
	if err := s.DB.QueryRow(`SELECT name FROM devices WHERE id = ?`, id).Scan(&name); err != nil {
		return fmt.Sprintf("#%d", id)
	}
	return name
}

// ---- 管理员与客户端的操作 ----

// StartAIRefresh 是管理员点「立即续期」：不管还有多久过期，马上走一遍续期（Claude 照样先通知）。
func (s *Server) StartAIRefresh(ctx context.Context, id int64) (ClaudeAccount, error) {
	a, err := s.claudeAccount(ctx, id)
	if err != nil {
		return a, err
	}
	cred, _, err := s.aiCredential(ctx, id)
	if err != nil {
		return a, err
	}
	if cred == nil || cred.RefreshToken == "" {
		return a, &InputError{"这个账号没有托管的 Refresh Token，不能续期：先填入令牌或在客户端登录上报"}
	}
	if len(a.Devices) == 0 {
		return a, &InputError{"这个账号还没有分配给任何客户端：续期由客户端执行"}
	}
	s.refreshMu.Lock()
	s.refresh.init()
	if st := s.refresh.states[id]; st != nil && (st.Phase == RefreshNotice || st.Phase == RefreshRunning) {
		s.refreshMu.Unlock()
		return a, &InputError{"已经在续期了"}
	}
	delete(s.refresh.states, id)
	s.startRefreshLocked(a, time.Now(), true)
	s.refreshMu.Unlock()
	s.geminiChanged()
	return s.claudeAccount(ctx, id)
}

// AckAIRefresh 记下客户端对续期通知的回报：busy 表示本机正在用这个账号。
func (s *Server) AckAIRefresh(ctx context.Context, d Device, id string, busy bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	for _, st := range s.refresh.states {
		if st.ID == id {
			if st.acks != nil {
				st.acks[d.ID] = busy
			}
			return nil
		}
	}
	return ErrNotFound
}

// FinishAIRefresh 保存客户端交回的续期结果。成功时按发起时的版本号保存（期间别处更新过就冲突），
// 不依赖内存里的状态：管理服务重启后交回来的结果照样保存，新的 Refresh Token 不会丢。
func (s *Server) FinishAIRefresh(ctx context.Context, d Device, id string, r AIRefreshResult) error {
	a, err := s.claudeAccount(ctx, r.AccountID)
	if err != nil {
		return err
	}
	if !allowed(a, d.ID) {
		return ErrNotFound
	}
	now := time.Now()
	s.refreshMu.Lock()
	s.refresh.init()
	st := s.refresh.states[a.ID]
	if st != nil && st.ID != id {
		st = nil
	}
	s.refreshMu.Unlock()

	if r.OK && r.Credential != nil {
		c := *r.Credential
		if err := c.Normalize(a.Kind); err != nil {
			return err
		}
		if c.RefreshToken == "" {
			return &InputError{"续期结果里没有 Refresh Token"}
		}
		if e := c.Email(); e != "" && e != a.Email {
			return &InputError{fmt.Sprintf("凭据属于 %s，不是 %s", e, a.Email)}
		}
		if _, err := s.storeAICredential(ctx, a.ID, c, d.Name+" 续期", r.BaseVersion); err != nil {
			return err
		}
		_, _ = s.DB.ExecContext(ctx, `UPDATE claude_accounts SET refresh_at = ?, refresh_error = '' WHERE id = ?`, now.UTC().Format(time.RFC3339), a.ID)
		s.refreshMu.Lock()
		if cur := s.refresh.states[a.ID]; cur != nil && cur.ID == id {
			delete(s.refresh.states, a.ID)
		}
		s.refresh.done[a.ID] = now
		s.refreshMu.Unlock()
		s.geminiChanged()
		return nil
	}

	msg := strings.TrimSpace(r.Error)
	if msg == "" {
		msg = "续期失败"
	}
	if len([]rune(msg)) > maxRefreshErrorLen {
		msg = string([]rune(msg)[:maxRefreshErrorLen])
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE claude_accounts SET refresh_error = ? WHERE id = ?`, msg, a.ID)
	if st != nil {
		s.refreshMu.Lock()
		st.Phase, st.Message, st.Permanent, st.next = RefreshFailed, msg, r.Permanent, now.Add(refreshRetry)
		s.refreshMu.Unlock()
		s.geminiChanged()
	}
	if r.Permanent && s.fire(ctx, fmt.Sprintf("ai-refresh-invalid:%d:%d", a.ID, r.BaseVersion)) {
		s.Send(ctx, "ai_refresh", notify.Message{
			Title:  fmt.Sprintf("%s 的 Refresh Token 已失效：%s", kindLabel(a.Kind), a.Email),
			Body:   msg + "。需要在客户端重新登录，或在管理服务重新填入令牌。",
			Urgent: true,
		})
	}
	return nil
}
