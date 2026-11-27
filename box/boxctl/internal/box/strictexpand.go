package box

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/gen"
	"boxctl/internal/guard"
)

// 严格域名展开（ADR-039）：目标为严格组的远程规则集（如内置的 claude、openai），
// 把其中的域名和网段并入 guard 的保护范围；否则这些域名经严格组走代理，内核不在时却能直连。
// 结果存到数据目录，开机后不用等下载就能下发；规则集列表变化时或每 6 小时重新下载。

const (
	expandFile     = "strict-expand.json"
	expandRefresh  = 6 * time.Hour
	expandRetry    = 10 * time.Minute
	expandTick     = time.Minute
	expandTimeout  = 30 * time.Second
	expandMaxBody  = 8 << 20
	expandMaxItems = 20000 // 超过时截断并提示：dnsmasq 条目过多会拖慢解析
)

// ExpandedRuleSet 是一个规则集的展开结果，展示在保护服务页。
type ExpandedRuleSet struct {
	Name      string    `json:"name"`
	Group     string    `json:"group"`
	URL       string    `json:"url"`
	Suffixes  []string  `json:"suffixes"`
	CIDRs     []string  `json:"cidrs"`
	Skipped   int       `json:"skipped"`
	Samples   []string  `json:"samples,omitempty"` // 无法展开的条目示例
	Error     string    `json:"error,omitempty"`   // 最近一次下载或解析失败；保留上一次成功的结果
	UpdatedAt time.Time `json:"updated_at"`
}

// StrictExpansion 是全部展开结果。
type StrictExpansion struct {
	RuleSets  []ExpandedRuleSet `json:"rule_sets"`
	Truncated bool              `json:"truncated,omitempty"`
}

type expander struct {
	path     string
	sources  func(context.Context) ([]core.StrictRuleSet, error)
	proxy    func(context.Context) string
	fetch    func(ctx context.Context, rawURL, proxy string) ([]byte, error)
	onChange func()

	mu     sync.Mutex
	result StrictExpansion
	sig    string    // 上次展开时的规则集列表摘要
	next   time.Time // 下一次刷新时间
}

func newExpander(dataDir string, sources func(context.Context) ([]core.StrictRuleSet, error), proxy func(context.Context) string) *expander {
	e := &expander{path: filepath.Join(dataDir, expandFile), sources: sources, proxy: proxy, fetch: fetchRuleSet}
	if b, err := os.ReadFile(e.path); err == nil {
		_ = json.Unmarshal(b, &e.result)
	}
	return e
}

// Result 返回当前的展开结果。
func (e *expander) Result() StrictExpansion {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

// Merge 把展开结果并入策略：只加合法、未重复的条目，总数有上限。
func (e *expander) Merge(p *guard.Policy) {
	r := e.Result()
	seen := map[string]bool{}
	for _, s := range p.Suffixes {
		seen[s] = true
	}
	for _, c := range p.CIDRs {
		seen[c] = true
	}
	n := 0
	for _, rs := range r.RuleSets {
		for _, s := range rs.Suffixes {
			if n < expandMaxItems && !seen[s] && guard.ValidSuffix(s) {
				p.Suffixes, seen[s] = append(p.Suffixes, s), true
				n++
			}
		}
		for _, c := range rs.CIDRs {
			if _, err := netip.ParsePrefix(c); err == nil && n < expandMaxItems && !seen[c] {
				p.CIDRs, seen[c] = append(p.CIDRs, c), true
				n++
			}
		}
	}
}

// Run 定时检查：规则集列表变了或到了刷新时间就重新展开，结果变化时调用 onChange。
func (e *expander) Run(ctx context.Context, onChange func()) {
	t := time.NewTicker(expandTick)
	defer t.Stop()
	for {
		if e.Refresh(ctx, false) {
			onChange()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func signature(list []core.StrictRuleSet) string {
	parts := make([]string, 0, len(list))
	for _, r := range list {
		parts = append(parts, r.Name+"|"+r.Target+"|"+r.URL+"|"+r.Behavior+"|"+r.Format)
	}
	return strings.Join(parts, "\n")
}

// Refresh 按需重新展开，force 时忽略刷新时间。返回结果是否变化。
func (e *expander) Refresh(ctx context.Context, force bool) bool {
	list, err := e.sources(ctx)
	if err != nil {
		return false
	}
	sig := signature(list)
	e.mu.Lock()
	due := force || sig != e.sig || time.Now().After(e.next)
	prev := e.result
	e.mu.Unlock()
	if !due {
		return false
	}

	old := map[string]ExpandedRuleSet{}
	for _, r := range prev.RuleSets {
		old[r.Name+"|"+r.URL] = r
	}
	proxy := e.proxy(ctx)
	next := StrictExpansion{RuleSets: []ExpandedRuleSet{}}
	failed := false
	total := 0
	for _, src := range list {
		r := ExpandedRuleSet{Name: src.Name, Group: src.Target, URL: src.URL}
		body, err := e.fetch(ctx, src.URL, proxy)
		var lines []string
		if err == nil {
			lines, err = gen.ParseRuleSet(body, src.Format)
		}
		if err != nil {
			// 保留上一次成功的结果，免得一次下载失败就把这些域名移出保护范围。
			if o, ok := old[src.Name+"|"+src.URL]; ok {
				r = o
			}
			r.Error, failed = err.Error(), true
		} else {
			x := gen.ExpandRuleSet(lines, src.Behavior)
			r.Suffixes, r.CIDRs, r.Skipped, r.Samples = x.Suffixes, x.CIDRs, x.Skipped, x.Samples
			r.UpdatedAt = time.Now().UTC()
		}
		if r.Suffixes == nil {
			r.Suffixes = []string{}
		}
		if r.CIDRs == nil {
			r.CIDRs = []string{}
		}
		total += len(r.Suffixes) + len(r.CIDRs)
		next.RuleSets = append(next.RuleSets, r)
	}
	next.Truncated = total > expandMaxItems

	changed := !sameEntries(prev, next)
	e.mu.Lock()
	e.result, e.sig = next, sig
	e.next = time.Now().Add(expandRefresh)
	if failed {
		e.next = time.Now().Add(expandRetry)
	}
	e.mu.Unlock()
	if b, err := json.MarshalIndent(next, "", "  "); err == nil {
		tmp := e.path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, e.path)
		}
	}
	return changed
}

func sameEntries(a, b StrictExpansion) bool {
	if len(a.RuleSets) != len(b.RuleSets) {
		return false
	}
	for i := range a.RuleSets {
		x, y := a.RuleSets[i], b.RuleSets[i]
		if x.Name != y.Name || !slices.Equal(x.Suffixes, y.Suffixes) || !slices.Equal(x.CIDRs, y.CIDRs) {
			return false
		}
	}
	return true
}

func fetchRuleSet(ctx context.Context, rawURL, proxy string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, expandTimeout)
	defer cancel()
	tr := &http.Transport{Proxy: nil}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, expandMaxBody))
}
