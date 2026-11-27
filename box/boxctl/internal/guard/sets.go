package guard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// 严格集合里 dnsmasq 动态写入的地址只在内存里，重启路由盒就没了。开机后内核起来之前，
// 局域网设备还缓存着受保护域名的解析结果，这些地址不在集合里就会被直连放行（开机窗口）。
// guard 定期和退出时把集合存到数据目录，启动装好规则后立即写回。

const (
	setsFile = "guard-sets.json"
	// saveEvery 是每多少次自检保存一次集合（约 10 分钟）；内容没变时不写，减少闪存写入。
	saveEvery = 120
	// maxSaved 是每个集合最多保存的地址数。
	maxSaved = 8192
)

var dynamicSets = []string{"strict_v4", "strict_v6"}

// SetElem 是动态集合里的一个地址和剩余有效期（秒）。
type SetElem struct {
	Addr string `json:"addr"`
	TTL  int    `json:"ttl"`
}

// setStore 是能读出动态集合的系统（真实系统；FakeSystem 不实现）。
type setStore interface {
	ReadSet(ctx context.Context, name string) ([]SetElem, error)
}

// watcher 是能订阅 nft 变化的系统：guard 的表、链被删时立即自检，不等下一个 5 秒。
type watcher interface {
	Watch(ctx context.Context, onChange func())
}

func (execSystem) ReadSet(ctx context.Context, name string) ([]SetElem, error) {
	out, err := run(ctx, "", "nft", "-j", "list", "set", "inet", Table, name)
	if err != nil {
		return nil, err
	}
	return parseSetJSON([]byte(out))
}

// parseSetJSON 解析 `nft -j list set` 的输出。带超时的元素是 {"elem":{"val":..., "expires":...}}，否则是字符串。
func parseSetJSON(b []byte) ([]SetElem, error) {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []SetElem
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		for _, raw := range item.Set.Elem {
			var plain string
			if json.Unmarshal(raw, &plain) == nil {
				out = append(out, SetElem{Addr: plain})
				continue
			}
			var e struct {
				Elem struct {
					Val     string `json:"val"`
					Expires int    `json:"expires"`
				} `json:"elem"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Elem.Val != "" {
				out = append(out, SetElem{Addr: e.Elem.Val, TTL: e.Elem.Expires})
			}
		}
	}
	return out, nil
}

// RestoreScript 生成把保存的地址写回集合的 nft 脚本。地址不合法或族不对的跳过；有效期按保存时的剩余时间，
// 不扣除关机的时长（路由器开机时钟可能不准），最长不超过集合的超时。
func RestoreScript(sets map[string][]SetElem) string {
	var b strings.Builder
	limit := int(setTimeoutDur / time.Second)
	for _, name := range dynamicSets {
		v6 := strings.HasSuffix(name, "v6")
		var parts []string
		for _, e := range sets[name] {
			a, err := netip.ParseAddr(e.Addr)
			if err != nil || a.Is6() != v6 || a.Is4In6() {
				continue
			}
			ttl := e.TTL
			if ttl <= 0 || ttl > limit {
				ttl = limit
			}
			parts = append(parts, fmt.Sprintf("%s timeout %ds", a, ttl))
			if len(parts) >= maxSaved {
				break
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, name, strings.Join(parts, ", "))
		}
	}
	return b.String()
}

func loadSets(path string) map[string][]SetElem {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string][]SetElem
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// saveSets 读出动态集合存盘；内容（地址列表）没变时不写。返回保存的地址数。
func (e *Engine) saveSets(ctx context.Context) {
	st, ok := e.sys.(setStore)
	if !ok {
		return
	}
	m := map[string][]SetElem{}
	for _, name := range dynamicSets {
		list, err := st.ReadSet(ctx, name)
		if err != nil {
			return // 表不在（正在重装）：保留上一份
		}
		slices.SortFunc(list, func(a, b SetElem) int { return strings.Compare(a.Addr, b.Addr) })
		if len(list) > maxSaved {
			list = list[:maxSaved]
		}
		m[name] = list
	}
	key := setsKey(m)
	e.mu.Lock()
	same := key == e.savedSets
	e.mu.Unlock()
	if same {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	path := filepath.Join(e.dataDir, setsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		e.log.Warn("保存严格集合失败", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		e.log.Warn("保存严格集合失败", "err", err)
		return
	}
	e.mu.Lock()
	e.savedSets, e.savedN, e.savedAt = key, len(m["strict_v4"])+len(m["strict_v6"]), time.Now().UTC()
	e.mu.Unlock()
}

func setsKey(m map[string][]SetElem) string {
	var b strings.Builder
	for _, name := range dynamicSets {
		b.WriteString(name + ":")
		for _, e := range m[name] {
			b.WriteString(e.Addr + ",")
		}
	}
	return b.String()
}

// restoreSets 把上次保存的地址写回集合（规则装好之后调用）。
func (e *Engine) restoreSets(ctx context.Context) {
	m := loadSets(filepath.Join(e.dataDir, setsFile))
	script := RestoreScript(m)
	if script == "" {
		return
	}
	if err := e.sys.Apply(ctx, script); err != nil {
		e.record("warn", "恢复上次的严格集合失败："+err.Error())
		return
	}
	n := strings.Count(script, " timeout ")
	e.mu.Lock()
	e.savedSets, e.savedN, e.savedAt = setsKey(m), n, time.Now().UTC()
	e.mu.Unlock()
	e.record("info", fmt.Sprintf("已恢复上次的严格集合（%d 个地址）", n))
}

// Watch 跑 `nft monitor`，guard 的表、链、规则被删或清空时回调（防抖 300 毫秒）。
// nft monitor 退出时 5 秒后重试；ctx 结束时返回。
func (execSystem) Watch(ctx context.Context, onChange func()) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "nft", "monitor")
		out, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			fire := debounce(300*time.Millisecond, onChange)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if monitorHit(sc.Text()) {
					fire()
				}
			}
			_ = cmd.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// monitorHit 判断 nft monitor 的一行是不是 guard 的表、链或规则被删。元素超时、dnsmasq 写入不算。
func monitorHit(line string) bool {
	if !strings.HasPrefix(line, "delete ") && !strings.HasPrefix(line, "flush ") && !strings.HasPrefix(line, "destroy ") {
		return false
	}
	if strings.HasPrefix(line, "delete element") || strings.HasPrefix(line, "destroy element") {
		return false
	}
	return strings.Contains(line, "inet "+Table+" ") || strings.HasSuffix(line, "inet "+Table) || strings.Contains(line, "ruleset")
}

func debounce(d time.Duration, fn func()) func() {
	var t *time.Timer
	return func() {
		if t != nil {
			t.Stop()
		}
		t = time.AfterFunc(d, fn)
	}
}
