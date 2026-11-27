package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Policy 是 panel 下发给 guard 的保护策略。guard 把它存到磁盘，开机时先加载上一份（M9-02）。
//
// 语义与桌面 Kill Switch 一致（ADR-032）：只保护受保护服务，规则常驻、不依赖内核存活。
type Policy struct {
	// Suffixes 是受保护服务的域名后缀：dnsmasq 把它们的解析结果写进严格集合，并只向内核 DNS 查询。
	Suffixes []string `json:"suffixes"`
	// CIDRs 是受保护服务的固定网段，直接进严格集合。
	CIDRs []string `json:"cidrs"`
	// Hold 为 true 时局域网去严格集合的流量在进内核之前就丢弃：严格线路出口不符（ADR-026）。
	Hold bool `json:"hold"`
	// DNSUpstream 是内核的 DNS 监听地址，如 127.0.0.1:1053。
	DNSUpstream string `json:"dns_upstream"`
	// ForwardAll 为 true 时局域网的全部 DNS 都转发给内核（内核在运行）；否则只有受保护域名。
	ForwardAll bool `json:"forward_all"`
	// RFC6761 为 true 时 boxctl 接管了 dnsmasq 的 boguspriv（ADR-081）：OpenWrt 打开 boguspriv 时会加载
	// rfc6761.conf，其中 server=/local/ 让 .local 只在本地应答，到不了内核。init 脚本关掉 boguspriv，
	// 由这里补回 bogus-priv 和 rfc6761.conf 里除 .local 之外的保留后缀。
	RFC6761 bool `json:"rfc6761,omitempty"`
	// Reason 是 Hold 的原因，显示在界面上。
	Reason string `json:"reason,omitempty"`
	// Blocks 是局域网设备的断网与时段管控（ADR-036），与受保护服务无关。
	Blocks []Block `json:"blocks,omitempty"`
	// CoreExpected 是内核应该在运行（用户启用了内核）：guard 每 500 毫秒检查内核，离线或异常时阻断（ADR-045）。
	CoreExpected bool `json:"core_expected,omitempty"`
	// CoreController 是内核控制接口的地址（本机），检查用。
	CoreController string `json:"core_controller,omitempty"`
	// CoreDown 由 guard 自己设置（面板下发的值忽略）：内核异常时局域网除了访问路由盒和内网之外全部丢弃。
	CoreDown bool `json:"core_down,omitempty"`
}

var suffixRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)

// ValidSuffix 报告域名后缀能否写进策略（Normalize 会拒绝整份策略里的任何一个无效后缀）。
func ValidSuffix(s string) bool {
	return suffixRE.MatchString(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "."))
}

// Normalize 校验并规范化：这些值会写进 nft 规则和 dnsmasq 配置，必须严格。
func (p Policy) Normalize() (Policy, error) {
	out := Policy{Hold: p.Hold, ForwardAll: p.ForwardAll, Reason: strings.TrimSpace(p.Reason), CoreExpected: p.CoreExpected}
	if p.CoreController != "" {
		ap, err := netip.ParseAddrPort(p.CoreController)
		if err != nil || !ap.Addr().IsLoopback() {
			return Policy{}, fmt.Errorf("内核控制接口须是本机地址加端口：%q", p.CoreController)
		}
		out.CoreController = ap.String()
	}
	for _, s := range p.Suffixes {
		s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), ".")
		if s == "" {
			continue
		}
		if !suffixRE.MatchString(s) {
			return Policy{}, fmt.Errorf("域名后缀无效：%q", s)
		}
		out.Suffixes = append(out.Suffixes, s)
	}
	for _, c := range p.CIDRs {
		pfx, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			addr, aerr := netip.ParseAddr(strings.TrimSpace(c))
			if aerr != nil {
				return Policy{}, fmt.Errorf("网段无效：%q", c)
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		out.CIDRs = append(out.CIDRs, pfx.Masked().String())
	}
	slices.Sort(out.Suffixes)
	out.Suffixes = slices.Compact(out.Suffixes)
	slices.Sort(out.CIDRs)
	out.CIDRs = slices.Compact(out.CIDRs)
	if p.DNSUpstream != "" {
		ap, err := netip.ParseAddrPort(p.DNSUpstream)
		if err != nil || !ap.Addr().IsLoopback() {
			return Policy{}, fmt.Errorf("DNS 上游须是本机地址加端口：%q", p.DNSUpstream)
		}
		out.DNSUpstream = ap.String()
	}
	if out.Suffixes == nil {
		out.Suffixes = []string{}
	}
	if out.CIDRs == nil {
		out.CIDRs = []string{}
	}
	blocks, err := normalizeBlocks(p.Blocks)
	if err != nil {
		return Policy{}, err
	}
	if len(blocks) > 0 {
		out.Blocks = blocks
	}
	return out, nil
}

// Version 是策略内容的摘要，写进 nft 表的注释，用来判断系统上的规则是不是这一份。
func (p Policy) Version() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

func (p Policy) v4() []string { return filterFamily(p.CIDRs, false) }
func (p Policy) v6() []string { return filterFamily(p.CIDRs, true) }

func filterFamily(cidrs []string, v6 bool) []string {
	var out []string
	for _, c := range cidrs {
		if strings.Contains(c, ":") == v6 {
			out = append(out, c)
		}
	}
	return out
}

func loadPolicy(path string) (Policy, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, false
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return Policy{}, false
	}
	p, err = p.Normalize()
	return p, err == nil
}

func savePolicy(path string, p Policy) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
