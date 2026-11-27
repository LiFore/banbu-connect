package killswitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Health 是 Mihomo 通路的探测结果，只用于展示和启用前的检查：
// 保护规则是持久的，探测失败不会放行任何流量。
type Health struct {
	DNSOK     bool   `json:"dns_ok"`
	DNSDetail string `json:"dns_detail"`
	// DomesticOK 是内核解析国内域名（直连上游）的结果。受保护域名走代理上游，单看它发现不了
	// 「国内 DoH 被网络拦了、整机无法解析」。只用于提示，不影响保护状态。
	DomesticOK     bool      `json:"domestic_dns_ok"`
	DomesticDetail string    `json:"domestic_dns_detail"`
	TUNOK          bool      `json:"tun_ok"`
	Interface      string    `json:"interface"`
	CheckedAt      time.Time `json:"checked_at"`
}

type Prober interface {
	Probe(ctx context.Context, suffix, listen, target string, physical []string) Health
}

type RealProber struct{}

func (RealProber) Probe(ctx context.Context, suffix, listen, target string, physical []string) Health {
	h := Health{CheckedAt: time.Now().UTC()}
	// 两个探测各自最长 5 秒，并行做，不拉长健康检查的周期。
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.DomesticOK, h.DomesticDetail = probeDNS(ctx, domesticProbe, listen)
		if !h.DomesticOK {
			h.DomesticDetail = "国内域名解析失败（" + domesticProbe + "）：Mihomo 的国内 DNS 上游都不可用"
		}
	}()
	h.DNSOK, h.DNSDetail = probeDNS(ctx, suffix, listen)
	<-done
	h.Interface = outgoingInterface(target)
	h.TUNOK = h.Interface != "" && !slices.Contains(physical, h.Interface)
	return h
}

// domesticProbe 走 geosite:cn 的直连上游。
const domesticProbe = "baidu.com"

func probeDNS(ctx context.Context, suffix, listen string) (bool, string) {
	if suffix == "" {
		return false, "没有启用的域名"
	}
	// 要等上游返回真实结果，给的时间比本地应答长。
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", listen)
	}}
	ips, err := r.LookupIP(ctx, "ip4", suffix)
	if err != nil {
		// 后缀本身可以没有 A 记录，只要 Mihomo 作答就算通路正常。
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return true, suffix + " 没有 A 记录"
		}
		return false, "Mihomo DNS 无响应"
	}
	if len(ips) == 0 {
		return true, suffix + " 没有 A 记录"
	}
	return true, ips[0].String()
}

// tunListening 问内核 TUN 是否真的在运行：TUN 启动失败时内核继续跑，只把 enable 置为 false。
func tunListening(ctx context.Context, cfg Config) bool {
	var general struct {
		TUN struct {
			Enable bool `json:"enable"`
		} `json:"tun"`
	}
	return controllerGet(ctx, cfg, "/configs", &general) == nil && general.TUN.Enable
}

// outgoingInterface 问路由表：UDP “连接”不发送任何数据，只选出口地址。
func outgoingInterface(target string) string {
	c, err := net.Dial("udp4", net.JoinHostPort(target, "443"))
	if err != nil {
		return ""
	}
	defer c.Close()
	local := c.LocalAddr().(*net.UDPAddr).IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
				return i.Name
			}
		}
	}
	return ""
}

// probeTarget 选一个用来判断出口的 IPv4 地址，优先用受保护的网段。
func probeTarget(d Desired) string {
	for _, c := range d.CIDRs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Addr().Is4() {
			return p.Addr().String()
		}
	}
	return "1.1.1.1"
}

// ---- 规则校验：受保护的域名走不走代理，全靠 Mihomo 规则分流 ----

// RuleCheck 是一个后缀的校验结果。Status：ok / missing / direct / unknown。
type RuleCheck struct {
	Service string `json:"service"`
	Suffix  string `json:"suffix"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Snippet string `json:"snippet,omitempty"`
}

type mihomoRule struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Proxy   string `json:"proxy"`
}

type mihomoProxy struct {
	Type string   `json:"type"`
	All  []string `json:"all"`
}

func checkRules(ctx context.Context, cfg Config) []RuleCheck {
	var out []RuleCheck
	if cfg.Controller == "" {
		return nil
	}
	var rules struct {
		Rules []mihomoRule `json:"rules"`
	}
	var proxies struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	errRules := controllerGet(ctx, cfg, "/rules", &rules)
	errProxies := controllerGet(ctx, cfg, "/proxies", &proxies)
	for _, s := range cfg.Services {
		if !s.Enabled {
			continue
		}
		for _, suffix := range s.Suffixes {
			c := RuleCheck{Service: s.Name, Suffix: suffix}
			if errRules != nil || errProxies != nil {
				c.Status, c.Detail = "unknown", "无法连接 Mihomo 控制接口"
			} else {
				c.Status, c.Detail = judgeSuffix(suffix, rules.Rules, proxies.Proxies)
			}
			if c.Status == "missing" || c.Status == "direct" {
				c.Snippet = fmt.Sprintf(`"DOMAIN-SUFFIX,%s,%s",`, suffix, s.Group)
			}
			out = append(out, c)
		}
	}
	return out
}

// judgeSuffix 按规则顺序找第一条确定命中该后缀的规则，再看它的目标能否走到 DIRECT。
// RULE-SET 的内容无法从接口得知，不算命中。
func judgeSuffix(suffix string, rules []mihomoRule, proxies map[string]mihomoProxy) (string, string) {
	for _, r := range rules {
		payload := strings.ToLower(r.Payload)
		hit := false
		switch r.Type {
		case "DomainSuffix":
			hit = suffix == payload || strings.HasSuffix(suffix, "."+payload)
		case "Domain":
			hit = suffix == payload
		case "DomainKeyword":
			hit = strings.Contains(suffix, payload)
		case "Match":
			return "missing", "只会落到兜底规则，内核可能替它直连"
		}
		if !hit {
			continue
		}
		if strings.EqualFold(r.Proxy, "REJECT") {
			return "ok", "被规则拒绝"
		}
		if path := pathToDirect(r.Proxy, proxies); path != "" {
			return "direct", "可以走到直连：" + path
		}
		return "ok", "走 " + r.Proxy
	}
	return "missing", "没有命中的规则"
}

func pathToDirect(start string, proxies map[string]mihomoProxy) string {
	var walk func(name string, trail []string) string
	walk = func(name string, trail []string) string {
		trail = append(slices.Clone(trail), name)
		if strings.EqualFold(name, "DIRECT") || strings.EqualFold(proxies[name].Type, "Direct") {
			return strings.Join(trail, " → ")
		}
		for _, m := range proxies[name].All {
			if slices.Contains(trail, m) {
				continue
			}
			if p := walk(m, trail); p != "" {
				return p
			}
		}
		return ""
	}
	return walk(start, nil)
}

func controllerGet(ctx context.Context, cfg Config, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	u, err := url.Parse(cfg.Controller + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if cfg.ControllerSecret != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.ControllerSecret)
	}
	// 控制接口在本机，不能被系统代理设置带偏。
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
