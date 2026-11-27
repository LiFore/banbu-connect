package dist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"boxctl/internal/gen"
)

// 远程规则集：管理员在「线路与规则」页配置，随配置下发；客户端据此生成 rule-providers 和规则。
// 没配置过时不下发，客户端用内置的 gen.DefaultRuleSets。

const (
	maxRuleSetBody    = 16 << 20
	ruleSetFetchLimit = 30 * time.Second
	// ruleSetCacheTTL 内直接用缓存；过期后重新下载，下载失败时仍用旧缓存并提示。
	ruleSetCacheTTL = 24 * time.Hour
	maxPreviewPage  = 2000
	sampleDomains   = 12
)

// cachedRuleSet 是缓存里的一份规则集（解析后的规则）。
type cachedRuleSet struct {
	URL       string
	Rules     []string
	Bytes     int
	FetchedAt string
	Cached    bool   // 来自缓存，没有重新下载
	Warning   string // 重新下载失败，用的是旧缓存
}

func (s *Server) readRuleSetCache(ctx context.Context, u, format string) (cachedRuleSet, bool) {
	var c cachedRuleSet
	var rules string
	err := s.DB.QueryRowContext(ctx, `SELECT rules, bytes, fetched_at FROM rule_set_cache WHERE url = ? AND format = ?`, u, format).
		Scan(&rules, &c.Bytes, &c.FetchedAt)
	if err != nil {
		return c, false
	}
	c.URL, c.Cached = u, true
	if rules != "" {
		c.Rules = strings.Split(rules, "\n")
	}
	return c, true
}

