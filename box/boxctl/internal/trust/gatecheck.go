package trust

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"boxctl/internal/killswitch"
)

// 闸门直连的逐连接确认：受保护服务的每个新连接到达出口闸门时，确认「这个服务此刻经受信网络的出口」符合要求。
//
// 参照 ippure 的出口检测：同一时刻、不同站点看到的出口可以不同（路由盒按域名分流），
// 所以按服务分别请求它自己的 /cdn-cgi/trace，而不是只看一个中立地址。
// 结果在 gateTTL 内复用（连接很密时不必每次都探测），过了一半就在后台刷新；
// 过期、网络变化后第一个连接同步探测，同一服务同时只探测一次，其他连接等它的结果。

const (
	gateTTL          = 10 * time.Second
	gateProbeTimeout = 3 * time.Second
)

// GateGroup 是一个受保护服务（按严格组）最近一次的逐连接确认，展示用。
type GateGroup struct {
	Group   string `json:"group"`
	URL     string `json:"url"`
	Egress  Egress `json:"egress"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
	// Direct、Proxied 是经闸门直连、改经代理的连接数（服务启动以来）。
	Direct  int64 `json:"direct"`
	Proxied int64 `json:"proxied"`
}

type GateStatus struct {
	Enabled bool        `json:"enabled"` // 设置里打开了闸门直连
	Active  bool        `json:"active"`  // 内核的 DIRECT 入口可用
	Groups  []GateGroup `json:"groups"`
}

type gateState struct {
	mu      sync.Mutex // 同一服务同时只探测一次
	gen     uint64     // 探测时的网络代次；网络变了结果作废
	last    Egress
	good    Egress
	allowed bool
	reason  string
	direct  int64
	proxied int64
	refresh bool // 后台刷新进行中
}

func (e *Evaluator) gateState(group string) *gateState {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	st := e.gates[group]
	if st == nil {
		st = &gateState{}
		e.gates[group] = st
	}
	return st
}

func (e *Evaluator) resetGates() {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	e.gates = map[string]*gateState{}
}

// GateAllow 在出口闸门收到某个严格组（受保护服务）的新连接时调用。
// 返回 true 时这个连接经内核的 DIRECT 直连（交给路由盒）；false 时经该组代理，reason 说明原因。
// 任何不确定（网络刚变、探测失败、超时、出口不符）都返回 false：经代理总是安全的。
func (e *Evaluator) GateAllow(ctx context.Context, group string) (bool, string) {
	cfg := e.config()
	if !e.gateActive(cfg) {
		return false, "没有开启闸门直连"
	}
	// 唤醒后第一个连接可能早于下一次 Tick：先判断一次，免得用睡前的确认放行。
	e.checkWake()
	e.mu.Lock()
	tun, gen := e.tun, e.gen
	e.mu.Unlock()
	if v := e.Verdict(tun); v.Path != killswitch.PathDirect {
		return e.gateCount(group, false, v.Reason)
	}
	if e.OnBox() {
		return e.gateCount(group, true, "") // 路由盒接管：按路由盒的策略走，不逐服务探测（ADR-082）
	}
	u := e.gateURLs()[group]
	if u == "" {
		return e.gateCount(group, true, "") // 没有这个服务的探测地址：以网络级的出口确认为准
	}
	st := e.gateState(group)
	st.mu.Lock()
	age := e.now().Sub(st.last.CheckedAt)
	fresh := st.gen == gen && !st.last.CheckedAt.IsZero() && age < gateTTL
	if fresh {
		allowed, reason := st.allowed, st.reason
		if age > gateTTL/2 && !st.refresh {
			st.refresh = true
			go e.gateRefresh(group, u)
		}
		st.mu.Unlock()
		return e.gateCount(group, allowed, reason)
	}
	allowed, reason := e.gateProbeLocked(ctx, st, group, u, gen)
	st.mu.Unlock()
	return e.gateCount(group, allowed, reason)
}

// gateStillAllowed 不探测、不计数，只看现有结果：这个组此刻是否仍可经闸门直连。闸门登记连接后用它复核。
func (e *Evaluator) gateStillAllowed(group string) (bool, string) {
	if !e.gateActive(e.config()) {
		return false, "没有开启闸门直连"
	}
	e.mu.Lock()
	tun, gen := e.tun, e.gen
	e.mu.Unlock()
	if v := e.Verdict(tun); v.Path != killswitch.PathDirect {
		return false, v.Reason
	}
	if e.OnBox() || e.gateURLs()[group] == "" {
		return true, ""
	}
	st := e.gateState(group)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.gen == gen && st.allowed, st.reason
}

func (e *Evaluator) gateRefresh(group, u string) {
	st := e.gateState(group)
	st.mu.Lock()
	defer st.mu.Unlock()
	e.mu.Lock()
	gen := e.gen
	e.mu.Unlock()
	e.gateProbeLocked(context.Background(), st, group, u, gen)
	st.refresh = false
}

// gateProbeLocked 同步探测这个服务经 DIRECT 的出口，调用时持有 st.mu。
func (e *Evaluator) gateProbeLocked(ctx context.Context, st *gateState, group, u string, gen uint64) (bool, string) {
	direct := e.direct()
	if direct == nil {
		return false, "内核的 DIRECT 入口不可用"
	}
	ctx, cancel := context.WithTimeout(ctx, gateProbeTimeout)
	defer cancel()
	eg := e.viaProxy(direct).Probe(ctx, []string{u})
	e.mu.Lock()
	sameNet, key, label, cfg := e.gen == gen, netKey(e.net, e.box), netLabel(e.net, e.box), cloneConfig(e.cfg)
	e.mu.Unlock()
	if !sameNet {
		return false, "网络变化，正在确认新网络"
	}
	prevAllowed, prevReason := st.allowed, st.reason
	st.gen, st.last = gen, eg
	if eg.OK {
		st.good = eg
		st.allowed, st.reason = e.gateMatch(cfg, key, label, group, eg)
	} else {
		st.allowed, st.reason = false, "出口探测失败："+eg.Error
	}
	if prevAllowed && !st.allowed && e.gateRevoke != nil {
		// 出口不再符合：已经经闸门直连的连接也断开，不让它们继续经这个出口传数据。
		e.gateRevoke(group)
	}
	if st.allowed != prevAllowed || st.reason != prevReason {
		switch {
		case !st.allowed:
			e.event("warn", fmt.Sprintf("「%s」暂不直连，改经代理：%s", group, st.reason))
		case !prevAllowed && prevReason != "":
			e.event("info", fmt.Sprintf("「%s」的出口 %s 已确认，恢复经受信网络直连", group, eg.IP))
		}
	}
	return st.allowed, st.reason
}

// gateMatch 判断这个服务的出口：设置了允许列表时按列表；否则与这个网络（Wi-Fi 或路由盒，key）上这个服务
// 记住的出口比较，第一次见到时记下来。地区允许列表同样适用。
func (e *Evaluator) gateMatch(cfg Config, key, label, group string, eg Egress) (bool, string) {
	ip, err := netip.ParseAddr(eg.IP)
	if err != nil {
		return false, "出口 IP 无效：" + eg.IP
	}
	if len(cfg.ExpectedEgress) > 0 {
		if !matchAny(cfg.ExpectedEgress, ip) {
			return false, fmt.Sprintf("出口 %s 不在允许列表里", eg.IP)
		}
	} else if key != "" {
		sk := serviceKey(key, group)
		switch base := cfg.Baselines[sk]; {
		case base == "":
			next := cloneConfig(cfg)
			next.Baselines[sk] = eg.IP
			if err := e.store(next); err != nil {
				return false, err.Error()
			}
			e.event("info", fmt.Sprintf("已记住%s上「%s」的出口 %s", label, group, eg.IP))
		case base != eg.IP:
			return false, fmt.Sprintf("出口从 %s 变为 %s；确认无误后在受信网络里接受新出口", base, eg.IP)
		}
	}
	if len(cfg.AllowedCountries) > 0 && !slices.Contains(cfg.AllowedCountries, eg.Country) {
		c := eg.Country
		if c == "" {
			c = "未知"
		}
		return false, fmt.Sprintf("出口地区 %s 不在允许列表里", c)
	}
	return true, ""
}

func (e *Evaluator) gateCount(group string, allowed bool, reason string) (bool, string) {
	st := e.gateState(group)
	st.mu.Lock()
	if allowed {
		st.direct++
	} else {
		st.proxied++
	}
	st.mu.Unlock()
	return allowed, reason
}

func (e *Evaluator) gateStatus() GateStatus {
	cfg := e.config()
	s := GateStatus{Enabled: cfg.GateDirect, Active: e.gateActive(cfg), Groups: []GateGroup{}}
	urls := e.gateURLs()
	e.gateMu.Lock()
	groups := make([]string, 0, len(e.gates)+len(urls))
	for g := range e.gates {
		groups = append(groups, g)
	}
	states := make(map[string]*gateState, len(e.gates))
	for g, st := range e.gates {
		states[g] = st
	}
	e.gateMu.Unlock()
	for g := range urls {
		if !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	slices.Sort(groups)
	for _, g := range groups {
		gg := GateGroup{Group: g, URL: urls[g]}
		if st := states[g]; st != nil {
			st.mu.Lock()
			gg.Egress, gg.Allowed, gg.Reason, gg.Direct, gg.Proxied = st.last, st.allowed, st.reason, st.direct, st.proxied
			st.mu.Unlock()
		}
		s.Groups = append(s.Groups, gg)
	}
	return s
}

// serviceKey 是「某个网络（Wi-Fi 名称或「路由盒:ID」）上某个服务」记住的出口在 Baselines 里的键。SSID 不含「#」以外的限制，
// 用最后一个「#」分隔，组名不含「#」。
func serviceKey(ssid, group string) string { return ssid + "#" + group }

func splitServiceKey(k string) (ssid, group string, ok bool) {
	i := strings.LastIndex(k, "#")
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}
