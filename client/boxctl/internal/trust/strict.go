package trust

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"boxctl/internal/killswitch"
)

// 严格线路的出口检查：经 Mihomo 给每个严格组开的探测入口请求探测地址，看到的就是这条线路的实际出口。
// 管理服务下发了允许的出口时，任何一组探测失败或出口不在列表里，受保护的服务都阻断；没下发时只展示结果。

// StrictHealth 是严格线路检查的阈值（M9-05）。零值字段取默认值。
type StrictHealth struct {
	Interval        int `json:"interval"`         // 全部符合时的探测间隔，秒
	BlockedInterval int `json:"blocked_interval"` // 阻断时的探测间隔，秒，用来尽快恢复
	Failures        int `json:"failures"`         // 连续失败几次后阻断
	Recover         int `json:"recover"`          // 阻断后连续成功几次才恢复
}

// DefaultStrictHealth 是默认阈值。
func DefaultStrictHealth() StrictHealth {
	return StrictHealth{Interval: 30, BlockedInterval: 10, Failures: maxProbeFailures, Recover: 1}
}

// Normalize 补上默认值并限制范围：间隔太短会给线路带来不必要的流量，阈值太大会让故障迟迟不阻断。
func (h StrictHealth) Normalize() StrictHealth {
	d := DefaultStrictHealth()
	clamp := func(v, def, lo, hi int) int {
		if v <= 0 {
			return def
		}
		return min(max(v, lo), hi)
	}
	return StrictHealth{
		Interval:        clamp(h.Interval, d.Interval, 10, 600),
		BlockedInterval: clamp(h.BlockedInterval, d.BlockedInterval, 5, 120),
		Failures:        clamp(h.Failures, d.Failures, 1, 10),
		Recover:         clamp(h.Recover, d.Recover, 1, 10),
	}
}

// maxAge 是最近一次成功探测的有效期：超过它没有成功探测就阻断。默认 75 秒（两个间隔加 15 秒）。
func (h StrictHealth) maxAge() time.Duration {
	return time.Duration(h.Interval)*2*time.Second + 15*time.Second
}

// StrictTarget 是一个严格组和它的探测入口端口。
type StrictTarget struct {
	Group string
	Port  int
}

// StrictGroup 是一个严格组的探测结果。
type StrictGroup struct {
	Group   string `json:"group"`
	Egress  Egress `json:"egress"`  // 最近一次探测，含失败
	Allowed bool   `json:"allowed"` // 当前可用：探测成功，且下发了列表时出口在列表里
	Reason  string `json:"reason,omitempty"`
}

// StrictStatus 是严格线路出口检查的状态，展示用。
type StrictStatus struct {
	Required bool          `json:"required"` // 管理服务下发了允许的出口
	Allowed  []string      `json:"allowed"`
	Groups   []StrictGroup `json:"groups"`
	Reason   string        `json:"reason,omitempty"` // 阻断原因；为空表示不阻断
	// Pending 表示阻断只是因为还在探测（刚启动、线路刚切换），很快会有结论。
	Pending bool `json:"pending,omitempty"`
}

type strictState struct {
	last      Egress // 最近一次探测，含失败
	good      Egress // 最近一次成功的探测
	failures  int    // 连续失败次数
	successes int    // 连续成功次数
	held      bool   // 因连续失败而阻断，等连续成功 Recover 次后解除
}

type StrictChecker struct {
	targets   func() []StrictTarget // 内核没在运行时返回空
	allowed   func() []string
	probeURLs func() []string
	prober    func(proxy *url.URL) EgressProber
	health    func() StrictHealth
	notify    Notify
	alert     Alert
	now       func() time.Time
	kick      chan struct{}
	onChange  func() // Run 设置；Invalidate 改变阻断判断时也要立即通知

	opMu sync.Mutex // 串行化 Check

	mu      sync.Mutex
	state   map[string]*strictState
	reason  string
	pending bool
}

func NewStrictChecker(targets func() []StrictTarget, allowed, probeURLs func() []string) *StrictChecker {
	return &StrictChecker{
		targets: targets, allowed: allowed, probeURLs: probeURLs,
		prober: func(p *url.URL) EgressProber { return HTTPProber{Proxy: p} },
		health: DefaultStrictHealth,
		notify: func(string, string) {}, alert: func(string, string) {}, now: time.Now, kick: make(chan struct{}, 1),
		state: map[string]*strictState{},
	}
}

// SetHealth 设置阈值来源，每次探测时读取，必须在 Run 之前调用。
func (c *StrictChecker) SetHealth(h func() StrictHealth) {
	c.health = func() StrictHealth { return h().Normalize() }
}

// SetNotify 设置事件出口，必须在 Run 之前调用。
func (c *StrictChecker) SetNotify(n Notify) { c.notify = n }

// SetAlert 设置系统通知出口，必须在 Run 之前调用。
func (c *StrictChecker) SetAlert(a Alert) { c.alert = a }

// Run 定时探测；是否阻断变化时调用 onChange。
func (c *StrictChecker) Run(ctx context.Context, onChange func()) {
	c.mu.Lock()
	c.onChange = onChange
	c.mu.Unlock()
	for {
		if c.Check(ctx) {
			onChange()
		}
		h := c.health()
		interval := time.Duration(h.Interval) * time.Second
		if c.Status().Reason != "" {
			interval = time.Duration(h.BlockedInterval) * time.Second
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-c.kick:
			t.Stop()
		}
	}
}

