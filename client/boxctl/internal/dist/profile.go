// Package dist 是配置下发：管理服务（boxctl server）发布配置和更新包，Banbu Connect 客户端拉取。
//
// 服务端为准：下发的节点、代理组、规则和线路覆盖客户端同名的本地配置；客户端在「高级」里
// 追加的本地节点、代理组、规则与下发的合并，同名时以下发的为准。
package dist

import (
	"strconv"
	"time"

	"boxctl/internal/gen"
	"boxctl/internal/groups"
)

// Profile 是一次发布的配置。节点是 Mihomo 的 proxies 条目（含凭据），代理组已解析出成员。
type Profile struct {
	Version     int64             `json:"version"`
	PublishedAt time.Time         `json:"published_at"`
	Note        string            `json:"note"`
	Nodes       []map[string]any  `json:"nodes"`
	Groups      []groups.Export   `json:"groups"`
	Selections  map[string]string `json:"selections"` // 线路：手动选择类的组 → 默认选中的成员
	Rules       []string          `json:"rules"`      // 放在客户端自定义规则之前
	// StrictEgress 是严格线路允许的出口 IP 或网段。不为空时，客户端经每个严格组探测出口，
	// 任何一组不在列表里，Kill Switch 就阻断受保护的服务。
	StrictEgress []string `json:"strict_egress,omitempty"`
	// BoxSecret 是客户端发现路由盒用的共享口令（ADR-073）：客户端验证路由盒，绑定了管理服务的路由盒用它应答。
	// 为空表示不启用自动发现。整份配置加密保存、经设备令牌下发。
	BoxSecret string `json:"box_secret,omitempty"`
	// RuleSets 是内置规则（远程规则集、GEOIP）；为空时客户端用自己的默认（gen.DefaultRuleSets）。
	RuleSets []gen.RuleSet `json:"rule_sets"` // 不能 omitempty：空列表（管理员删光）和没配置（null）含义不同
	// Variants 是发布时快照的规则方案（ADR-062），只存在管理服务里；下发前由 ForDevice 换成客户端自己的那份并去掉。
	Variants []RuleVariant `json:"variants,omitempty"`
	// RuleProfile 是这台客户端用的方案名；用默认方案时为空。
	RuleProfile string `json:"rule_profile,omitempty"`
	// ProfileVersion 是方案的版本号（代码-时间戳，ADR-064）。
	ProfileVersion string `json:"profile_version,omitempty"`
}

// Merged 是合并后交给配置生成器的内容。
type Merged struct {
	Nodes    []map[string]any
	Groups   []groups.Export
	Rules    []string
	RuleSets []gen.RuleSet // nil 表示用默认
	Warnings []string
}

// Merge 把下发的配置和本地配置合并。同名时下发的为准，本地的跳过并给出提示。
func Merge(p Profile, nodes []map[string]any, gs []groups.Export, rules []string) Merged {
	m := Merged{}
	names := map[string]bool{}
	for _, n := range p.Nodes {
		name, _ := n["name"].(string)
		names[name] = true
		m.Nodes = append(m.Nodes, n)
	}
	for _, g := range p.Groups {
		names[g.Name] = true
		m.Groups = append(m.Groups, g)
	}
	var skipped []string
	for _, n := range nodes {
		name, _ := n["name"].(string)
		if names[name] {
			skipped = append(skipped, name)
			continue
		}
		names[name] = true
		m.Nodes = append(m.Nodes, n)
	}
	for _, g := range gs {
		if names[g.Name] {
			// 本地默认的代理组（Claude、ChatGPT 等）被下发的同名组取代是正常情况，不提示。
			continue
		}
		names[g.Name] = true
		m.Groups = append(m.Groups, g)
	}
	if len(skipped) > 0 {
		m.Warnings = append(m.Warnings, "这些本地节点与下发的节点或代理组同名，已忽略："+joinMax(skipped, 5))
	}
	m.Rules = append(append([]string{}, p.Rules...), rules...)
	m.RuleSets = p.RuleSets
	return m
}

func joinMax(list []string, max int) string {
	out := ""
	for i, v := range list {
		if i == max {
			return out + " 等 " + strconv.Itoa(len(list)) + " 个"
		}
		if i > 0 {
			out += "、"
		}
		out += v
	}
	return out
}
