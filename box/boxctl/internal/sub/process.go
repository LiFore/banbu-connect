package sub

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Options 是单条订阅的清洗与拉取参数，存于 subscriptions.options。
type Options struct {
	// FilterPatterns：名称匹配任一正则的节点视为信息节点并丢弃（M2-05）。
	FilterPatterns []string `json:"filter_patterns"`
	// KeepUnmatched：无法识别地区时 true 归入“其他”，false 丢弃（M2-06）。
	KeepUnmatched bool `json:"keep_unmatched"`
	// NameTemplate：显示名模板，变量 {region} {seq} {seq:02} {sub} {name} {protocol}（M2-07）。
	NameTemplate string `json:"name_template"`
	TimeoutSec   int    `json:"timeout_sec"`
	Retries      int    `json:"retries"`
	// UpdateHours 是自动更新的间隔（小时），0 表示不自动更新（M2-11）。
	UpdateHours int `json:"update_hours"`
}

func DefaultOptions() Options {
	return Options{
		FilterPatterns: []string{
			"剩余", "流量", "到期", "过期", "重置", "官网", "网址", "公告", "客服",
			"节点数量", "下载", "没节点", "(?i)expire", "(?i)traffic",
		},
		KeepUnmatched: false,
		NameTemplate:  "{region} - {seq:02} - {sub}",
		TimeoutSec:    30,
		Retries:       2,
		UpdateHours:   12,
	}
}

var templateVar = regexp.MustCompile(`\{(\w+)(?::(\d+))?\}`)

var knownVars = map[string]bool{"region": true, "seq": true, "sub": true, "name": true, "protocol": true}

// Validate 检查正则可编译、模板合法。模板必须含 {seq}，保证同一订阅内显示名唯一。
func (o Options) Validate() error {
	for _, p := range o.FilterPatterns {
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("过滤规则 %q 不是合法正则：%v", p, err)
		}
	}
	hasSeq := false
	for _, m := range templateVar.FindAllStringSubmatch(o.NameTemplate, -1) {
		if !knownVars[m[1]] {
			return fmt.Errorf("显示名模板含未知变量 {%s}", m[1])
		}
		hasSeq = hasSeq || m[1] == "seq"
	}
	if !hasSeq {
		return errors.New("显示名模板必须包含 {seq}")
	}
	if o.TimeoutSec < 1 || o.TimeoutSec > 300 {
		return errors.New("超时须在 1–300 秒之间")
	}
	if o.UpdateHours < 0 || o.UpdateHours > 24*7 {
		return errors.New("自动更新间隔须在 0–168 小时之间（0 为不自动更新）")
	}
	if o.Retries < 0 || o.Retries > 5 {
		return errors.New("重试次数须在 0–5 之间")
	}
	return nil
}

// Node 是清洗后的订阅节点。Mapping 含凭据，入库前须 SplitSecrets。
type Node struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"` // 订阅中的原始名称
	DisplayName string         `json:"display_name"`
	Region      string         `json:"region"`
	Protocol    string         `json:"protocol"`
	Server      string         `json:"server"`
	Port        int            `json:"port"`
	Mapping     map[string]any `json:"-"`
}

type InvalidNode struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// Report 汇总清洗过程，供预览界面展示。只含名称与统计，不含凭据。
type Report struct {
	Parsed     int            `json:"parsed"`
	Protocols  map[string]int `json:"protocols"` // 解析得到的协议分布
	Filtered   []string       `json:"filtered"`  // 信息节点
	Unmatched  []string       `json:"unmatched"` // 地区无法识别而丢弃
	Invalid    []InvalidNode  `json:"invalid"`   // Mihomo 校验不通过
	Duplicates []string       `json:"duplicates"`
	Kept       int            `json:"kept"`
	Regions    map[string]int `json:"regions"` // 保留节点的地区分布
}

// Validator 校验单个代理配置；生产中为 ValidateWithMihomo。
type Validator func(map[string]any) error

// Process 依次执行：信息节点过滤 → 校验 → 地区识别 → 稳定 ID 去重 → 显示名生成。
func Process(raw []map[string]any, subName string, opts Options, validate Validator) ([]Node, Report) {
	rep := Report{Protocols: map[string]int{}, Regions: map[string]int{},
		Filtered: []string{}, Unmatched: []string{}, Invalid: []InvalidNode{}, Duplicates: []string{}}

	filters := make([]*regexp.Regexp, 0, len(opts.FilterPatterns))
	for _, p := range opts.FilterPatterns {
		filters = append(filters, regexp.MustCompile(p)) // 已由 Validate 保证
	}

	seen := map[string]bool{}
	seq := map[string]int{}
	var nodes []Node
	for _, m := range raw {
		name := str(m, "name")
		proto := str(m, "type")
		rep.Parsed++
		rep.Protocols[proto]++

		if matchAny(filters, name) {
			rep.Filtered = append(rep.Filtered, name)
			continue
		}
		if p := PortOf(m); p > 0 {
			m["port"] = p // 统一为 int，避免 JSON float64 进入配置
		}
		if validate != nil {
			if err := validate(m); err != nil {
				rep.Invalid = append(rep.Invalid, InvalidNode{Name: name, Error: err.Error()})
				continue
			}
		}
		region := MatchRegion(name)
		if region == "" {
			if !opts.KeepUnmatched {
				rep.Unmatched = append(rep.Unmatched, name)
				continue
			}
			region = RegionOther
		}
		id := NodeID(m)
		if seen[id] {
			rep.Duplicates = append(rep.Duplicates, name)
			continue
		}
		seen[id] = true

		seq[region]++
		nodes = append(nodes, Node{
			ID:       id,
			Name:     name,
			Region:   region,
			Protocol: proto,
			Server:   str(m, "server"),
			Port:     PortOf(m),
			DisplayName: renderName(opts.NameTemplate, map[string]string{
				"region": region, "sub": subName, "name": name, "protocol": proto,
			}, seq[region]),
			Mapping: m,
		})
		rep.Regions[region]++
	}
	rep.Kept = len(nodes)
	return nodes, rep
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func renderName(tpl string, vars map[string]string, seq int) string {
	return templateVar.ReplaceAllStringFunc(tpl, func(tok string) string {
		m := templateVar.FindStringSubmatch(tok)
		if m[1] != "seq" {
			return vars[m[1]]
		}
		width, _ := strconv.Atoi(m[2])
		s := strconv.Itoa(seq)
		if len(s) < width {
			s = strings.Repeat("0", width-len(s)) + s
		}
		return s
	})
}
