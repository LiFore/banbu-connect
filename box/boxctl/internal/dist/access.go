package dist

import (
	"context"
	"net/http"
	"time"
	"unicode/utf8"
)

// ---- 客户端接入状态 ----
//
// 客户端此刻怎么上网：经路由盒、受信 Wi-Fi 直连、本机代理、阻断，还是没有使用代理。客户端在状态变化时立即上报，
// 没变化时每分钟上报一次。管理服务记最近一次的快照（device_access），并按接入方式分段记历史
// （device_access_sessions）。与诊断数据分开：不受「诊断上报」开关影响，也不含 Wi-Fi 名称。

const (
	AccessBox     = "box"          // 在路由盒网络上，流量交给路由盒出口
	AccessTrusted = "trusted_wifi" // 在受信 Wi-Fi 上直连
	AccessLocal   = "local"        // 经本机内核（TUN 或系统代理）
	AccessHold    = "hold"         // 受保护服务被阻断
	AccessOff     = "off"          // 没有使用代理：内核没运行，也不在受信网络上

	// accessGap 是客户端多久没上报就算离线（客户端每分钟上报一次）。
	accessGap = 3 * time.Minute
	// accessKeep 是历史分段的保留时间。
	accessKeep = 90 * 24 * time.Hour
	// accessMaxText 是原因等文字的长度上限（字符）。
	accessMaxText = 300
)

func validAccessMode(m string) bool {
	switch m {
	case AccessBox, AccessTrusted, AccessLocal, AccessHold, AccessOff:
		return true
	}
	return false
}

// AccessState 是客户端上报的接入状态。
type AccessState struct {
	Mode        string  `json:"mode"`
	Reason      string  `json:"reason,omitempty"` // Kill Switch 的判断原因，Wi-Fi 名称已去掉
	CoreRunning bool    `json:"core_running"`
	TUN         bool    `json:"tun"`                  // TUN 实际在接管
	ProxyMode   string  `json:"proxy_mode,omitempty"` // rule / global / direct
	KillSwitch  string  `json:"killswitch,omitempty"` // Kill Switch 的状态
	Gate        bool    `json:"gate"`                 // 出口闸门直连在生效
	BoxID       string  `json:"box_id,omitempty"`     // 发现并验证了的路由盒
	BoxAddr     string  `json:"box_addr,omitempty"`
	Network     DiagNet `json:"network"`
	// Since 是客户端进入这个接入方式的时间（客户端时钟）。
	Since time.Time `json:"since"`
}

func clipN(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (a *AccessState) normalize() error {
	if !validAccessMode(a.Mode) {
		return &InputError{"接入方式无效"}
	}
	a.Reason = clipN(a.Reason, accessMaxText)
	a.ProxyMode = clipN(a.ProxyMode, 16)
	a.KillSwitch = clipN(a.KillSwitch, 32)
	a.BoxID = clipN(a.BoxID, 64)
	a.BoxAddr = clipN(a.BoxAddr, 64)
	a.Network.Interface = clipN(a.Network.Interface, 64)
	a.Network.Hint = clipN(a.Network.Hint, accessMaxText)
	return nil
}

// DeviceAccess 是一台客户端最近一次上报的接入状态。
type DeviceAccess struct {
	DeviceID   int64       `json:"device_id"`
	State      AccessState `json:"state"`
	Since      string      `json:"since"` // 管理服务记的这一段的开始时间
	ReportedAt string      `json:"reported_at"`
	Online     bool        `json:"online"` // 最近 accessGap 内上报过
}

// AccessSession 是一段接入方式。
type AccessSession struct {
	Mode    string `json:"mode"`
	BoxID   string `json:"box_id,omitempty"`
	StartAt string `json:"start_at"`
	EndAt   string `json:"end_at"` // 空表示还在进行
	LastAt  string `json:"last_seen"`
}

// ReportAccess 上报此刻的接入状态。
func (c *Client) ReportAccess(ctx context.Context, st AccessState) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/client/access", st, nil, requestTimeout)
}
