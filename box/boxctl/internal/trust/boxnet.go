package trust

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"boxctl/internal/boxfind"
)

// 路由盒自动发现（ADR-073）：除了受信 Wi-Fi 名单，在当前网络上找到并验证了路由盒也算受信网络（路由盒网络）。
// 换网络后立即找一次，之后每 boxRecheck 复查；连续 boxMaxFailures 次复查失败才认为路由盒不在了
// （偶发超时不立即切走；切走之前闸门的逐连接出口确认仍然兜底）。出口基准按路由盒 ID 记，不按 Wi-Fi 名。

const (
	boxRecheck     = 30 * time.Second
	boxMissRetry   = 15 * time.Second // 没找到时多久再找一次（路由盒可能刚开机）
	boxMaxFailures = 2
)

// BoxStatus 是路由盒发现的状态，展示用。
type BoxStatus struct {
	Enabled   bool      `json:"enabled"` // 有口令（管理服务下发了）
	ID        string    `json:"id,omitempty"`
	Addr      string    `json:"addr,omitempty"`
	Error     string    `json:"error,omitempty"` // 最近一次验证失败的原因；只是没找到时为空
	CheckedAt time.Time `json:"checked_at"`
	// 以下是排查用的详情（「高级 → 路由盒发现」）。
	Name      string        `json:"name"`      // 查询的域名
	Port      int           `json:"port"`      // 挑战应答端口
	Interface string        `json:"interface"` // 最近一次发现用的网卡
	DNS       []string      `json:"dns"`       // 最近一次问的 DNS 服务器（物理网卡的，已去掉 TUN 网段）
	Trace     boxfind.Trace `json:"trace"`     // 最近一次发现的过程
	Fails     int           `json:"fails"`     // 连续失败次数，达到 MaxFailures 才认为路由盒不在了
	MaxFails  int           `json:"max_fails"`
	Busy      bool          `json:"busy"`             // 正在发现
	NextAt    time.Time     `json:"next_at,omitzero"` // 预计下次发现的时间
}

// BoxFinder 在 servers（物理网卡的 DNS）上找路由盒并验证。测试替换它。
type BoxFinder func(ctx context.Context, servers []netip.Addr, secret string) (boxfind.Box, error)

// SetBoxDiscovery 打开路由盒发现：secret 返回当前口令（为空时不找），servers 按网卡名读 DNS 服务器。
// 必须在 Run 之前调用。
func (e *Evaluator) SetBoxDiscovery(secret func() string, servers func(iface string) []netip.Addr) {
	e.boxSecret, e.boxServers = secret, servers
}

// SetBoxHello 让客户端每次验证通过路由盒后报一次自己的名字、系统和版本（路由盒设备页显示）。必须在 Run 之前调用。
func (e *Evaluator) SetBoxHello(hello func() boxfind.Hello) { e.boxHello = hello }

func (e *Evaluator) discoveryOn() bool { return e.boxSecret != nil && e.boxSecret() != "" }

// boxDueLocked 判断现在该不该找路由盒，调用时持有 e.mu。
func (e *Evaluator) boxDueLocked(n Network) bool {
	if !e.discoveryOn() || n.Interface == "" || e.boxBusy {
		return false
	}
	if e.boxAt.IsZero() {
		return true
	}
	interval := boxRecheck
	if e.box.ID == "" {
		interval = boxMissRetry
	}
	return e.now().Sub(e.boxAt) >= interval
}

