package killswitch

import (
	"time"
)

// 系统通知：受保护的服务被阻断、恢复放行、出口探测失败。
// Engine 只把通知排进 Status.Notices，由托盘外壳（用户会话里）轮询后弹出系统通知。

const (
	NoticeBlocked     = "blocked"
	NoticeRestored    = "restored"
	NoticeProbeFailed = "probe_failed"
	NoticeAutoStart   = "auto_start"   // 离开受信网络后自动启动了代理
	NoticeRulesFailed = "rules_failed" // 应该有规则（经内核或阻断），但规则没能写到系统上

	maxNotices = 20
	// 正在确认出口（刚连上受信 Wi-Fi、切换了严格线路）、内核重启这类阻断通常几秒就结束，
	// 持续这么久才通知，免得每次换网络都弹「阻断」再弹「恢复」。确定的阻断（出口不符合、探测失败）立即通知。
	noticeGrace = 15 * time.Second
)

// Notice 是一条待弹出的系统通知。
type Notice struct {
	// ID 单调递增，取自纳秒时间戳：服务重启后也不会回退，外壳只弹比上次看到的更大的。
	ID    int64     `json:"id"`
	Kind  string    `json:"kind"`
	Title string    `json:"title"`
	Body  string    `json:"body"`
	Time  time.Time `json:"time"`
}

type noticeState struct {
	now      func() time.Time
	list     []Notice
	lastID   int64
	since    time.Time // 这次阻断开始的时间；没有阻断时为零
	notified bool      // 这次阻断已经通知过
	failed   bool      // 这次「规则没写上」已经通知过
}

// add 排一条通知，调用时持有 e.mu。
func (n *noticeState) add(kind, title, body string) {
	now := n.now()
	id := now.UnixNano()
	if id <= n.lastID {
		id = n.lastID + 1
	}
	n.lastID = id
	n.list = append(n.list, Notice{ID: id, Kind: kind, Title: title, Body: body, Time: now.UTC()})
	if len(n.list) > maxNotices {
		n.list = n.list[len(n.list)-maxNotices:]
	}
}

// Notify 排一条系统通知，供 Policy 报告出口探测失败。保护没有启用时不通知：那时探测失败不会阻断任何东西。
func (e *Engine) Notify(kind, title, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Enabled {
		e.notices.add(kind, title, body)
	}
}

// trackBlocked 按当前状态决定是否通知阻断或恢复放行。不能在持有 e.mu 时调用。
func (e *Engine) trackBlocked() {
	s := e.Status()
	if s.State == StateError {
		// 规则状态不明，不据此判断阻断与否；但应该有规则时要让用户知道：这时可能完全没有保护。
		// 看最新的判断而不是已生效的：从直连换到经内核时写规则失败，已生效的仍是直连。
		live := e.liveVerdict()
		e.mu.Lock()
		if !e.notices.failed && live.Path != PathDirect {
			e.notices.add(NoticeRulesFailed, "保护规则没有生效", failedBody(s))
			e.notices.failed = true
		}
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	e.notices.failed = false
	e.mu.Unlock()
	blocked := s.State == StateHeld || s.State == StateBlocking
	e.mu.Lock()
	defer e.mu.Unlock()
	n := &e.notices
	if !blocked {
		if n.notified && s.Enabled {
			n.add(NoticeRestored, "已恢复访问", restoredBody(s))
		}
		n.since, n.notified = time.Time{}, false
		return
	}
	now := n.now()
	if n.since.IsZero() {
		n.since = now
	}
	definite := s.State == StateHeld && !s.Verdict.Pending
	if !n.notified && (definite || now.Sub(n.since) >= noticeGrace) {
		n.add(NoticeBlocked, "已阻断受保护的服务", blockedBody(s))
		n.notified = true
	}
}

func failedBody(s Status) string {
	if s.LastError != "" {
		return s.LastError + "。受保护的服务可能直连，正在重试"
	}
	return "系统规则与配置不一致，受保护的服务可能直连，正在重试"
}

func blockedBody(s Status) string {
	if s.State == StateBlocking {
		if s.Health.DNSDetail != "" {
			return "代理内核不可用：" + s.Health.DNSDetail
		}
		return "代理内核不可用"
	}
	if s.Verdict.Reason != "" {
		return s.Verdict.Reason
	}
	return "当前网络的出口还没有确认"
}

func restoredBody(s Status) string {
	switch s.State {
	case StateDirect:
		return "受信网络，直连放行：" + s.Verdict.Reason
	case StateProxyOnly:
		return "经本机内核访问（TUN 未接管，只有走系统代理的应用可以访问）"
	default:
		return "经本机内核访问受保护的服务"
	}
}

// Announce 排一条系统通知，不论保护是否启用：用于用户自己打开的提醒（如离开受信网络后自动启动代理）。
func (e *Engine) Announce(kind, title, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notices.add(kind, title, body)
}
