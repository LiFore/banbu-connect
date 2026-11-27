package box

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"boxctl/internal/buildinfo"
	"boxctl/internal/core"
	"boxctl/internal/ipc"
	"boxctl/internal/trust"
)

// MethodStatus 是面板本机状态接口（panel.sock）的方法名：`boxctl status` 和 LuCI 的「服务 → Banbu Box」页调用。
// 只走本机 unix socket，不经 HTTPS 和登录，所以只给运行概况和监控项，不含节点、订阅地址等配置内容。
const MethodStatus = "status"

// LocalStatus 是 panel.sock 的 status 返回值。
type LocalStatus struct {
	Panel struct {
		Version   string    `json:"version"`
		PID       int       `json:"pid"`
		StartedAt time.Time `json:"started_at"`
	} `json:"panel"`
	Core struct {
		core.CoreStatus
		// Service 是面板每 500 毫秒对内核的检查：托管方式、在线、PID、版本、意外离线次数。
		Service core.ServiceHealth `json:"service"`
		// HoldRules 是内核里的拒绝副本当前是否打开（严格线路阻断在内核里生效）。
		HoldRules bool `json:"hold_rules"`
		// 实时流量（monitor 每秒采样）
		Up            int64 `json:"up"`
		Down          int64 `json:"down"`
		UploadTotal   int64 `json:"upload_total"`
		DownloadTotal int64 `json:"download_total"`
		Connections   int   `json:"connections"`
		Devices       int   `json:"devices"`
	} `json:"core"`
	KillSwitch struct {
		Enabled  bool `json:"enabled"`
		Services int  `json:"services"`
	} `json:"killswitch"`
	// Strict 是严格线路的出口检查，每组一条。
	Strict struct {
		Required bool                `json:"required"`
		Allowed  []string            `json:"allowed"`
		Reason   string              `json:"reason,omitempty"`
		Pending  bool                `json:"pending"`
		Groups   []trust.StrictGroup `json:"groups"`
	} `json:"strict"`
	// Network 是网络监控：默认路由和 WAN 地址变化时严格线路先阻断再重新确认。
	Network struct {
		Fingerprint string    `json:"fingerprint"`
		Changes     int       `json:"changes"`
		ChangedAt   time.Time `json:"changed_at,omitempty"`
	} `json:"network"`
	// Push 是面板向内核和 guard 下发阻断状态的情况。
	Push struct {
		Hold     bool      `json:"hold"`   // 面板算出的严格线路阻断
		Reason   string    `json:"reason"` // 阻断原因
		At       time.Time `json:"at,omitempty"`
		Error    string    `json:"error,omitempty"`
		Retrying bool      `json:"retrying"`
	} `json:"push"`
	// GuardPolicy 是最近一次成功下发给 guard 的策略版本；与 guard 自报的版本不同说明下发没跟上。
	GuardPolicy   string `json:"guard_policy"`
	ConfigVersion int64  `json:"config_version,omitempty"`
	Subscriptions int    `json:"subscriptions"`
	Server        bool   `json:"server_bound"`
}

var panelStarted = time.Now().UTC()

func (b *Box) localStatus(ctx context.Context) LocalStatus {
	sum := b.summary(ctx)
	var out LocalStatus
	out.Panel.Version, out.Panel.PID, out.Panel.StartedAt = buildinfo.Version, os.Getpid(), panelStarted
	out.Core.CoreStatus = sum.Core
	out.Core.Service = b.sup.Health()
	out.Core.HoldRules = b.sup.HoldRules()
	if live, _ := b.monitor.Live(); live.Running {
		out.Core.Up, out.Core.Down = live.Up, live.Down
		out.Core.UploadTotal, out.Core.DownloadTotal = live.UploadTotal, live.DownloadTotal
		out.Core.Connections, out.Core.Devices = live.Total, len(live.Devices)
	}
	out.KillSwitch.Enabled = sum.KillSwitch.Enabled
	for _, s := range sum.KillSwitch.Services {
		if s.Enabled {
			out.KillSwitch.Services++
		}
	}
	out.Strict.Required, out.Strict.Allowed, out.Strict.Groups = sum.Strict.Required, sum.Strict.Allowed, sum.Strict.Groups
	out.Strict.Reason, out.Strict.Pending = sum.Strict.Reason, sum.Strict.Pending

	b.netMu.Lock()
	out.Network.Fingerprint, out.Network.Changes, out.Network.ChangedAt = b.netFP, b.netChanges, b.netAt
	b.netMu.Unlock()

	if p, err := b.Policy(ctx); err == nil {
		out.Push.Hold, out.Push.Reason = p.Hold, p.Reason
	}
	b.pushMu.Lock()
	out.GuardPolicy = b.pushed
	out.Push.At, out.Push.Error, out.Push.Retrying = b.pushAt, b.pushErr, b.retryPending
	b.pushMu.Unlock()
	out.ConfigVersion = sum.ConfigVersion
	out.Subscriptions = len(sum.Subscriptions)
	out.Server = sum.Server != nil && sum.Server.Bound
	return out
}

// serveStatus 在 path 上提供本机状态接口，直到 b.ctx 结束。
func (b *Box) serveStatus(path string) {
	srv := &ipc.Server{
		Path:   path,
		Logger: b.log,
		Handlers: map[string]ipc.Handler{
			MethodStatus: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return b.localStatus(ctx), nil
			},
		},
	}
	if err := srv.Serve(b.ctx); err != nil {
		b.log.Warn("本机状态接口启动失败", "path", path, "err", err)
	}
}
