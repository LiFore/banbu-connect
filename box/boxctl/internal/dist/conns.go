package dist

import (
	"context"
	"net/http"
	"time"
)

// ---- 代理连接统计：上报格式（S1.8，ADR-048）----
//
// 桌面客户端和路由盒各自统计经代理组的连接（不含 DIRECT），按分钟汇总，连同关闭的连接记录每分钟打成一批，
// 按序号上报；管理服务按（客户端, 序号）去重。采集见 core.ConnStats，存储与查询见 conns_server.go。

// ConnMinute 是一分钟里一种组合的用量。
type ConnMinute struct {
	Minute   time.Time `json:"minute"`
	Path     []string  `json:"path"`              // 规则选中的代理组 → … → 实际出站节点
	Host     string    `json:"host"`              // 域名，没有时为目标 IP；前 N 以外合并为「(其他)」
	Rule     string    `json:"rule"`              // 规则类型
	Payload  string    `json:"payload"`           // 规则内容
	Process  string    `json:"process,omitempty"` // 发起连接的进程（桌面端）
	Source   string    `json:"source,omitempty"`  // 来源 IP（路由盒）
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
	Conns    int64     `json:"conns"` // 这一分钟新出现的连接数
}

// ConnRecord 是一条已关闭的代理连接。流量是最后一次采样看到的累计值。
type ConnRecord struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Network     string    `json:"network"`
	Host        string    `json:"host"`
	Destination string    `json:"destination"`
	Rule        string    `json:"rule"`
	Payload     string    `json:"payload"`
	Path        []string  `json:"path"`
	Process     string    `json:"process,omitempty"`
	Source      string    `json:"source,omitempty"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
}

// ConnUnattributed 是一分钟里内核总流量与已归属流量的差值：两次采样之间开始又结束的连接，
// 以及连接关闭前最后一段流量，归不到具体连接（可能是代理也可能是 DIRECT）。
type ConnUnattributed struct {
	Minute   time.Time `json:"minute"`
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
}

// ConnSource 是路由盒上一个来源 IP 对应的局域网设备（BXC-F0-027）。
type ConnSource struct {
	IP   string `json:"ip"`
	MAC  string `json:"mac,omitempty"`
	Name string `json:"name,omitempty"` // 备注名，没有时为主机名
}

// LANAddr 是桌面客户端自己的局域网地址：管理服务据此把路由盒统计到的、来自这台电脑的流量
// （闸门直连时经路由盒代理）归到这台客户端（BXC-F0-027）。
type LANAddr struct {
	IP  string `json:"ip"`
	MAC string `json:"mac"`
}

// ConnBatch 是一批上报（通常是一分钟）。
type ConnBatch struct {
	Seq          int64              `json:"seq"`
	Minutes      []ConnMinute       `json:"minutes"`
	Unattributed []ConnUnattributed `json:"unattributed,omitempty"`
	Records      []ConnRecord       `json:"records"`
	Dropped      int64              `json:"dropped,omitempty"` // 超过上限没有记录的已关闭连接数
	Sources      []ConnSource       `json:"sources,omitempty"`
	LAN          []LANAddr          `json:"lan,omitempty"`
}

// ConnReport 是一次上报。Kind 是 desktop 或 box。
type ConnReport struct {
	Kind    string      `json:"kind"`
	Batches []ConnBatch `json:"batches"`
}

// ConnReportResult 是上报的结果。Live 为真表示管理页面正在看这台客户端的实时连接（BXC-F0-030）。
type ConnReportResult struct {
	Acked int64 `json:"acked"` // 已保存的最大序号
	Live  bool  `json:"live"`
}

// ReportConns 上报代理连接统计。
func (c *Client) ReportConns(ctx context.Context, r ConnReport) (ConnReportResult, error) {
	var out ConnReportResult
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/client/conns", r, &out, time.Minute)
	return out, err
}
