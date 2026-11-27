package gen

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// RuleSet 是一条内置规则：远程规则集（RULE-SET，内核按 URL 定期下载）或 GEOIP。
// 列表的顺序就是规则的顺序，排在下发规则、自定义规则和受保护服务的规则之后、兜底规则之前。
// 管理服务上可以配置并随配置下发；没有下发时用 DefaultRuleSets。
type RuleSet struct {
	// Type 为 rule-set（默认）或 geoip。
	Type string `json:"type,omitempty"`
	// Name 是规则集名称（rule-provider 的键）；geoip 时是国家或地区代码，如 CN、LAN。
	Name string `json:"name"`
	// Behavior：domain、ipcidr、classical。
	Behavior string `json:"behavior,omitempty"`
	// Format：yaml（默认）、text、mrs。
	Format   string `json:"format,omitempty"`
	URL      string `json:"url,omitempty"`
	Interval int    `json:"interval,omitempty"` // 秒，默认 86400
	// Target 是命中后交给的代理组，或 DIRECT、REJECT。目标组不存在时这条跳过。
	Target    string `json:"target"`
	NoResolve bool   `json:"no_resolve,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

const (
	TypeRuleSet = "rule-set"
	TypeGeoIP   = "geoip"
)

func (r RuleSet) isGeoIP() bool { return r.Type == TypeGeoIP }

func (r RuleSet) rule() string {
	s := "RULE-SET," + r.Name + "," + r.Target
	if r.isGeoIP() {
		s = "GEOIP," + r.Name + "," + r.Target
	}
	if r.NoResolve {
		s += ",no-resolve"
	}
	return s
}

// DefaultRuleSets 是内置的规则（原来写死在生成器里的那份）。严格规则集（ChatGPT、Claude）在前。
// IP 类规则统一 no-resolve：不为了匹配规则而在本地解析域名，避免 DNS 泄漏。
func DefaultRuleSets() []RuleSet {
	ls := func(name, behavior, target string, noResolve bool) RuleSet {
		return RuleSet{Name: name, Behavior: behavior, URL: loyalsoldier + name + ".txt", Target: target, NoResolve: noResolve}
	}
	bm := func(name, path, target string) RuleSet {
		return RuleSet{Name: name, Behavior: "classical", URL: blackmatrix + path, Target: target}
	}
	geo := func(code string) RuleSet {
		return RuleSet{Type: TypeGeoIP, Name: code, Target: "DIRECT", NoResolve: true}
	}
	return []RuleSet{
		bm("openai", "OpenAI/OpenAI.yaml", "ChatGPT"),
		bm("claude", "Claude/Claude.yaml", "Claude"),
		ls("reject", "domain", "广告拦截", false),
		ls("direct", "domain", "DIRECT", false),
		ls("private", "domain", "DIRECT", false),
		ls("cncidr", "ipcidr", "DIRECT", true),
		ls("lancidr", "ipcidr", "DIRECT", true),
		geo("LAN"),
		geo("CN"),
		ls("applications", "classical", "DIRECT", false),
		bm("spotify", "Spotify/Spotify.yaml", "Spotify"),
		ls("telegramcidr", "ipcidr", "电报消息", true),
		ls("tld-not-cn", "domain", "代理模式", false),
		ls("google", "domain", "代理模式", false),
		ls("icloud", "domain", "代理模式", false),
		ls("apple", "domain", "代理模式", false),
		ls("gfw", "domain", "代理模式", false),
		ls("greatfire", "domain", "代理模式", false),
		ls("proxy", "domain", "代理模式", false),
	}
}

var ruleSetName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidateRuleSets 检查规则集列表，返回第一个问题。
func ValidateRuleSets(list []RuleSet) error {
	seen := map[string]bool{}
	for i, r := range list {
		where := fmt.Sprintf("第 %d 条（%s）", i+1, r.Name)
		if !ruleSetName.MatchString(r.Name) {
			return fmt.Errorf("第 %d 条：名称只能用字母、数字、_ . -", i+1)
		}
		if strings.TrimSpace(r.Target) == "" || strings.Contains(r.Target, ",") {
			return fmt.Errorf("%s：目标无效", where)
		}
		switch r.Type {
		case TypeGeoIP:
			continue
		case "", TypeRuleSet:
		default:
			return fmt.Errorf("%s：类型只能是 rule-set 或 geoip", where)
		}
		if seen[r.Name] {
			return fmt.Errorf("%s：名称重复", where)
		}
		seen[r.Name] = true
		switch r.Behavior {
		case "domain", "ipcidr", "classical":
		default:
			return fmt.Errorf("%s：behavior 只能是 domain、ipcidr、classical", where)
		}
		switch r.Format {
		case "", "yaml", "text", "mrs":
		default:
			return fmt.Errorf("%s：格式只能是 yaml、text、mrs", where)
		}
		if r.Format == "mrs" && r.Behavior == "classical" {
			return fmt.Errorf("%s：mrs 格式不支持 classical", where)
		}
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%s：地址须是 http(s) 链接", where)
		}
		if r.Interval < 0 {
			return fmt.Errorf("%s：更新间隔无效", where)
		}
	}
	return nil
}

// ruleSetRules 按顺序生成内置规则和用到的 rule-providers；停用的、目标不存在的跳过。
func ruleSetRules(list []RuleSet, targetExists func(string) bool, proxy, mirror string) (rules []string, providers map[string]any) {
	providers = map[string]any{}
	for _, r := range list {
		if r.Disabled || !targetExists(r.Target) {
			continue
		}
		rules = append(rules, r.rule())
		if r.isGeoIP() {
			continue
		}
		interval := r.Interval
		if interval == 0 {
			interval = 86400
		}
		ext := ".yaml"
		switch r.Format {
		case "text":
			ext = ".list"
		case "mrs":
			ext = ".mrs"
		}
		p := map[string]any{"type": "http", "behavior": r.Behavior, "url": MirrorURL(r.URL, mirror), "path": "./ruleset/" + r.Name + ext, "interval": interval}
		if r.Format != "" {
			p["format"] = r.Format
		}
		// 下载经代理组：国内直连 jsDelivr 常失败。组为空（直连模式）时直连。
		if proxy != "" {
			p["proxy"] = proxy
		}
		providers[r.Name] = p
	}
	return rules, providers
}

// ParseRuleSet 解析下载到的规则集内容，返回其中的规则（用于预览）。
// yaml 取 payload 列表；text 每行一条（# 和 // 开头为注释）；mrs 是二进制，无法预览。
func ParseRuleSet(body []byte, format string) ([]string, error) {
	switch format {
	case "mrs":
		return nil, fmt.Errorf("mrs 是二进制格式，无法显示内容")
	case "text":
		return textLines(body), nil
	}
	var doc struct {
		Payload []string `yaml:"payload"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil || doc.Payload == nil {
		// 有的 .txt/.list 实际是纯文本，按行读。
		if lines := textLines(body); len(lines) > 0 && !bytes.Contains(body, []byte("payload:")) {
			return lines, nil
		}
		if err != nil {
			return nil, fmt.Errorf("不是有效的规则集（yaml 解析失败：%v）", err)
		}
		return nil, fmt.Errorf("不是有效的规则集：没有 payload")
	}
	out := make([]string, 0, len(doc.Payload))
	for _, p := range doc.Payload {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func textLines(body []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "//") {
			continue
		}
		out = append(out, l)
	}
	return out
}
