package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/component/proxydialer"
	C "github.com/metacubex/mihomo/constant"
)

// 进程内连通性测试（M3-07/08）：用与运行时相同的 Mihomo 出站实现直接拨号，
// 不依赖运行中的 Mihomo，因此节点保存前即可测试；前置代理链通过自建的
// probeTunnel 解析。流量从 panel 进程发出，目的地为节点服务器地址
// （M9 的 output 规则按目的地址放行代理节点）。

const (
	DefaultDelayURL = "https://www.gstatic.com/generate_204"
	DefaultExitURL  = "https://ipinfo.io/json"
	probeTimeout    = 10 * time.Second
)

// ChainNode 是链路中的一跳。Name 在链内唯一，Dialer 指向下一跳的 Name（空表示直连）。
type ChainNode struct {
	Name    string
	Mapping map[string]any
	Dialer  string
}

// probeTunnel 只用于让 dialer-proxy 按名字找到链内的其它节点。
type probeTunnel struct{ proxies map[string]C.Proxy }

func (t *probeTunnel) HandleTCPConn(conn net.Conn, _ *C.Metadata)   { conn.Close() }
func (t *probeTunnel) HandleUDPPacket(p C.UDPPacket, _ *C.Metadata) { p.Drop() }
func (t *probeTunnel) NatTable() C.NatTable                         { return nil }
func (t *probeTunnel) Proxies() map[string]C.Proxy                  { return t.proxies }

// buildChain 构造整条链路的出站对象，返回链首与释放函数。chain[0] 为被测节点。
func buildChain(chain []ChainNode) (C.Proxy, func(), error) {
	t := &probeTunnel{proxies: map[string]C.Proxy{}}
	closeAll := func() {
		for _, p := range t.proxies {
			_ = p.Close()
		}
	}
	for _, n := range chain {
		m := make(map[string]any, len(n.Mapping)+2)
		for k, v := range n.Mapping {
			m[k] = v
		}
		m["name"] = n.Name
		delete(m, "dialer-proxy")
		if n.Dialer != "" {
			m["dialer-proxy"] = n.Dialer
		}
		p, err := adapter.ParseProxy(m, adapter.WithTunnelForAPI(t))
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("节点 %s 配置无效：%w", n.Name, err)
		}
		t.proxies[n.Name] = p
	}
	return t.proxies[chain[0].Name], closeAll, nil
}

// Delay 返回经链路访问 url 的延迟（毫秒）。
func Delay(ctx context.Context, chain []ChainNode, url string) (int, error) {
	head, release, err := buildChain(chain)
	if err != nil {
		return 0, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ms, err := head.URLTest(ctx, url, nil)
	if err != nil {
		return 0, simplifyDialError(err)
	}
	return int(ms), nil
}

// ExitInfo 是出口检测结果。
type ExitInfo struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
	ASN     string `json:"asn"`
	Org     string `json:"org"`
}

// Exit 经链路请求 IP 信息服务（ipinfo.io 格式），返回出口 IP、ASN、地区。
func Exit(ctx context.Context, chain []ChainNode, infoURL string) (ExitInfo, error) {
	head, release, err := buildChain(chain)
	if err != nil {
		return ExitInfo{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	transport := &http.Transport{DialContext: proxydialer.New(head, false).DialContext}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return ExitInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return ExitInfo{}, simplifyDialError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExitInfo{}, fmt.Errorf("IP 信息服务返回 HTTP %d", resp.StatusCode)
	}
	var raw struct {
		IP, Country, Region, City, Org string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&raw); err != nil {
		return ExitInfo{}, fmt.Errorf("IP 信息服务返回内容无法解析：%w", err)
	}
	info := ExitInfo{IP: raw.IP, Country: raw.Country, Region: raw.Region, City: raw.City, Org: raw.Org}
	if asn, org, ok := strings.Cut(raw.Org, " "); ok && strings.HasPrefix(asn, "AS") {
		info.ASN, info.Org = asn, org // ipinfo 的 org 形如 "AS15169 Google LLC"
	}
	return info, nil
}

func simplifyDialError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "i/o timeout") {
		return fmt.Errorf("超时（%s）", probeTimeout)
	}
	return err
}
