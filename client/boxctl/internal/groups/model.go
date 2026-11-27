// Package groups 管理代理组（M4）：显式/动态成员、严格组、空组兜底、模板，
// 以及组、节点、前置代理组成的整张图的一致性校验（环路、严格组可达 DIRECT）。
package groups

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	TypeSelect      = "select"
	TypeURLTest     = "url-test"
	TypeFallback    = "fallback"
	TypeLoadBalance = "load-balance"

	Direct = "DIRECT"
	Reject = "REJECT"

	DefaultTestURL = "https://www.gstatic.com/generate_204"
)

// Filter 是动态成员条件（M4-03）：同一字段内为“或”，字段之间为“且”；
// 未启用时不产生动态成员；启用且全部包含条件为空时匹配所有节点（再应用排除条件）。
type Filter struct {
	Enabled       bool     `json:"enabled"`
	Regions       []string `json:"regions"`
	Subscriptions []string `json:"subscriptions"` // 订阅 id
	Protocols     []string `json:"protocols"`
	Kinds         []string `json:"kinds"` // subscription / manual / isp
	Tags          []string `json:"tags"`
	ExcludeTags   []string `json:"exclude_tags"`
	NameRegex     string   `json:"name_regex"`
}

// Members 对应 proxy_groups.members。Explicit 为有序引用：节点 id、组 id、DIRECT、REJECT。
type Members struct {
	Explicit []string `json:"explicit"`
	Filter   Filter   `json:"filter"`
}

type Options struct {
	URL            string `json:"url,omitempty"`
	Interval       int    `json:"interval,omitempty"`
	Tolerance      int    `json:"tolerance,omitempty"`
	Timeout        int    `json:"timeout,omitempty"`
	MaxFailedTimes int    `json:"max_failed_times,omitempty"`
	Lazy           bool   `json:"lazy,omitempty"`
	Strategy       string `json:"strategy,omitempty"` // load-balance
	Hidden         bool   `json:"hidden,omitempty"`
	// EmptyFallback：成员为空时的替代成员（组 id 或 REJECT），默认 REJECT（M4-04）。
	EmptyFallback string `json:"empty_fallback,omitempty"`
}

type Group struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Strict    bool    `json:"strict"`
	SortOrder int     `json:"sort_order"`
	Members   Members `json:"members"`
	Options   Options `json:"options"`
	Icon      string  `json:"icon"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

var reservedNames = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true,
	"COMPATIBLE": true, "GLOBAL": true}

var strategies = map[string]bool{"consistent-hashing": true, "round-robin": true, "sticky-sessions": true}

// normalize 填默认值并做与图无关的字段校验（M4-01）。
func (g *Group) normalize() error {
	g.Name = strings.TrimSpace(g.Name)
	if n := len([]rune(g.Name)); n == 0 || n > 64 {
		return invalid("组名须为 1–64 字")
	}
	if reservedNames[strings.ToUpper(g.Name)] {
		return invalid("%s 是保留名称", g.Name)
	}
	o := &g.Options
	switch g.Type {
	case TypeSelect:
		o.URL, o.Interval, o.Tolerance, o.Lazy, o.Strategy = "", 0, 0, false, ""
	case TypeURLTest, TypeFallback, TypeLoadBalance:
		if o.URL == "" {
			o.URL = DefaultTestURL
		}
		if !strings.HasPrefix(o.URL, "http://") && !strings.HasPrefix(o.URL, "https://") {
			return invalid("测试地址须为 http(s) 链接")
		}
		if o.Interval == 0 {
			o.Interval = 300
		}
		if o.Interval < 30 || o.Interval > 86400 {
			return invalid("测试间隔须在 30–86400 秒")
		}
		if o.Tolerance < 0 || o.Tolerance > 5000 || o.Timeout < 0 || o.Timeout > 60000 || o.MaxFailedTimes < 0 {
			return invalid("容差 / 超时 / 失败次数超出范围")
		}
		if g.Type != TypeURLTest {
			o.Tolerance = 0
		}
		if g.Type == TypeLoadBalance {
			if o.Strategy == "" {
				o.Strategy = "consistent-hashing"
			}
			if !strategies[o.Strategy] {
				return invalid("负载均衡策略须为 consistent-hashing / round-robin / sticky-sessions")
			}
		} else {
			o.Strategy = ""
		}
	default:
		return invalid("组类型须为 select / url-test / fallback / load-balance")
	}
	if o.EmptyFallback == "" {
		o.EmptyFallback = Reject
	}
	if o.EmptyFallback == Direct && g.Strict {
		return invalid("严格组的空组兜底不能是 DIRECT")
	}
	if g.Members.Filter.NameRegex != "" {
		if _, err := regexp.Compile(g.Members.Filter.NameRegex); err != nil {
			return invalid("名称正则无效：%v", err)
		}
	}
	seen := map[string]bool{}
	explicit := []string{}
	for _, ref := range g.Members.Explicit {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		if ref == g.ID {
			return invalid("组不能包含自身")
		}
		seen[ref] = true
		explicit = append(explicit, ref)
	}
	g.Members.Explicit = explicit
	f := &g.Members.Filter
	for _, l := range []*[]string{&f.Regions, &f.Subscriptions, &f.Protocols, &f.Kinds, &f.Tags, &f.ExcludeTags} {
		if *l == nil {
			*l = []string{}
		}
	}
	return nil
}

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{fmt.Sprintf(format, a...)} }
