package dist

import (
	"context"
	"net/http"
)

// ---- 客户端更新方式与最低版本（S1.10，ADR-057）----
//
// 每个更新包可以设置更新方式：
//   - normal：客户端打开首页时提示，用户点「更新」（原来的做法）；
//   - push：客户端后台定时检查，发现后发系统通知；
//   - silent：客户端后台自动下载安装，下次退出、重启或空闲时生效；
//   - force：强制更新：客户端后台自动下载安装；下次打开应用时全屏提示，更新并重启后才能继续使用。
// 另可设置最低版本：客户端版本低于已发布版本里最高的最低版本时同样按 force 处理。
// 只有装了带这些逻辑的版本（0.2.5 之后）才会按这里的方式更新。

const (
	UpdateNormal = "normal"
	UpdatePush   = "push"
	UpdateSilent = "silent"
	UpdateForce  = "force"
)

// ---- 各客户端的更新进度（BXC-F0-058）----

// UpdateProgressReport 是客户端上报的更新进度。
type UpdateProgressReport struct {
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Phase   string `json:"phase"` // requested / checking / downloading / installing / installed / failed / none
	Error   string `json:"error,omitempty"`
}

// ---- 客户端 ----

// ReportUpdateProgress 把更新进度报给管理服务。
func (c *Client) ReportUpdateProgress(ctx context.Context, p UpdateProgressReport) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/update/progress", p, nil, requestTimeout)
}