// Kick 让 Run 立即探测一次。
func (c *StrictChecker) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Invalidate 作废探测结果并立即重测：线路的成员、节点或配置变了，旧的出口不再代表现在的流量。
// 管理服务下发了允许的出口时，重测完成前按「正在探测」阻断（通常一两秒），免得流量先经没确认的出口出去。
func (c *StrictChecker) Invalidate() {
	targets := c.targets()
	c.mu.Lock()
	for _, st := range c.state {
		*st = strictState{last: st.last}
	}
	s := c.statusLocked(targets)
	changed := s.Reason != c.reason
	c.reason, c.pending = s.Reason, s.Pending
	onChange := c.onChange
	c.mu.Unlock()
	if changed && onChange != nil {
		onChange()
	}
	c.Kick()
}

// Check 并行探测所有严格组。返回阻断原因是否变化。
func (c *StrictChecker) Check(ctx context.Context) bool {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	targets := c.targets()
	urls := c.probeURLs()
	results := make([]Egress, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			proxy := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(t.Port)}
			results[i] = c.prober(proxy).Probe(ctx, urls)
		}()
	}
	wg.Wait()

	required := len(c.allowed()) > 0
	h := c.health()
	var alerts []string
	c.mu.Lock()
	next := map[string]*strictState{}
	for i, t := range targets {
		st := c.state[t.Group]
		if st == nil {
			st = &strictState{}
		}
		// 只在从成功（或第一次）变成失败时通知；没下发允许的出口时失败不会阻断，不打扰。
		if required && !results[i].OK && (st.last.OK || st.last.CheckedAt.IsZero()) {
			alerts = append(alerts, fmt.Sprintf("严格线路「%s」：%s", t.Group, results[i].Error))
		}
		st.last = results[i]
		if results[i].OK {
			st.good, st.failures = results[i], 0
			st.successes++
		} else {
			st.failures, st.successes = st.failures+1, 0
		}
		if st.failures >= h.Failures {
			st.held = true
		}
		if st.held && st.successes >= h.Recover {
			st.held = false
		}
		next[t.Group] = st
	}
	c.state = next
	prev, prevPending := c.reason, c.pending
	s := c.statusLocked(targets)
	c.reason, c.pending = s.Reason, s.Pending
	reason := c.reason
	c.mu.Unlock()

	for _, a := range alerts {
		c.alert("出口探测失败", a)
	}
	if reason == prev {
		return false
	}
	switch {
	case reason != "" && !s.Pending:
		c.notify("warn", reason)
	case reason == "" && prev != "" && !prevPending:
		c.notify("info", "严格线路的出口都符合要求")
	}
	return true
}

func (c *StrictChecker) Status() StrictStatus {
	targets := c.targets()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked(targets)
}

func (c *StrictChecker) statusLocked(targets []StrictTarget) StrictStatus {
	allowed := c.allowed()
	s := StrictStatus{Required: len(allowed) > 0, Allowed: append([]string{}, allowed...), Groups: []StrictGroup{}}
	now := c.now()
	maxAge := c.health().maxAge()
	for _, t := range targets {
		g := StrictGroup{Group: t.Group}
		pending := false
		st := c.state[t.Group]
		if st != nil {
			g.Egress = st.last
		}
		switch {
		case st == nil:
			g.Reason, pending = fmt.Sprintf("正在探测严格线路「%s」的出口", t.Group), true
		case !st.good.OK || st.held || now.Sub(st.good.CheckedAt) > maxAge:
			if st.last.Error != "" {
				g.Reason = fmt.Sprintf("严格线路「%s」出口探测失败：%s", t.Group, st.last.Error)
			} else if st.held {
				g.Reason = fmt.Sprintf("严格线路「%s」刚恢复，等待连续 %d 次探测成功", t.Group, c.health().Recover)
			} else {
				g.Reason, pending = fmt.Sprintf("正在探测严格线路「%s」的出口", t.Group), true
			}
		case s.Required && !egressAllowed(allowed, st.good.IP):
			g.Reason = fmt.Sprintf("严格线路「%s」的出口 %s 不在管理服务允许的列表里", t.Group, st.good.IP)
		default:
			g.Allowed = true
		}
		if s.Required && !g.Allowed && (s.Reason == "" || s.Pending && !pending) {
			s.Reason, s.Pending = g.Reason, pending // 确定的阻断原因优先于「正在探测」
		}
		s.Groups = append(s.Groups, g)
	}
	return s
}

func egressAllowed(list []string, ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && matchAny(list, a)
}

// StrictPolicy 在 Inner 判断经本机内核时，再看严格线路的出口：不符合就阻断。
// 直连和阻断时流量不经严格组，出口检查没有意义。
type StrictPolicy struct {
	Inner  killswitch.Policy
	Strict *StrictChecker
}

func (p StrictPolicy) Verdict(tun bool) killswitch.Verdict {
	v := p.Inner.Verdict(tun)
	if v.Path != killswitch.PathProxy {
		return v
	}
	if s := p.Strict.Status(); s.Reason != "" {
		return killswitch.Verdict{Path: killswitch.PathHold, Reason: s.Reason, Pending: s.Pending}
	}
	return v
}
