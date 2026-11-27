package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// 内核检查（ADR-045）：内核是独立服务，guard 不依赖面板，自己每 500 毫秒检查一次：
// 控制接口（GET /version）、TUN 网卡、DNS 监听。用户启用了内核（策略 CoreExpected）而检查失败时，
// 第一次失败就阻断：受保护服务存在时局域网除了访问路由盒和内网之外全部丢弃（core_down 链）。
// 连续 coreRecover 次成功才解除，避免抖动时反复放通。

const (
	corePeriod  = 500 * time.Millisecond
	coreTimeout = 400 * time.Millisecond
	coreRecover = 2
)

// CoreHealth 是 guard 对内核的检查结果。
type CoreHealth struct {
	Expected  bool      `json:"expected"` // 用户启用了内核
	Healthy   bool      `json:"healthy"`
	Blocking  bool      `json:"blocking"` // 正在因内核异常阻断局域网
	Reason    string    `json:"reason,omitempty"`
	Version   string    `json:"version,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Since     time.Time `json:"since,omitempty"` // 当前的健康 / 异常状态从什么时候开始
	Failures  int       `json:"failures"`        // 连续失败次数
	Checks    int64     `json:"checks"`
	FailTotal int64     `json:"fail_total"`
	Blocks    int       `json:"blocks"` // guard 运行以来因内核异常阻断的次数
	BlockedAt time.Time `json:"blocked_at,omitempty"`
}

// coreProber 检查一次内核，返回版本；不正常时返回原因。
type coreProber func(ctx context.Context, p Policy) (version string, err error)

func httpCoreProbe(secretFile string, fake bool) coreProber {
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	return func(ctx context.Context, p Policy) (string, error) {
		if p.CoreController == "" {
			return "", errors.New("面板还没有下发内核地址")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.CoreController+"/version", nil)
		if err != nil {
			return "", err
		}
		if b, err := os.ReadFile(secretFile); err == nil {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", errors.New("内核离线：控制接口没有响应")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("内核异常：控制接口返回 HTTP %d", resp.StatusCode)
		}
		var v struct {
			Version string `json:"version"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&v)
		if !fake {
			ifi, err := net.InterfaceByName(TUNDevice)
			if err != nil || ifi.Flags&net.FlagUp == 0 {
				return v.Version, errors.New("内核异常：TUN 网卡 " + TUNDevice + " 不在或没有启用，局域网流量没有经过内核")
			}
		}
		if p.DNSUpstream != "" {
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp", p.DNSUpstream)
			if err != nil {
				return v.Version, errors.New("内核异常：DNS 监听 " + p.DNSUpstream + " 没有响应")
			}
			c.Close()
		}
		return v.Version, nil
	}
}

// RunCoreMonitor 每 500 毫秒检查一次内核，直到 ctx 结束。
func (e *Engine) RunCoreMonitor(ctx context.Context) {
	t := time.NewTicker(corePeriod)
	defer t.Stop()
	for {
		e.coreTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// coreTick 检查一次内核；阻断状态变化时立即重装规则。
func (e *Engine) coreTick(ctx context.Context) {
	if e.coreProbe == nil {
		return
	}
	e.mu.Lock()
	p := e.policy
	e.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, coreTimeout)
	start := time.Now()
	version, err := e.coreProbe(cctx, p)
	cancel()
	now := time.Now().UTC()

	e.mu.Lock()
	h := &e.core
	healthyBefore := h.Healthy || h.CheckedAt.IsZero()
	h.Expected, h.CheckedAt, h.LatencyMS = p.CoreExpected, now, time.Since(start).Milliseconds()
	h.Checks++
	if version != "" {
		h.Version = version
	}
	if err == nil {
		h.Healthy, h.Reason, h.Failures = true, "", 0
		e.okStreak++
	} else {
		h.Healthy, h.Reason = false, err.Error()
		h.Failures++
		h.FailTotal++
		e.okStreak = 0
	}
	if h.Healthy != healthyBefore || h.Since.IsZero() {
		h.Since = now
	}
	// 只在用户启用了内核、且有受保护服务时阻断：用户自己停掉内核时局域网照常直连（受保护服务仍被严格集合拦着）。
	want := e.coreDown
	switch {
	case !p.CoreExpected || len(p.Suffixes) == 0:
		want = false
	case err != nil:
		want = true // 第一次失败就阻断
	case e.okStreak >= coreRecover:
		want = false
	}
	changed := want != e.coreDown
	if changed {
		e.coreDown = want
		h.Blocking = want
		if want {
			h.Blocks++
			h.BlockedAt = now
		}
	}
	reason := h.Reason
	e.mu.Unlock()

	if !changed {
		return
	}
	// 装的时候重新取策略：等锁期间面板可能刚下发了新的。
	e.instMu.Lock()
	e.mu.Lock()
	cur := e.policy
	eff := e.effectiveLocked(cur)
	e.mu.Unlock()
	err = e.install(ctx, eff, cur.Version())
	e.instMu.Unlock()
	if err != nil {
		return
	}
	if want {
		e.record("error", "内核离线或异常，已阻断局域网（只放行访问路由盒和内网）："+reason)
	} else {
		e.record("info", "内核恢复正常，已解除阻断")
	}
}

// counterReader 是能读出 nft 计数器的系统。
type counterReader interface {
	Counters(ctx context.Context) map[string]uint64
}

// Counters 返回 guard 表里各条链丢弃的包数（带 counter 的规则累加）。
func (execSystem) Counters(ctx context.Context) map[string]uint64 {
	out, err := run(ctx, "", "nft", "-j", "list", "table", "inet", Table)
	if err != nil {
		return nil
	}
	return parseCounters([]byte(out))
}

func parseCounters(b []byte) map[string]uint64 {
	var doc struct {
		Nftables []struct {
			Rule *struct {
				Chain string            `json:"chain"`
				Expr  []json.RawMessage `json:"expr"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	out := map[string]uint64{}
	for _, item := range doc.Nftables {
		if item.Rule == nil {
			continue
		}
		chain := item.Rule.Chain
		if strings.HasPrefix(chain, "dev_") {
			chain = "device_block" // 每台设备一条链，合在一起
		}
		for _, raw := range item.Rule.Expr {
			var x struct {
				Counter *struct {
					Packets uint64 `json:"packets"`
				} `json:"counter"`
			}
			if json.Unmarshal(raw, &x) == nil && x.Counter != nil {
				out[chain] += x.Counter.Packets
			}
		}
	}
	return out
}
