package desktop

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/dist"
	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

// ---- 接入状态上报 ----
//
// 每 5 秒算一次此刻怎么上网（dist.Access*），变化时立即上报，没变化时每分钟上报一次。
// 只要绑定了管理服务就上报，与诊断数据分开（不受「诊断上报」开关影响）；不含 Wi-Fi 名称。

const (
	accessTick      = 5 * time.Second
	accessHeartbeat = time.Minute
)

// accessMode 按 Kill Switch 的判断、受信网络和内核状态算出接入方式：
//   - 阻断：受保护服务被拦；
//   - 在受信网络上直连（Kill Switch 判断直连，或闸门直连生效）：发现了路由盒算「经路由盒」，否则算「受信 Wi-Fi」；
//   - 内核在运行：本机代理（TUN 或系统代理）；
//   - 都不是：没有使用代理。
func accessMode(core core.CoreStatus, ks killswitch.Status, n trust.Status) string {
	direct := ks.Verdict.Path == killswitch.PathDirect || (n.Trusted && n.Gate.Active)
	switch {
	case ks.Enabled && ks.Verdict.Path == killswitch.PathHold:
		return dist.AccessHold
	case n.Trusted && direct && n.Box.ID != "":
		return dist.AccessBox
	case n.Trusted && direct:
		return dist.AccessTrusted
	case core.Running:
		return dist.AccessLocal
	default:
		return dist.AccessOff
	}
}

func accessState(c core.CoreStatus, ks killswitch.Status, n trust.Status) dist.AccessState {
	st := dist.AccessState{
		Mode:        accessMode(c, ks, n),
		Reason:      redact(ks.Verdict.Reason),
		CoreRunning: c.Running,
		TUN:         c.TUNActive,
		ProxyMode:   c.Mode,
		KillSwitch:  ks.State,
		Gate:        n.Trusted && n.Gate.Active,
		Network:     diagNet(n),
	}
	if n.Box.ID != "" {
		st.BoxID, st.BoxAddr = n.Box.ID, n.Box.Addr
	}
	st.Network.Hint = redact(st.Network.Hint)
	return st
}

// sameAccess 判断两次状态是否相同（不比较 Since）。
func sameAccess(a, b dist.AccessState) bool {
	a.Since, b.Since = time.Time{}, time.Time{}
	return a == b
}

// runAccess 持续上报接入状态，直到 ctx 结束。
func runAccess(ctx context.Context, c *dist.Client, sup *core.Supervisor, ks *killswitch.Engine, n *trust.Evaluator, log *slog.Logger) {
	var prev, sent dist.AccessState // prev：上一轮算出的；sent：最近一次上报成功的
	var lastAt, since time.Time
	reported := false
	backoff := time.Time{} // 管理服务是旧版本（没有接口）时，过一阵再试
	t := time.NewTicker(accessTick)
	defer t.Stop()
	for {
		now := time.Now()
		cur := accessState(sup.Status(), ks.Status(), n.Status())
		if since.IsZero() || cur.Mode != prev.Mode || cur.BoxID != prev.BoxID {
			since = now.UTC()
		}
		cur.Since = since
		switch {
		case !c.Status().Bound:
			reported = false
		case now.After(backoff) && (!reported || !sameAccess(cur, sent) || now.Sub(lastAt) >= accessHeartbeat):
			err := c.ReportAccess(ctx, cur)
			var he *dist.HTTPError
			switch {
			case err == nil:
				sent, lastAt, reported = cur, now, true
			case errors.As(err, &he) && he.Status == http.StatusNotFound:
				backoff = now.Add(30 * time.Minute)
			case ctx.Err() == nil:
				log.Debug("上报接入状态失败", "err", err)
			}
		}
		prev = cur
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
