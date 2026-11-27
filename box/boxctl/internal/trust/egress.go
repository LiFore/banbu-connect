package trust

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Egress 是一次出口探测的结果。
type Egress struct {
	OK        bool      `json:"ok"`
	IP        string    `json:"ip"`
	Country   string    `json:"country"`
	Colo      string    `json:"colo,omitempty"` // Cloudflare 接入点（如 HKG、BOS），看出流量从哪里进的 Cloudflare
	Source    string    `json:"source"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	LatencyMS int64     `json:"latency_ms,omitempty"` // 整次探测（含失败重试其他地址）用了多久，诊断用
}

type EgressProber interface {
	Probe(ctx context.Context, urls []string) Egress
}

// HTTPProber 请求探测地址并读出出口 IP。
//
// 自己解析域名，不走系统解析：阻断期间受保护的域名在系统里解析不了，探测也会跟着失败。
// Go 自带的解析器读 /etc/resolv.conf（Windows 上读网卡的 DNS），不经过 /etc/resolver 和 NRPT。
// 不走系统代理：要看的是这台机器自己的出口。Proxy 不为空时经它探测（严格线路的探测入口）。
type HTTPProber struct {
	Proxy *url.URL
	// Network 为 tcp4 或 tcp6 时只用这一种地址连接（出口检测分别看 IPv4、IPv6），为空时自动。经代理时无效。
	Network string
}

const probeTimeout = 8 * time.Second

func (p HTTPProber) Probe(ctx context.Context, urls []string) Egress {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Resolver: &net.Resolver{PreferGo: true}}
	dial := dialer.DialContext
	if p.Network != "" && p.Proxy == nil {
		dial = func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, p.Network, addr)
		}
	}
	var proxy func(*http.Request) (*url.URL, error)
	if p.Proxy != nil {
		proxy = http.ProxyURL(p.Proxy)
	}
	client := &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			Proxy:               proxy,
			DialContext:         dial,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start := time.Now()
	var errs []string
	for _, u := range urls {
		e, err := probeOne(ctx, client, u)
		if err == nil {
			e.LatencyMS = time.Since(start).Milliseconds()
			return e
		}
		errs = append(errs, err.Error())
	}
	return Egress{Error: strings.Join(errs, "；"), CheckedAt: time.Now().UTC(), LatencyMS: time.Since(start).Milliseconds()}
}

func probeOne(ctx context.Context, client *http.Client, u string) (Egress, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Egress{}, err
	}
	req.Header.Set("User-Agent", "boxctl-gate")
	resp, err := client.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) {
			err = ue.Unwrap()
		}
		return Egress{}, fmt.Errorf("%s：%v", hostOf(u), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Egress{}, fmt.Errorf("%s：HTTP %d", hostOf(u), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return Egress{}, fmt.Errorf("%s：%v", hostOf(u), err)
	}
	ip, country, ok := parseEgress(string(body))
	if !ok {
		return Egress{}, fmt.Errorf("%s：返回内容里没有 IP", hostOf(u))
	}
	return Egress{OK: true, IP: ip, Country: country, Colo: traceField(string(body), "colo"), Source: u, CheckedAt: time.Now().UTC()}, nil
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

// parseEgress 支持 Cloudflare 的 /cdn-cgi/trace（ip=、loc= 行）和只返回一个 IP 的纯文本。
func parseEgress(body string) (ip, country string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch k {
		case "ip":
			ip = v
		case "loc":
			country = strings.ToUpper(v)
		}
	}
	if ip == "" {
		ip = strings.TrimSpace(body)
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", "", false
	}
	return a.Unmap().String(), country, true
}

func traceField(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == key {
			return v
		}
	}
	return ""
}
