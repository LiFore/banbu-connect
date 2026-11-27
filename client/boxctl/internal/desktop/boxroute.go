package desktop

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/gen"
)

func sameEndpoint(a, b *gen.ProxyEndpoint) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// boxRouter 按当前网络决定流量交给谁（ADR-073、ADR-082、ADR-084）。开了闸门直连时：
//   - 发现并验证了路由盒（含代理入口）：重建内核配置，除 DIRECT 外都经路由盒的代理入口（gen.Input.Box），
//     按路由盒的策略走，不看出口探测；路由盒换了（地址变化）也重建。
//   - 在受信 Wi-Fi 上（没有路由盒）且出口确认通过：「路由盒切换」选择组（gen.BoxRoutePrefix）选 DIRECT；
//     受保护服务由出口闸门逐连接确认。
//   - 其他时候选原来的目标，按本机规则走。
//
// 网络判断一变（含路由变化时立即作废）就同步一次；新配置生效后也同步（内核重启会恢复上次保存的选择）；
// 另外每 boxRouteEvery 核对一次，防止有人在线路页手动改了它。
type boxRouter struct {
	sup     *core.Supervisor
	direct  func() bool               // 交给路由盒或受信 Wi-Fi 直连
	box     func() *gen.ProxyEndpoint // 当前路由盒的代理入口；不在路由盒网络上时为 nil
	kick    chan struct{}
	log     *slog.Logger
	last    bool                              // 上一次同步的方向，只用于日志
	lastBox atomic.Pointer[gen.ProxyEndpoint] // 上一次同步时的路由盒；变化时重建内核配置
	// revokeGate 断开经出口闸门直连的连接（gate.Revoke("")）。
	revokeGate func(group string) int
	// wasDirect 是上一次同步时是否直连（只在 Run 的协程里读写）；从直连变为不直连时断开旧连接。
	wasDirect bool
	// drainUntil 之前持续断开经旧路由盒入口的连接：离开路由盒网络后内核要等配置重建（防抖 + 重载）才不再用它。
	drainUntil time.Time
}

// boxDrain 是离开路由盒网络后持续清理旧入口连接的时长，覆盖配置重建的耗时。
const boxDrain = 30 * time.Second

// Box 返回最近一次同步时交给的路由盒（生成内核配置时用，gen.Input.Box）；不在路由盒网络上时为 nil。
func (b *boxRouter) Box() *gen.ProxyEndpoint { return b.lastBox.Load() }

const boxRouteEvery = 5 * time.Second

func (b *boxRouter) Kick() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

func (b *boxRouter) Run(ctx context.Context) {
	t := time.NewTicker(boxRouteEvery)
	defer t.Stop()
	for {
		b.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.kick:
		}
	}
}

func (b *boxRouter) sync(ctx context.Context) {
	if b.sup == nil || b.direct == nil {
		return
	}
	direct := b.direct()
	var box *gen.ProxyEndpoint
	if direct && b.box != nil {
		box = b.box()
	}
	prevBox := b.lastBox.Swap(box)
	boxGone := prevBox != nil && !sameEndpoint(prevBox, box)
	if !sameEndpoint(prevBox, box) {
		server := ""
		if box != nil {
			server = box.Server
		}
		b.log.Info("路由盒变化，重建内核配置", "box", server)
		b.sup.Trigger()
	}
	if boxGone && box == nil {
		b.drainUntil = time.Now().Add(boxDrain)
	}
	lostDirect := b.wasDirect && !direct
	b.wasDirect = direct
	defer func() { b.revoke(ctx, lostDirect, boxGone) }()
	routes := b.sup.BoxRoutes()
	if len(routes) == 0 {
		return
	}
	if direct != b.last {
		b.log.Info("路由盒切换", "direct", direct)
		b.last = direct
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for name, target := range routes {
		want := target
		if direct && box == nil {
			want = "DIRECT"
		}
		if code, out, err := b.sup.CallCore(ctx, http.MethodGet, core.ProxyPath(name), nil); err == nil && code == http.StatusOK {
			var cur struct {
				Now string `json:"now"`
			}
			if json.Unmarshal(out, &cur) == nil && cur.Now == want {
				continue
			}
		}
		body, _ := json.Marshal(map[string]string{"name": want})
		if code, out, err := b.sup.CallCore(ctx, http.MethodPut, core.ProxyPath(name), body); err != nil || code >= 300 {
			b.log.Warn("切换路由盒选择组失败", "group", name, "want", want, "code", code, "err", err, "body", string(out))
		}
	}
}

// revoke 在选择组切换之后断开不该再存在的旧连接：内核切换选择组、重载配置都不会断开已经建立的连接，
// 浏览器的 HTTP/2、WebSocket 长连接会一直沿用当时的路（直连，或者旧路由盒的代理入口）。
//   - lostDirect：不再直连（离开受信网络、出口不符、网络变化正在确认）——断开经「路由盒切换」选择组直连的连接
//     和经出口闸门直连的连接；
//   - boxGone：离开或换了路由盒——断开经路由盒代理入口的连接。离开后 boxDrain 内每次同步都再清一遍，
//     配置重建之前新建的连接仍会指向旧入口。
func (b *boxRouter) revoke(ctx context.Context, lostDirect, boxGone bool) {
	draining := !b.drainUntil.IsZero() && time.Now().Before(b.drainUntil) && b.lastBox.Load() == nil
	if !lostDirect && !boxGone && !draining {
		return
	}
	if lostDirect && b.revokeGate != nil {
		b.revokeGate("")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	n := b.sup.CloseConns(ctx, func(chains []string, inbound string) bool {
		if (boxGone || draining) && slices.Contains(chains, gen.BoxProxyName) {
			return true
		}
		if !lostDirect {
			return false
		}
		if inbound == gen.GateDirectListener {
			return true
		}
		return len(chains) > 0 && chains[0] == "DIRECT" &&
			slices.ContainsFunc(chains, func(c string) bool { return strings.HasPrefix(c, gen.BoxRoutePrefix) })
	})
	if n > 0 {
		b.log.Warn("线路判断变化，已断开旧连接", "count", n, "lost_direct", lostDirect, "box_gone", boxGone || draining)
	}
}
