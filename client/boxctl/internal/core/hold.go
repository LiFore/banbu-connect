package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"boxctl/internal/gen"
)

// 内核里的 hold（路由盒）：生成器在每条指向严格组的规则前放了一条指向拒绝出站的副本（gen.HoldRules）。
// 内核加载配置时副本生效，也就是先阻断；面板按严格线路的检查结果用 PATCH /rules/disable 开关它们。
// guard 的 nft hold 只能拦严格集合里的地址；经加密 DNS 拿到地址的连接靠这里拦。

// SetHold 打开（hold 为 true）或关闭内核里的 hold 副本。内核没在运行时什么也不做。
// 同一个内核进程、同一份配置、同一个状态只下发一次；从放行变为阻断时，断开已经经严格组建立的连接。
func (s *Supervisor) SetHold(ctx context.Context, hold bool) error {
	key, ok := s.holdState(hold)
	if !ok {
		return nil
	}
	s.mu.Lock()
	same, wasHold := key == s.holdKey, s.holdOn
	s.mu.Unlock()
	if same {
		return nil
	}

	code, body, err := s.CallCore(ctx, http.MethodGet, "/rules", nil)
	if err != nil {
		return fmt.Errorf("读取内核规则失败：%w", err)
	}
	if code >= 300 {
		return fmt.Errorf("读取内核规则失败：HTTP %d", code)
	}
	var list struct {
		Rules []struct {
			Index int    `json:"index"`
			Proxy string `json:"proxy"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("读取内核规则失败：%w", err)
	}
	payload := map[int]bool{}
	for _, r := range list.Rules {
		if gen.IsHoldTarget(r.Proxy) {
			payload[r.Index] = !hold // 值是 disabled
		}
	}
	if len(payload) > 0 {
		b, _ := json.Marshal(payload)
		code, body, err = s.CallCore(ctx, http.MethodPatch, "/rules/disable", b)
		if err != nil {
			return fmt.Errorf("切换内核里的阻断规则失败：%w", err)
		}
		if code >= 300 {
			return fmt.Errorf("切换内核里的阻断规则失败：HTTP %d %s", code, body)
		}
	}
	// 期间内核换了配置或进程：索引可能对不上，不记下，下一次下发重做。
	if now, ok := s.holdState(hold); !ok || now != key {
		return fmt.Errorf("内核配置在切换阻断规则时变了，稍后重试")
	}
	s.mu.Lock()
	s.holdKey, s.holdOn = key, hold
	s.mu.Unlock()
	if hold && !wasHold {
		s.closeStrictConns(ctx)
	}
	return nil
}

// HoldRules 报告内核里的 hold 副本是否打开（最近一次成功下发的状态）；内核没在运行时为 false。
func (s *Supervisor) HoldRules() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliveLocked() && s.holdKey != "" && s.holdOn
}

func (s *Supervisor) holdState(hold bool) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.aliveLocked() {
		return "", false
	}
	return fmt.Sprintf("%d|%d|%s|%v", s.coreGen, s.pidLocked(), ConfigHash(s.applied), hold), true
}

// closeStrictConns 断开经严格组的连接：阻断前已经建立的长连接不会再经不符合要求的出口继续传数据。
func (s *Supervisor) closeStrictConns(ctx context.Context) {
	_, groups := s.GatePorts()
	if len(groups) == 0 {
		return
	}
	n := s.CloseConns(ctx, func(chains []string, _ string) bool {
		return slices.ContainsFunc(chains, func(ch string) bool { _, ok := groups[ch]; return ok })
	})
	if n > 0 {
		s.log.Warn("严格线路阻断，已断开经严格组的连接", "count", n)
	}
}

// CloseConns 断开内核里 match 选中的连接（按出站链和入站监听名），返回断开的条数。内核没在运行时为 0。
// 内核换线路（选择组切换、配置重载）不会断开已经建立的连接，判断变了要靠这里把旧连接断掉。
func (s *Supervisor) CloseConns(ctx context.Context, match func(chains []string, inbound string) bool) int {
	code, body, err := s.CallCore(ctx, http.MethodGet, "/connections", nil)
	if err != nil || code >= 300 {
		return 0
	}
	var list RawConnections
	if json.Unmarshal(body, &list) != nil {
		return 0
	}
	n := 0
	for _, c := range list.Connections {
		if match(c.Chains, c.Metadata.InboundName) {
			if code, _, err := s.CallCore(ctx, http.MethodDelete, "/connections/"+c.ID, nil); err == nil && code < 300 {
				n++
			}
		}
	}
	return n
}