// loadRuleSet 取规则集内容：缓存未过期时直接用，否则（或 refresh 时）下载并写入缓存。
func (s *Server) loadRuleSet(ctx context.Context, rawURL, format string, refresh bool) (cachedRuleSet, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return cachedRuleSet{}, &InputError{"地址须是 http(s) 链接"}
	}
	key := u.String()
	old, ok := s.readRuleSetCache(ctx, key, format)
	if ok && !refresh {
		if t, err := time.Parse(time.RFC3339, old.FetchedAt); err == nil && time.Since(t) < ruleSetCacheTTL {
			return old, nil
		}
	}
	_, rules, size, err := s.fetchRuleSet(ctx, key, format)
	if err != nil {
		var ie *InputError
		if ok && !errors.As(err, &ie) {
			old.Warning = "重新下载失败，显示的是上次缓存：" + err.Error()
			return old, nil
		}
		return cachedRuleSet{}, err
	}
	c := cachedRuleSet{URL: key, Rules: rules, Bytes: size, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO rule_set_cache (url, format, rules, count, bytes, fetched_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(url, format) DO UPDATE SET rules = excluded.rules, count = excluded.count, bytes = excluded.bytes, fetched_at = excluded.fetched_at`,
		key, format, strings.Join(rules, "\n"), len(rules), size, c.FetchedAt)
	return c, err
}

// 规则分类：用于在页面上按域名、IP 段等分开显示。
const (
	kindDomain  = "domain"  // 完整域名
	kindSuffix  = "suffix"  // 域名后缀（含子域名）
	kindKeyword = "keyword" // 域名关键字
	kindRegex   = "regex"   // 域名正则、通配
	kindIP      = "ip"      // IP 段、ASN、GEOIP
	kindOther   = "other"   // 进程、端口等
)

// classifyRule 按规则集的内容类型解析一行，返回类别和值（域名、网段等）。
func classifyRule(line, behavior string) (kind, value string) {
	if behavior == "" {
		switch {
		case strings.Contains(line, ","):
			behavior = "classical"
		case strings.Contains(line, "/") || strings.Count(line, ":") > 1:
			behavior = "ipcidr"
		default:
			behavior = "domain"
		}
	}
	switch behavior {
	case "ipcidr":
		return kindIP, line
	case "domain":
		switch {
		case strings.HasPrefix(line, "+."):
			return kindSuffix, line[2:]
		case strings.HasPrefix(line, "."):
			return kindSuffix, line[1:]
		case strings.Contains(line, "*"):
			return kindRegex, line
		}
		return kindDomain, line
	}
	typ, rest, _ := strings.Cut(line, ",")
	val, _, _ := strings.Cut(rest, ",")
	val = strings.TrimSpace(val)
	switch strings.ToUpper(strings.TrimSpace(typ)) {
	case "DOMAIN":
		return kindDomain, val
	case "DOMAIN-SUFFIX":
		return kindSuffix, val
	case "DOMAIN-KEYWORD":
		return kindKeyword, val
	case "DOMAIN-REGEX", "DOMAIN-WILDCARD", "GEOSITE":
		return kindRegex, val
	case "IP-CIDR", "IP-CIDR6", "IP-SUFFIX", "IP-ASN", "GEOIP", "SRC-IP-CIDR":
		return kindIP, val
	}
	return kindOther, line
}

func isDomainKind(k string) bool {
	return k == kindDomain || k == kindSuffix || k == kindKeyword || k == kindRegex
}

// RuleSetItem 是规则集里的一条。
type RuleSetItem struct {
	Rule  string `json:"rule"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// RuleSetPreview 是规则集内容的一页（按类别、关键字筛选）。
type RuleSetPreview struct {
	URL       string         `json:"url"`
	Count     int            `json:"count"`
	Kinds     map[string]int `json:"kinds"`
	Matched   int            `json:"matched"` // 筛选后的条数
	Items     []RuleSetItem  `json:"items"`
	Bytes     int            `json:"bytes"`
	FetchedAt string         `json:"fetched_at"`
	Cached    bool           `json:"cached"`
	Warning   string         `json:"warning,omitempty"`
}

// PreviewQuery 是查看规则集内容的请求。Kind 为空表示全部，"domains" 表示所有域名类。
type PreviewQuery struct {
	URL      string `json:"url"`
	Format   string `json:"format"`
	Behavior string `json:"behavior"`
	Refresh  bool   `json:"refresh"`
	Q        string `json:"q"`
	Kind     string `json:"kind"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// PreviewRuleSet 由管理服务取规则集（优先用缓存）并解析，按类别和关键字筛选后分页返回。
func (s *Server) PreviewRuleSet(ctx context.Context, in PreviewQuery) (RuleSetPreview, error) {
	c, err := s.loadRuleSet(ctx, in.URL, in.Format, in.Refresh)
	if err != nil {
		return RuleSetPreview{}, err
	}
	if in.Limit <= 0 || in.Limit > maxPreviewPage {
		in.Limit = 200
	}
	q := strings.ToLower(strings.TrimSpace(in.Q))
	out := RuleSetPreview{URL: c.URL, Count: len(c.Rules), Kinds: map[string]int{}, Items: []RuleSetItem{}, Bytes: c.Bytes,
		FetchedAt: c.FetchedAt, Cached: c.Cached, Warning: c.Warning}
	for _, line := range c.Rules {
		kind, value := classifyRule(line, in.Behavior)
		out.Kinds[kind]++
		switch {
		case in.Kind == "domains" && !isDomainKind(kind):
			continue
		case in.Kind != "" && in.Kind != "domains" && in.Kind != kind:
			continue
		case q != "" && !strings.Contains(strings.ToLower(line), q):
			continue
		}
		if out.Matched >= in.Offset && len(out.Items) < in.Limit {
			out.Items = append(out.Items, RuleSetItem{Rule: line, Kind: kind, Value: value})
		}
		out.Matched++
	}
	return out, nil
}

// RuleSetStat 是规则集的统计：条数、各类别条数和前几个域名。
type RuleSetStat struct {
	URL       string         `json:"url"`
	Format    string         `json:"format"`
	Count     int            `json:"count"`
	Kinds     map[string]int `json:"kinds"`
	Sample    []string       `json:"sample"` // 前几个域名（没有域名时是前几条规则）
	FetchedAt string         `json:"fetched_at,omitempty"`
	Cached    bool           `json:"cached"`
	Missing   bool           `json:"missing,omitempty"` // 没有缓存，也没有要求下载
	Error     string         `json:"error,omitempty"`
}

// RuleSetStats 统计一组规则集。fetch 为 false 时只读缓存；为 true 时没缓存（或过期）的下载；refresh 时全部重新下载。
func (s *Server) RuleSetStats(ctx context.Context, list []gen.RuleSet, fetch, refresh bool) []RuleSetStat {
	out := []RuleSetStat{}
	for _, rs := range list {
		if rs.Type == gen.TypeGeoIP || rs.URL == "" || rs.Format == "mrs" {
			continue
		}
		st := RuleSetStat{URL: rs.URL, Format: rs.Format, Kinds: map[string]int{}, Sample: []string{}}
		var c cachedRuleSet
		var err error
		if !fetch && !refresh {
			var ok bool
			if c, ok = s.readRuleSetCache(ctx, strings.TrimSpace(rs.URL), rs.Format); !ok {
				st.Missing = true
				out = append(out, st)
				continue
			}
		} else if c, err = s.loadRuleSet(ctx, rs.URL, rs.Format, refresh); err != nil {
			st.Error = err.Error()
			out = append(out, st)
			continue
		}
		st.Count, st.FetchedAt, st.Cached, st.Error = len(c.Rules), c.FetchedAt, c.Cached, c.Warning
		var other []string
		for _, line := range c.Rules {
			kind, value := classifyRule(line, rs.Behavior)
			st.Kinds[kind]++
			if isDomainKind(kind) && len(st.Sample) < sampleDomains {
				st.Sample = append(st.Sample, value)
			} else if len(other) < sampleDomains {
				other = append(other, value)
			}
		}
		if len(st.Sample) == 0 {
			st.Sample = other
		}
		out = append(out, st)
	}
	return out
}

// fetchRuleSet 下载并解析规则集，返回规范化的地址、全部规则和字节数。
func (s *Server) fetchRuleSet(ctx context.Context, rawURL, format string) (string, []string, int, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", nil, 0, &InputError{"地址须是 http(s) 链接"}
	}
	ctx, cancel := context.WithTimeout(ctx, ruleSetFetchLimit)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if s.FetchProxy != "" {
		if p, err := url.Parse(s.FetchProxy); err == nil {
			transport.Proxy = http.ProxyURL(p)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, 0, err
	}
	req.Header.Set("User-Agent", "clash.meta") // 部分规则源按 UA 返回 Clash 格式
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return "", nil, 0, fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, 0, fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRuleSetBody+1))
	if err != nil {
		return "", nil, 0, fmt.Errorf("下载失败：%w", err)
	}
	if len(body) > maxRuleSetBody {
		return "", nil, 0, &InputError{"规则集超过 16 MB，无法预览"}
	}
	rules, err := gen.ParseRuleSet(body, format)
	if err != nil {
		return "", nil, 0, &InputError{err.Error()}
	}
	return u.String(), rules, len(body), nil
}

func (s *Server) ruleSetRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	mux.Handle("GET /api/v1/dist/rule-sets", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.RuleSets(r.Context())
		if err != nil {
			writeErr(w, err, log)
			return
		}
		custom := list != nil
		if !custom {
			list = gen.DefaultRuleSets()
		}
		writeJSON(w, http.StatusOK, map[string]any{"rule_sets": list, "custom": custom})
	}))
	mux.Handle("GET /api/v1/dist/rule-sets/defaults", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, gen.DefaultRuleSets())
	}))
	// rule_sets 为 null 表示恢复内置默认。
	mux.Handle("PUT /api/v1/dist/rule-sets", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RuleSets []gen.RuleSet `json:"rule_sets"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.SetRuleSets(r.Context(), in.RuleSets); err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	mux.Handle("POST /api/v1/dist/rule-sets/preview", authed(func(w http.ResponseWriter, r *http.Request) {
		var in PreviewQuery
		if !decode(w, r, &in) {
			return
		}
		p, err := s.PreviewRuleSet(r.Context(), in)
		var ie *InputError
		switch {
		case err == nil:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, p)
		case errors.As(err, &ie):
			writeErr(w, err, log)
		default:
			// 远端下载失败不是管理服务的故障。
			writeJSON(w, http.StatusBadGateway, apiError{"fetch_failed", err.Error()})
		}
	}))
	// 统计：fetch=false 只读缓存（页面打开时用），fetch=true 下载没缓存或过期的，refresh=true 全部重新下载。
	mux.Handle("POST /api/v1/dist/rule-sets/stats", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RuleSets []gen.RuleSet `json:"rule_sets"`
			Fetch    bool          `json:"fetch"`
			Refresh  bool          `json:"refresh"`
		}
		if !decode(w, r, &in) {
			return
		}
		writeJSON(w, http.StatusOK, s.RuleSetStats(r.Context(), in.RuleSets, in.Fetch, in.Refresh))
	}))
}
