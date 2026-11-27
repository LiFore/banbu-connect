package gen

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// 规则离线模拟（M5-06）：给一个域名或 IP，按规则顺序找出命中的规则和出口组。
// 不做 DNS 解析、没有 geo 数据和来源信息，所以有些规则判断不了（三值逻辑：命中、不命中、不确定），
// 不确定的规则会列出来，提醒实际可能先命中它们。

// Query 是一次模拟的目标。
type Query struct {
	Host    string     // 域名；查询是 IP 时为空
	IP      netip.Addr // 查询是 IP 时有效
	Port    int        // 0 表示不限
	Network string     // tcp / udp，空为 tcp
}

// ParseQuery 把用户输入解析成查询：IP 或域名。
func ParseQuery(s string, port int, network string) (Query, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if network == "" {
		network = "tcp"
	}
	q := Query{Port: port, Network: network}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		q.IP = a
		return q, nil
	}
	if s == "" || strings.ContainsAny(s, " ,") {
		return q, fmt.Errorf("请输入域名或 IP")
	}
	q.Host = s
	return q, nil
}

// SimRule 是参与模拟的一条规则。
type SimRule struct {
	Rule   string
	Source string // 来自哪里：下发规则、受保护服务、远程规则集、兜底
}

// RuleSetContent 是 RULE-SET 引用的规则集内容。
type RuleSetContent struct {
	Behavior string
	Lines    []string
	Err      string // 下载或解析失败：引用它的规则按不确定处理
}

// SimMatch 是命中的规则。
type SimMatch struct {
	Index  int    `json:"index"` // 从 1 开始
	Rule   string `json:"rule"`
	Source string `json:"source"`
	Target string `json:"target"`
	Final  string `json:"final,omitempty"` // 目标是手动选择组时的默认成员
}

// SimUncertain 是排在命中规则之前、无法离线判断的规则。
type SimUncertain struct {
	Index  int    `json:"index"`
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

// SimResult 是模拟结果。
type SimResult struct {
	Match     *SimMatch      `json:"match"`
	Uncertain []SimUncertain `json:"uncertain"`
}

type tri int

const (
	no tri = iota
	yes
	unknown
)

type simCtx struct {
	q      Query
	sets   map[string]RuleSetContent
	reason string // 最近一次 unknown 的原因
}

// Simulate 依次匹配规则，返回第一条确定命中的规则，以及它之前不确定的规则。
func Simulate(q Query, rules []SimRule, sets map[string]RuleSetContent) SimResult {
	res := SimResult{Uncertain: []SimUncertain{}}
	c := &simCtx{q: q, sets: sets}
	for i, r := range rules {
		cond, target, noResolve, ok := splitRule(r.Rule)
		if !ok {
			continue
		}
		c.reason = ""
		switch c.match(cond, noResolve) {
		case yes:
			res.Match = &SimMatch{Index: i + 1, Rule: r.Rule, Source: r.Source, Target: target}
			return res
		case unknown:
			if len(res.Uncertain) < 20 {
				res.Uncertain = append(res.Uncertain, SimUncertain{Index: i + 1, Rule: r.Rule, Reason: c.reason})
			}
		}
	}
	return res
}

// splitRule 拆出条件、目标和 no-resolve。条件形如 DOMAIN,a.com 或 AND,((…),(…)) 或 MATCH。
func splitRule(r string) (cond, target string, noResolve bool, ok bool) {
	r = strings.TrimSpace(r)
	if r == "" || strings.HasPrefix(r, "#") {
		return "", "", false, false
	}
	if i := strings.LastIndex(r, "),"); i >= 0 && isLogic(r) {
		rest := strings.Split(r[i+2:], ",")
		return r[:i+1], strings.TrimSpace(rest[0]), false, true
	}
	parts := strings.Split(r, ",")
	for len(parts) > 2 && (parts[len(parts)-1] == "no-resolve" || parts[len(parts)-1] == "src") {
		noResolve = noResolve || parts[len(parts)-1] == "no-resolve"
		parts = parts[:len(parts)-1]
	}
	if strings.EqualFold(parts[0], "MATCH") && len(parts) == 2 {
		return "MATCH", strings.TrimSpace(parts[1]), false, true
	}
	if len(parts) < 3 {
		return "", "", false, false
	}
	return strings.Join(parts[:len(parts)-1], ","), strings.TrimSpace(parts[len(parts)-1]), noResolve, true
}

func isLogic(r string) bool {
	t, _, _ := strings.Cut(r, ",")
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "AND", "OR", "NOT":
		return true
	}
	return false
}

func (c *simCtx) unknown(reason string) tri {
	if c.reason == "" {
		c.reason = reason
	}
	return unknown
}

// match 判断一个条件：TYPE,值 或逻辑条件。
func (c *simCtx) match(cond string, noResolve bool) tri {
	if cond == "MATCH" {
		return yes
	}
	typ, val, _ := strings.Cut(cond, ",")
	typ = strings.ToUpper(strings.TrimSpace(typ))
	val = strings.TrimSpace(val)
	switch typ {
	case "AND", "OR", "NOT":
		return c.logic(typ, val)
	}
	return c.simple(typ, val, noResolve)
}

