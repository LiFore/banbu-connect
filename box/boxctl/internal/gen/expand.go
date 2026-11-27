package gen

import (
	"net/netip"
	"strings"
)

// Expansion 是从一个规则集里展开出的、可以交给 guard 的条目（ADR-039）：
// 域名（含后缀）进 dnsmasq 的 nftset，网段进严格集合。GEOSITE、ASN、关键字、进程等无法展开，只计数。
type Expansion struct {
	Suffixes []string
	CIDRs    []string
	Skipped  int      // 无法展开的条目数
	Samples  []string // 无法展开的前几条，提示用
}

const expandSamples = 5

// ExpandRuleSet 展开规则集内容（ParseRuleSet 的结果）。behavior：domain、ipcidr、classical。
// 完整域名按后缀处理：dnsmasq 的 server=/x/ 本来就覆盖子域名，多保护一些子域名是安全的方向。
func ExpandRuleSet(lines []string, behavior string) Expansion {
	var e Expansion
	skip := func(l string) {
		e.Skipped++
		if len(e.Samples) < expandSamples {
			e.Samples = append(e.Samples, l)
		}
	}
	addDomain := func(d, raw string) {
		d = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "+."), ".")
		if d == "" || strings.ContainsAny(d, "*/ ") || !strings.Contains(d, ".") {
			skip(raw)
			return
		}
		e.Suffixes = append(e.Suffixes, d)
	}
	addCIDR := func(c, raw string) {
		c = strings.TrimSpace(c)
		if p, err := netip.ParsePrefix(c); err == nil {
			e.CIDRs = append(e.CIDRs, p.Masked().String())
			return
		}
		if a, err := netip.ParseAddr(c); err == nil {
			e.CIDRs = append(e.CIDRs, netip.PrefixFrom(a, a.BitLen()).String())
			return
		}
		skip(raw)
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		switch behavior {
		case "domain":
			addDomain(l, l)
		case "ipcidr":
			addCIDR(l, l)
		default: // classical：TYPE,值[,参数]
			typ, rest, ok := strings.Cut(l, ",")
			if !ok {
				skip(l)
				continue
			}
			val, _, _ := strings.Cut(rest, ",")
			switch strings.ToUpper(strings.TrimSpace(typ)) {
			case "DOMAIN", "DOMAIN-SUFFIX":
				addDomain(val, l)
			case "IP-CIDR", "IP-CIDR6":
				addCIDR(val, l)
			default:
				skip(l)
			}
		}
	}
	return e
}
