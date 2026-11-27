package core

import (
	"fmt"
	"maps"
	"strings"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/config"
)

// builtinTargets 是内核内置的出站，规则和代理组可以直接引用。
var builtinTargets = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true}

// parseInProcess 在进程内解析配置（M8-03），先于 `mihomo -t` 发现问题并给出更具体的原因：
// YAML 结构、每个节点内核能否解析、代理组成员和规则目标是否存在、规则集是否定义、MATCH 是否唯一且在最后。
// 只做不产生副作用的检查；规则集下载、geo 数据由 `mihomo -t` 负责。
func parseInProcess(b []byte) error {
	raw, err := config.UnmarshalRawConfig(b)
	if err != nil {
		return fmt.Errorf("YAML 解析失败：%w", err)
	}
	names := maps.Clone(builtinTargets)
	for i, p := range raw.Proxy {
		name, _ := p["name"].(string)
		if name == "" {
			return fmt.Errorf("第 %d 个节点没有名称", i+1)
		}
		if names[name] {
			return fmt.Errorf("节点名称重复：%s", name)
		}
		px, err := adapter.ParseProxy(maps.Clone(p))
		if err != nil {
			return fmt.Errorf("节点「%s」无法解析：%w", name, err)
		}
		_ = px.Close()
		names[name] = true
	}
	for _, g := range raw.ProxyGroup {
		name, _ := g["name"].(string)
		if name == "" {
			return fmt.Errorf("有代理组没有名称")
		}
		if names[name] {
			return fmt.Errorf("代理组名称与节点或其他组重复：%s", name)
		}
		names[name] = true
	}
	for _, g := range raw.ProxyGroup {
		members, _ := g["proxies"].([]any)
		for _, m := range members {
			if s, _ := m.(string); !names[s] {
				return fmt.Errorf("代理组「%v」的成员「%v」不存在", g["name"], m)
			}
		}
	}
	for i, r := range raw.Rule {
		target, typ := ruleTarget(r)
		last := i == len(raw.Rule)-1
		if typ == "MATCH" && !last {
			return fmt.Errorf("MATCH 只能是最后一条规则（第 %d 条）", i+1)
		}
		if last && typ != "MATCH" {
			return fmt.Errorf("最后一条规则须是 MATCH")
		}
		if typ == "RULE-SET" {
			parts := strings.Split(r, ",")
			if len(parts) < 3 {
				return fmt.Errorf("规则格式错误：%s", r)
			}
			if _, ok := raw.RuleProvider[strings.TrimSpace(parts[1])]; !ok {
				return fmt.Errorf("规则「%s」引用的规则集没有定义", r)
			}
		}
		if typ == "SUB-RULE" {
			if _, ok := raw.SubRules[target]; !ok {
				return fmt.Errorf("规则「%s」引用的子规则没有定义", r)
			}
			continue
		}
		if !names[target] {
			return fmt.Errorf("规则「%s」的目标「%s」不存在", r, target)
		}
	}
	return nil
}

// ruleOptions 是规则末尾的参数，不是目标。
var ruleOptions = map[string]bool{"no-resolve": true, "src": true}

// ruleTarget 返回规则的类型和目标。逻辑规则 AND,((…),(…)),目标 的目标在最后一个 ")," 之后。
func ruleTarget(rule string) (target, typ string) {
	typ, rest, _ := strings.Cut(rule, ",")
	typ = strings.ToUpper(strings.TrimSpace(typ))
	switch typ {
	case "MATCH":
		return strings.TrimSpace(rest), typ
	case "AND", "OR", "NOT", "SUB-RULE":
		if i := strings.LastIndex(rest, "),"); i >= 0 {
			rest = rest[i+2:]
		}
		parts := strings.Split(rest, ",")
		return strings.TrimSpace(parts[0]), typ
	}
	parts := strings.Split(rest, ",")
	for len(parts) > 2 && ruleOptions[strings.TrimSpace(parts[len(parts)-1])] {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 2 {
		return "", typ
	}
	return strings.TrimSpace(parts[len(parts)-1]), typ
}