func (c *simCtx) logic(op, body string) tri {
	inner := strings.TrimSpace(body)
	if !strings.HasPrefix(inner, "(") || !strings.HasSuffix(inner, ")") {
		return c.unknown("逻辑规则格式无法解析")
	}
	items := splitParen(inner[1 : len(inner)-1])
	if items == nil {
		return c.unknown("逻辑规则格式无法解析")
	}
	var vals []tri
	for _, it := range items {
		it = strings.TrimSpace(it)
		if !strings.HasPrefix(it, "(") || !strings.HasSuffix(it, ")") {
			return c.unknown("逻辑规则格式无法解析")
		}
		vals = append(vals, c.match(it[1:len(it)-1], false))
	}
	switch op {
	case "NOT":
		if len(vals) != 1 {
			return c.unknown("NOT 只能有一个条件")
		}
		switch vals[0] {
		case yes:
			return no
		case no:
			return yes
		}
		return unknown
	case "AND":
		out := yes
		for _, v := range vals {
			if v == no {
				return no
			}
			if v == unknown {
				out = unknown
			}
		}
		return out
	default: // OR
		out := no
		for _, v := range vals {
			if v == yes {
				return yes
			}
			if v == unknown {
				out = unknown
			}
		}
		return out
	}
}

// splitParen 按最外层逗号切分。
func splitParen(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil
	}
	return append(out, s[start:])
}

func (c *simCtx) simple(typ, val string, noResolve bool) tri {
	h := c.q.Host
	switch typ {
	case "DOMAIN", "DOMAIN-SUFFIX", "DOMAIN-KEYWORD", "DOMAIN-REGEX":
		if h == "" {
			return no // 直接访问 IP 时没有域名（嗅探另说，不在模拟范围）
		}
		v := strings.ToLower(val)
		switch typ {
		case "DOMAIN":
			return boolTri(h == v)
		case "DOMAIN-SUFFIX":
			return boolTri(h == v || strings.HasSuffix(h, "."+v))
		case "DOMAIN-KEYWORD":
			return boolTri(strings.Contains(h, v))
		default:
			re, err := regexp.Compile(val)
			if err != nil {
				return c.unknown("正则无效")
			}
			return boolTri(re.MatchString(h))
		}
	case "GEOSITE":
		if h == "" {
			return no
		}
		return c.unknown("GEOSITE 需要 geo 数据，离线无法判断")
	case "IP-CIDR", "IP-CIDR6", "IP-SUFFIX":
		if !c.q.IP.IsValid() {
			if noResolve {
				return no
			}
			return c.unknown("要先解析域名才能判断 IP 规则")
		}
		p, err := netip.ParsePrefix(val)
		if err != nil {
			if a, aerr := netip.ParseAddr(val); aerr == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			} else {
				return c.unknown("网段无效")
			}
		}
		if typ == "IP-SUFFIX" {
			return boolTri(ipSuffix(c.q.IP, p))
		}
		return boolTri(p.Contains(c.q.IP))
	case "GEOIP", "IP-ASN":
		if !c.q.IP.IsValid() {
			if noResolve {
				return no
			}
			return c.unknown("要先解析域名才能判断 " + typ)
		}
		if typ == "GEOIP" && strings.EqualFold(val, "LAN") {
			return boolTri(c.q.IP.IsPrivate() || c.q.IP.IsLoopback() || c.q.IP.IsLinkLocalUnicast())
		}
		return c.unknown(typ + " 需要 geo 数据，离线无法判断")
	case "DST-PORT":
		if c.q.Port == 0 {
			return c.unknown("没有指定端口")
		}
		return boolTri(portIn(c.q.Port, val))
	case "NETWORK":
		return boolTri(strings.EqualFold(val, c.q.Network))
	case "SRC-IP-CIDR", "SRC-PORT", "IN-PORT", "IN-TYPE", "IN-USER", "IN-NAME", "PROCESS-NAME", "PROCESS-PATH", "PROCESS-NAME-REGEX", "PROCESS-PATH-REGEX", "UID", "DSCP":
		return c.unknown(typ + " 取决于发起连接的设备或程序")
	case "RULE-SET":
		return c.ruleSet(val, noResolve)
	}
	return c.unknown("不支持离线判断 " + typ)
}

func (c *simCtx) ruleSet(name string, noResolve bool) tri {
	set, ok := c.sets[name]
	if !ok {
		return c.unknown("规则集「" + name + "」没有定义")
	}
	if set.Err != "" {
		return c.unknown("规则集「" + name + "」无法读取：" + set.Err)
	}
	out := no
	for _, l := range set.Lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var v tri
		switch set.Behavior {
		case "domain":
			d := strings.ToLower(l)
			switch {
			case c.q.Host == "":
				v = no
			case strings.HasPrefix(d, "+."):
				v = boolTri(c.q.Host == d[2:] || strings.HasSuffix(c.q.Host, d[1:]))
			case strings.HasPrefix(d, "."):
				v = boolTri(strings.HasSuffix(c.q.Host, d))
			case strings.Contains(d, "*"):
				v = boolTri(wildcard(d, c.q.Host))
			default:
				v = boolTri(c.q.Host == d)
			}
		case "ipcidr":
			v = c.simple("IP-CIDR", l, noResolve)
		default:
			v = c.match(l, noResolve)
		}
		if v == yes {
			return yes
		}
		if v == unknown {
			out = unknown
		}
	}
	return out
}

func wildcard(pattern, host string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `[^.]+`) + "$"
	ok, _ := regexp.MatchString(re, host)
	return ok
}

func ipSuffix(ip netip.Addr, p netip.Prefix) bool {
	if ip.BitLen() != p.Addr().BitLen() {
		return false
	}
	a, b := ip.AsSlice(), p.Addr().AsSlice()
	bits := p.Bits()
	// 比较最低的 bits 位
	for i := 0; i < bits; i++ {
		pos := len(a)*8 - 1 - i
		mask := byte(1) << (7 - uint(pos%8))
		if a[pos/8]&mask != b[pos/8]&mask {
			return false
		}
	}
	return true
}

func portIn(port int, spec string) bool {
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool { return r == '/' || r == ',' }) {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

func boolTri(b bool) tri {
	if b {
		return yes
	}
	return no
}
