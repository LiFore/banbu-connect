package desktop

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/trust"
)

// 线路预览（指定域名的出口与延迟）：经内核的本机代理入口真实请求一次这个域名，让内核按规则分流；
// 连接还开着时从内核的 /connections 读出命中的规则和出站链，再经规则选中的那个代理组的线路探测入口
// （gen.Result.Routes）请求 Cloudflare trace，得到这条线路的出口 IP 和国家。
// 离线模拟规则（管理服务的规则预览）判断不了远程规则集、GEOIP 和当前选中的节点，这里看的是实际结果。
// Google、GitHub 没有 Cloudflare trace 这类回显出口的地址，出口检测里它们也用这个办法。

// RouteTest 是一个域名的测试结果。
type RouteTest struct {
	Target      string        `json:"target"` // 用户输入
	URL         string        `json:"url"`
	Host        string        `json:"host"`
	OK          bool          `json:"ok"`
	Error       string        `json:"error,omitempty"`
	Status      int           `json:"status,omitempty"`     // 站点的 HTTP 状态码
	ConnectMS   int64         `json:"connect_ms,omitempty"` // 新建连接：经代理建立隧道 + TLS 握手
	LatencyMS   int64         `json:"latency_ms,omitempty"` // 已建立的连接上一次请求的往返（取中位数）
	Samples     []int64       `json:"samples,omitempty"`
	Rule        string        `json:"rule,omitempty"`
	RulePayload string        `json:"rule_payload,omitempty"`
	Path        []string      `json:"path,omitempty"`  // 规则选中的代理组 → … → 实际出站节点
	Group       string        `json:"group,omitempty"` // 规则选中的代理组（或 DIRECT、节点）
	Egress      *trust.Egress `json:"egress,omitempty"`
	CheckedAt   time.Time     `json:"checked_at"`
}

// ServiceTargets 是出口检测里固定检测的服务：没有 Cloudflare trace，只能按规则实测。
var ServiceTargets = []struct{ Name, Target string }{
	{"Google", "https://www.google.com/generate_204"},
	{"GitHub", "https://github.com/"},
}

const routeSamples = 3

// parseRouteTarget 接受域名、域名:端口或 http(s) 地址，返回请求地址和主机名。
func parseRouteTarget(in string) (*url.URL, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil, errors.New("请输入域名或地址")
	}
	if !strings.Contains(in, "://") {
		in = "https://" + in
	}
	u, err := url.Parse(in)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("不是有效的域名或地址")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("只支持 http、https 地址")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// TestRoute 测试一个域名。内核没在运行时返回错误结果（不直连出去）。
func TestRoute(ctx context.Context, s *core.Supervisor, target string) RouteTest {
	res := RouteTest{Target: target, CheckedAt: time.Now().UTC()}
	u, err := parseRouteTarget(target)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.URL, res.Host = u.String(), u.Hostname()
	mixed := s.MixedProxy(ctx)
	if mixed == "" {
		res.Error = "代理没有在运行：启动代理后才能按规则测试"
		return res
	}
	proxy, _ := url.Parse(mixed)
	tr := &http.Transport{
		Proxy:               http.ProxyURL(proxy),
		TLSHandshakeTimeout: 8 * time.Second,
		MaxIdleConnsPerHost: 1,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Timeout:       12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// 第一次：新建连接，记建连耗时；之后在同一连接上再请求几次，取往返的中位数。
	for i := 0; i < routeSamples; i++ {
		ms, connect, status, err := timedRequest(ctx, client, u)
		if err != nil {
			if i == 0 {
				res.Error = err.Error()
			}
			break
		}
		if i == 0 {
			res.ConnectMS, res.Status, res.OK = connect, status, true
		}
		res.Samples = append(res.Samples, ms)
	}
	if len(res.Samples) > 0 {
		sorted := append([]int64(nil), res.Samples...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		res.LatencyMS = sorted[len(sorted)/2]
	}

	// 连接还在空闲池里：读内核记录的规则和出站链。站点连不上时内核也可能记下了规则，照样读。
	if c, ok := findConn(ctx, s, res.Host); ok {
		res.Rule, res.RulePayload, res.Path = c.Rule, c.RulePayload, c.Path()
		if len(c.Chains) > 0 {
			res.Group = c.Chains[len(c.Chains)-1]
		}
	}
	tr.CloseIdleConnections()

	if res.Group != "" {
		res.Egress = routeEgress(ctx, s, res.Group)
	} else if res.OK {
		res.Error = "内核里找不到这次请求的连接记录，无法判断走了哪条规则"
	}
	return res
}

// timedRequest 发一次 HEAD（站点不支持时换 GET），返回整次耗时、新建连接的耗时和状态码。
func timedRequest(ctx context.Context, client *http.Client, u *url.URL) (total, connect int64, status int, err error) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		var start, connStart time.Time
		var connMS int64
		trace := &httptrace.ClientTrace{
			ConnectStart: func(string, string) { connStart = time.Now() },
			TLSHandshakeDone: func(tls.ConnectionState, error) {
				if !connStart.IsZero() {
					connMS = time.Since(connStart).Milliseconds()
				}
			},
		}
		req, rerr := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, u.String(), nil)
		if rerr != nil {
			return 0, 0, 0, rerr
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (boxctl route test)")
		start = time.Now()
		resp, derr := client.Do(req)
		if derr != nil {
			var ue *url.Error
			if errors.As(derr, &ue) {
				derr = ue.Err
			}
			var ne net.Error
			if errors.As(derr, &ne) && ne.Timeout() {
				return 0, 0, 0, fmt.Errorf("%s：连接超时", u.Hostname())
			}
			return 0, 0, 0, fmt.Errorf("%s：%v", u.Hostname(), derr)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		total = time.Since(start).Milliseconds()
		if method == http.MethodHead && resp.StatusCode == http.StatusMethodNotAllowed {
			continue
		}
		if connMS == 0 && !connStart.IsZero() {
			connMS = total
		}
		return total, connMS, resp.StatusCode, nil
	}
	return 0, 0, 0, errors.New("站点拒绝了请求")
}

// findConn 在内核的连接里找这次请求：主机名一致、来自本机的最新一条。
func findConn(ctx context.Context, s *core.Supervisor, host string) (core.Conn, bool) {
	raw, ok := s.FetchConnections(ctx)
	if !ok {
		return core.Conn{}, false
	}
	var best core.Conn
	found := false
	for _, c := range raw.Conns() {
		if !strings.EqualFold(c.Host, host) {
			continue
		}
		if !found || c.Start.After(best.Start) {
			best, found = c, true
		}
	}
	return best, found
}

// routeEgress 经代理组的线路探测入口请求 Cloudflare trace。规则直接指向节点等没有入口的目标时返回说明。
func routeEgress(ctx context.Context, s *core.Supervisor, group string) *trust.Egress {
	p := s.RouteProxy(group)
	if p == nil {
		return &trust.Egress{Error: "规则直接指向「" + group + "」，没有可用的探测入口", CheckedAt: time.Now().UTC()}
	}
	eg := trust.HTTPProber{Proxy: p}.Probe(ctx, []string{"https://www.cloudflare.com/cdn-cgi/trace", "https://1.1.1.1/cdn-cgi/trace"})
	return &eg
}

// TestRoutes 并行测试多个目标，结果顺序和输入一致。
func TestRoutes(ctx context.Context, s *core.Supervisor, targets []string) []RouteTest {
	out := make([]RouteTest, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = TestRoute(ctx, s, t)
		}()
	}
	wg.Wait()
	return out
}