// startBoxFind 在后台找路由盒，结果变化时 Kick。调用时持有 e.mu。
func (e *Evaluator) startBoxFindLocked(ctx context.Context, iface string) {
	gen, secret := e.gen, e.boxSecret()
	e.boxBusy = true
	e.probeWG.Add(1)
	go func() {
		defer e.probeWG.Done()
		var servers []netip.Addr
		for _, a := range e.boxServers(iface) {
			if a.IsValid() && !tunRange.Contains(a.Unmap()) {
				servers = append(servers, a)
			}
		}
		var tr boxfind.Trace
		b, err := e.findBox(boxfind.WithTrace(ctx, &tr), servers, secret)
		if err == nil && !boxfind.ValidID(b.ID) {
			err = errors.New("路由盒 ID 不合法")
		}
		if err == nil {
			err = e.boxProxy(ctx, b.Addr.Addr(), secret)
		}
		e.mu.Lock()
		if gen != e.gen {
			e.mu.Unlock()
			return
		}
		e.boxBusy, e.boxAt = false, e.now()
		e.boxIface, e.boxTrace = iface, tr
		e.boxDNS = make([]string, 0, len(servers))
		for _, a := range servers {
			e.boxDNS = append(e.boxDNS, a.String())
		}
		prev := e.box
		if err == nil {
			e.box, e.boxFails, e.boxErr = b, 0, ""
		} else {
			e.boxFails++
			e.boxErr = err.Error()
			if errors.Is(err, boxfind.ErrNotFound) {
				e.boxErr = "" // 名字解析不到：这个网络上没有路由盒，不算错误
			}
			if prev.ID == "" || e.boxFails >= boxMaxFailures {
				e.box = boxfind.Box{}
			}
		}
		cur := e.box
		if cur.ID != prev.ID {
			// 换了路由盒（或第一次找到、不在了）：出口结果属于原来的网络，作废重来。
			e.resetEgressLocked()
		}
		e.mu.Unlock()
		switch {
		case cur.ID != "" && cur.ID != prev.ID:
			e.event("info", fmt.Sprintf("发现路由盒「%s」（%s），验证通过", cur.ID, cur.Addr.Addr()))
		case cur.ID == "" && prev.ID != "":
			e.event("warn", fmt.Sprintf("路由盒「%s」不在了：%s", prev.ID, err))
		case cur.ID == "" && err != nil && !errors.Is(err, boxfind.ErrNotFound) && e.boxFails == 1:
			e.event("warn", "路由盒验证失败："+err.Error())
		}
		if cur.ID != prev.ID {
			e.Kick()
		}
		// 每次复查通过都报一次：路由盒据此显示接入的客户端，记录在内存里，重启后靠下一次复查补上。
		if err == nil && e.boxHello != nil {
			if herr := e.sendHello(ctx, b.Addr, secret, e.boxHello()); herr != nil && e.log != nil {
				e.log.Debug("向路由盒报客户端信息失败（旧版路由盒不支持）", "err", herr)
			}
		}
	}()
}

// clearBoxLocked 在网络变化时清掉发现结果，调用时持有 e.mu。
func (e *Evaluator) clearBoxLocked() {
	e.box, e.boxAt, e.boxFails, e.boxErr, e.boxBusy = boxfind.Box{}, time.Time{}, 0, "", false
	e.boxIface, e.boxDNS, e.boxTrace = "", nil, boxfind.Trace{}
}

// RecheckBox 让下一轮立即重新找路由盒（「高级 → 路由盒发现」的按钮）。正在找时不重复。
func (e *Evaluator) RecheckBox() {
	e.mu.Lock()
	if !e.boxBusy {
		e.boxAt = time.Time{}
	}
	e.mu.Unlock()
	e.Kick()
}

func (e *Evaluator) boxStatusLocked() BoxStatus {
	s := BoxStatus{Enabled: e.discoveryOn(), ID: e.box.ID, Error: e.boxErr, CheckedAt: e.boxAt,
		Name: boxfind.Name, Port: boxfind.Port, Interface: e.boxIface, DNS: append([]string{}, e.boxDNS...),
		Trace: e.boxTrace, Fails: e.boxFails, MaxFails: boxMaxFailures, Busy: e.boxBusy}
	if e.box.ID != "" {
		s.Addr = e.box.Addr.Addr().String()
	}
	if s.Enabled && !e.boxAt.IsZero() && !e.boxBusy {
		interval := boxRecheck
		if e.box.ID == "" {
			interval = boxMissRetry
		}
		s.NextAt = e.boxAt.Add(interval)
	}
	return s
}

// netKey 是当前受信网络的标识：路由盒按 ID，否则按 Wi-Fi 名称。出口基准按它记。
func netKey(n Network, box boxfind.Box) string {
	if box.ID != "" {
		return "路由盒:" + box.ID
	}
	return n.SSID
}

// netLabel 是当前受信网络的说明，用在事件和原因里。
func netLabel(n Network, box boxfind.Box) string {
	if box.ID != "" {
		return fmt.Sprintf("路由盒「%s」", box.ID)
	}
	return fmt.Sprintf("Wi-Fi「%s」", n.SSID)
}
