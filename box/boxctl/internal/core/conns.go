package core

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// 内核连接快照：桌面客户端和路由盒共用（路由盒的连接页、设备流量、规则命中，以及两端的代理连接统计）。

// Conn 是一条连接，字段取自内核 /connections。
type Conn struct {
	ID          string    `json:"id"`
	Network     string    `json:"network"`
	Source      string    `json:"source"` // 来源 IP
	Host        string    `json:"host"`
	Destination string    `json:"destination"` // IP:端口
	Rule        string    `json:"rule"`
	RulePayload string    `json:"rule_payload"`
	Chains      []string  `json:"chains"` // 出站链：第一个是实际出站，最后一个是规则选中的代理组
	Process     string    `json:"process,omitempty"`
	Inbound     string    `json:"inbound,omitempty"` // 入站监听名：路由盒上经客户端代理入口进来的是 gen.ClientListener
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
	Start       time.Time `json:"start"`
}

// RawConnections 是内核 /connections 的原始响应。
type RawConnections struct {
	DownloadTotal int64 `json:"downloadTotal"`
	UploadTotal   int64 `json:"uploadTotal"`
	Connections   []struct {
		ID       string `json:"id"`
		Metadata struct {
			Network         string `json:"network"`
			SourceIP        string `json:"sourceIP"`
			DestinationIP   string `json:"destinationIP"`
			DestinationPort string `json:"destinationPort"`
			Host            string `json:"host"`
			SniffHost       string `json:"sniffHost"`
			// Process 只在桌面端有值（内核能查到发起连接的进程时）。TODO(verify:V-conn-process)
			Process     string `json:"process"`
			InboundName string `json:"inboundName"`
		} `json:"metadata"`
		Upload      int64     `json:"upload"`
		Download    int64     `json:"download"`
		Start       time.Time `json:"start"`
		Chains      []string  `json:"chains"`
		Rule        string    `json:"rule"`
		RulePayload string    `json:"rulePayload"`
	} `json:"connections"`
}

// Conns 把原始响应转成连接列表。
func (r RawConnections) Conns() []Conn {
	out := make([]Conn, 0, len(r.Connections))
	for _, c := range r.Connections {
		host := c.Metadata.Host
		if host == "" {
			host = c.Metadata.SniffHost
		}
		conn := Conn{ID: c.ID, Network: c.Metadata.Network, Source: c.Metadata.SourceIP, Host: host,
			Destination: JoinHostPort(c.Metadata.DestinationIP, c.Metadata.DestinationPort),
			Rule:        c.Rule, RulePayload: c.RulePayload, Chains: c.Chains, Process: c.Metadata.Process, Inbound: c.Metadata.InboundName,
			Upload: c.Upload, Download: c.Download, Start: c.Start}
		if conn.Chains == nil {
			conn.Chains = []string{}
		}
		out = append(out, conn)
	}
	return out
}

// ConnSnapshot 是某一时刻的连接快照。Running 为假表示内核没在运行（或读取失败）。
type ConnSnapshot struct {
	Time          time.Time
	Running       bool
	UploadTotal   int64
	DownloadTotal int64
	Conns         []Conn
}

// Snapshot 把原始响应转成快照。
func (r RawConnections) Snapshot(now time.Time, running bool) ConnSnapshot {
	if !running {
		return ConnSnapshot{Time: now}
	}
	return ConnSnapshot{Time: now, Running: true, UploadTotal: r.UploadTotal, DownloadTotal: r.DownloadTotal, Conns: r.Conns()}
}

// FetchConnections 读一次内核的连接。读不到时 running 为假。
func (s *Supervisor) FetchConnections(ctx context.Context) (RawConnections, bool) {
	var raw RawConnections
	code, body, err := s.CallCore(ctx, http.MethodGet, "/connections", nil)
	if err != nil || code != http.StatusOK || json.Unmarshal(body, &raw) != nil {
		return RawConnections{}, false
	}
	return raw, true
}

// JoinHostPort 拼地址；IPv6 加方括号，端口为空时只有地址。
func JoinHostPort(ip, port string) string {
	if ip == "" {
		return ""
	}
	if strings.Contains(ip, ":") {
		ip = "[" + ip + "]"
	}
	if port == "" {
		return ip
	}
	return ip + ":" + port
}

// directOutbounds 是不经代理的出站：实际出站是它们的连接不统计（ADR-048）。
var directOutbounds = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true}

// IsProxied 表示连接经代理出去：实际出站（chains 的第一个）不是 DIRECT、REJECT 一类。
func (c Conn) IsProxied() bool {
	return len(c.Chains) > 0 && !directOutbounds[strings.ToUpper(c.Chains[0])]
}

// Path 是从规则选中的代理组到实际出站节点的链路（chains 倒过来）。
// 链式代理时中间是各跳。TODO(verify:V-conn-chains) 链式代理时 chains 的顺序。
func (c Conn) Path() []string {
	out := make([]string, len(c.Chains))
	for i, x := range c.Chains {
		out[len(c.Chains)-1-i] = x
	}
	return out
}
