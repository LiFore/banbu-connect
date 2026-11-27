package nodes

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/metacubex/mihomo/common/convert"

	"boxctl/internal/sub"
)

// Candidate 是一条待保存的导入结果。Mapping 含用户刚粘贴的凭据，只回给发起导入的会话。
type Candidate struct {
	Kind        string         `json:"kind"` // manual / isp
	DisplayName string         `json:"display_name"`
	Region      string         `json:"region"`
	Protocol    string         `json:"protocol"`
	Summary     string         `json:"summary"` // 协议://server:port，不含凭据
	Mapping     map[string]any `json:"mapping,omitempty"`
	Warning     string         `json:"warning,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// ParseImport 识别粘贴内容并逐条解析（M3-03/04/05）：
//   - curl 命令（-x/--proxy、-U/--proxy-user、--socks5[-hostname]）→ ISP 代理
//   - http(s)://、socks5(h):// 代理 URL，host:port:user:pass，user:pass@host:port → ISP 代理
//   - vless:// hy2:// anytls:// trojan:// ss:// tuic:// vmess:// 等分享链接 → 自建隧道
//   - Mihomo YAML 片段（单个节点或 proxies: 列表）→ 自建隧道
func ParseImport(text string, validate sub.Validator) []Candidate {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	var cands []Candidate

	switch {
	case looksLikeYAML(text):
		cands = parseYAML(text)
	default:
		text = strings.ReplaceAll(text, "\\\n", " ") // shell 续行
		lines := strings.Split(text, "\n")
		if len(lines) == 1 && !strings.Contains(text, "://") && !strings.Contains(text, " ") && !looksLikeHostPort(text) {
			// 可能是 Base64 编码的链接列表
			if ms, _ := convert.ConvertsV2Ray([]byte(text)); len(ms) > 0 {
				for _, m := range ms {
					cands = append(cands, Candidate{Kind: KindManual, Mapping: m})
				}
				break
			}
		}
		for _, line := range lines {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			cands = append(cands, parseLine(line))
		}
	}
	finalize(cands, validate)
	return cands
}

func parseLine(line string) Candidate {
	if strings.HasPrefix(line, "curl ") || line == "curl" {
		return parseCurl(line)
	}
	if scheme, _, ok := strings.Cut(line, "://"); ok {
		if isProxyScheme(strings.ToLower(scheme)) {
			return ispFromURL(line, "", "")
		}
		ms, _ := convert.ConvertsV2Ray([]byte(line))
		if len(ms) == 0 {
			return Candidate{Kind: KindManual, Summary: scheme + "://…",
				Error: fmt.Sprintf("无法解析 %s:// 链接：格式错误或不支持（支持 vless、vmess、trojan、ss、ssr、hysteria、hysteria2/hy2、tuic、anytls）", scheme)}
		}
		return Candidate{Kind: KindManual, Mapping: ms[0]}
	}
	if c, ok := parseHostPortUserPass(line); ok {
		return c
	}
	if strings.Contains(line, "@") {
		return ispFromURL("http://"+line, "", "")
	}
	return Candidate{Error: "无法识别：既不是分享链接、代理地址，也不是 curl 命令"}
}

func isProxyScheme(s string) bool {
	switch s {
	case "http", "https", "socks5", "socks5h", "socks":
		return true
	}
	return false
}

// parseCurl 从 curl 命令中提取代理参数，忽略目标 URL 与其它选项。
func parseCurl(line string) Candidate {
	args, err := shellSplit(line)
	if err != nil {
		return Candidate{Kind: KindISP, Error: "curl 命令解析失败：" + err.Error()}
	}
	var proxy, user, socks string
	for i := 1; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-x" || a == "--proxy":
			proxy = next()
		case strings.HasPrefix(a, "--proxy="):
			proxy = strings.TrimPrefix(a, "--proxy=")
		case strings.HasPrefix(a, "-x") && len(a) > 2:
			proxy = a[2:]
		case a == "-U" || a == "--proxy-user":
			user = next()
		case strings.HasPrefix(a, "--proxy-user="):
			user = strings.TrimPrefix(a, "--proxy-user=")
		case a == "--socks5" || a == "--socks5-hostname":
			socks = next()
		}
	}
	switch {
	case socks != "":
		proxy = "socks5://" + socks
	case proxy == "":
		return Candidate{Kind: KindISP, Error: "curl 命令中没有找到 -x / --proxy / --socks5 参数"}
	case !strings.Contains(proxy, "://"):
		proxy = "http://" + proxy // 与 curl 默认一致
	}
	u, p, _ := strings.Cut(user, ":")
	return ispFromURL(proxy, u, p)
}

// ispFromURL 把代理 URL 转为 http/socks5 节点；overrideUser 非空时优先于 URL 中的用户信息。
func ispFromURL(raw, overrideUser, overridePass string) Candidate {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return Candidate{Kind: KindISP, Error: "代理地址格式错误"}
	}
	m := map[string]any{"server": u.Hostname()}
	switch strings.ToLower(u.Scheme) {
	case "http":
		m["type"] = "http"
	case "https":
		m["type"] = "http"
		m["tls"] = true
	case "socks5", "socks5h", "socks":
		m["type"] = "socks5"
		m["udp"] = true
	default:
		return Candidate{Kind: KindISP, Error: "不支持的代理协议 " + u.Scheme + "（支持 http、https、socks5）"}
	}
	port := 1080 // curl 对未写端口的代理默认 1080
	if ps := u.Port(); ps != "" {
		if port, err = strconv.Atoi(ps); err != nil || port < 1 || port > 65535 {
			return Candidate{Kind: KindISP, Error: "代理端口无效"}
		}
	}
	m["port"] = port
	user, pass := overrideUser, overridePass
	if user == "" && u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	if user != "" {
		m["username"] = user
		m["password"] = pass
	}
	return Candidate{Kind: KindISP, Mapping: m}
}

// parseHostPortUserPass 解析供应商常见的 host:port:user:pass（密码中可含冒号）。
func parseHostPortUserPass(line string) (Candidate, bool) {
	parts := strings.SplitN(line, ":", 4)
	if len(parts) != 4 || strings.ContainsAny(line, " @/") {
		return Candidate{}, false
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return Candidate{}, false
	}
	return ispFromURL("http://"+net.JoinHostPort(parts[0], parts[1]), parts[2], parts[3]), true
}

func looksLikeHostPort(s string) bool {
	_, _, err := net.SplitHostPort(s)
	return err == nil || strings.Count(s, ":") == 3
}

func looksLikeYAML(text string) bool {
	first := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	return strings.HasPrefix(first, "proxies:") || strings.HasPrefix(first, "- ") ||
		strings.HasPrefix(first, "{") || strings.HasPrefix(first, "[") ||
		strings.HasPrefix(first, "name:") || strings.HasPrefix(first, "type:")
}

// parseYAML 依次尝试：原文（proxies: 列表或数组）、包装为单个块状节点、包装为单个流式节点。
func parseYAML(text string) []Candidate {
	attempts := []string{text}
	if strings.HasPrefix(text, "{") {
		attempts = append(attempts, "proxies: ["+text+"]")
	} else {
		attempts = append(attempts, "proxies:\n  - "+strings.ReplaceAll(text, "\n", "\n    "))
	}
	lastErr := errors.New("没有找到节点")
	for _, t := range attempts {
		ms, format, err := sub.Parse([]byte(t))
		if err != nil || format != sub.FormatClash {
			if err != nil {
				lastErr = err
			}
			continue
		}
		out := make([]Candidate, 0, len(ms))
		for _, m := range ms {
			kind := KindManual
			if t, _ := m["type"].(string); t == "http" || t == "socks5" {
				kind = KindISP // 与 curl 写法一致，否则没有 isp 标签，会混进默认的自动选择组
			}
			out = append(out, Candidate{Kind: kind, Mapping: m})
		}
		return out
	}
	return []Candidate{{Kind: KindManual, Error: "YAML 解析失败：" + lastErr.Error()}}
}

// finalize 补齐显示名、地区、摘要，剥离 dialer-proxy，并逐条校验。
func finalize(cands []Candidate, validate sub.Validator) {
	used := map[string]int{}
	for i := range cands {
		c := &cands[i]
		if c.Error != "" || c.Mapping == nil {
			continue
		}
		m := c.Mapping
		c.Protocol, _ = m["type"].(string)
		c.Summary = fmt.Sprintf("%s://%v:%v", c.Protocol, m["server"], m["port"])
		if _, ok := m["dialer-proxy"]; ok {
			delete(m, "dialer-proxy")
			c.Warning = "已忽略原配置中的 dialer-proxy，请在保存后选择前置代理"
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			if c.Kind == KindISP {
				name = fmt.Sprintf("ISP %v", m["server"])
			} else {
				name = fmt.Sprintf("%s %v", c.Protocol, m["server"])
			}
		}
		used[name]++
		if n := used[name]; n > 1 {
			name = fmt.Sprintf("%s %d", name, n)
		}
		c.DisplayName = name
		delete(m, "name")
		c.Region = sub.MatchRegion(name)
		if validate != nil {
			if err := validate(withName(m, name)); err != nil {
				c.Error = "校验失败：" + err.Error()
			}
		}
	}
}

func withName(m map[string]any, name string) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["name"] = name
	return out
}

// shellSplit 按 POSIX shell 规则切分参数：支持单引号、双引号与反斜杠转义。
func shellSplit(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg, quote := false, rune(0)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			if r == '"' {
				quote = 0
			} else if r == '\\' && i+1 < len(runes) && strings.ContainsRune(`"\$`+"`", runes[i+1]) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inArg = true
		case r == ' ' || r == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("引号未闭合")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}
