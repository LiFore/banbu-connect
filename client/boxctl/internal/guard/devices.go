package guard

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// 设备断网与时段管控（ADR-036）：按局域网设备的 IP，在 prerouting（raw 优先级）丢弃它去外网的流量。
// 放在 prerouting 而不是 forward：被内核 auto-redirect 接管的 TCP 进的是本机，不经过 forward。
// 去路由盒自身（DNS、DHCP、面板）和私有网段（局域网互访）的不受影响。
// 规则常驻在 guard 的表里，不依赖内核和面板存活。

const (
	// MaxBlocks 是能管控的设备数上限：每台设备一条链，链名固定编号，便于原子地清理旧链。
	MaxBlocks = 64
)

// Block 是一台设备的管控：Windows 为空表示一直断网，否则只在这些时段放行。
type Block struct {
	IP      string   `json:"ip"`
	Windows []Window `json:"windows,omitempty"`
}

// Window 是一个放行时段。Days 为 1–7（周一到周日），空表示每天；Start、End 为 HH:MM，End 早于 Start 表示跨午夜。
type Window struct {
	Days  []int  `json:"days,omitempty"`
	Start string `json:"start"`
	End   string `json:"end"`
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

var dayNames = []string{"", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// privateNets 是局域网互访和组播，设备断网时仍然放行。
const privateNets = "10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10, 224.0.0.0/4, 255.255.255.255"

func normalizeBlocks(in []Block) ([]Block, error) {
	if len(in) > MaxBlocks {
		return nil, fmt.Errorf("最多管控 %d 台设备", MaxBlocks)
	}
	out := []Block{}
	seen := map[string]bool{}
	for _, b := range in {
		a, err := netip.ParseAddr(strings.TrimSpace(b.IP))
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("设备地址须是 IPv4：%q", b.IP)
		}
		if seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		nb := Block{IP: a.String()}
		for _, w := range b.Windows {
			if !hhmm.MatchString(w.Start) || !hhmm.MatchString(w.End) || w.Start == w.End {
				return nil, fmt.Errorf("设备 %s 的时段无效：%s–%s", a, w.Start, w.End)
			}
			days := []int{}
			for _, d := range w.Days {
				if d < 1 || d > 7 {
					return nil, fmt.Errorf("设备 %s 的星期无效：%d", a, d)
				}
				days = append(days, d)
			}
			slices.Sort(days)
			days = slices.Compact(days)
			if len(days) == 7 {
				days = []int{}
			}
			nb.Windows = append(nb.Windows, Window{Days: days, Start: w.Start, End: w.End})
		}
		out = append(out, nb)
	}
	slices.SortFunc(out, func(a, b Block) int { return netip.MustParseAddr(a.IP).Compare(netip.MustParseAddr(b.IP)) })
	return out, nil
}

// windowRules 把放行时段换成 nft 的匹配条件；跨午夜的拆成两段，后一段落在次日。
func windowRules(w Window) []string {
	days := w.Days
	match := func(ds []int, from, to string) string {
		s := ""
		if len(ds) > 0 {
			names := make([]string, len(ds))
			for i, d := range ds {
				names[i] = `"` + dayNames[d] + `"`
			}
			s = "meta day { " + strings.Join(names, ", ") + " } "
		}
		return s + fmt.Sprintf(`meta hour "%s"-"%s"`, from, to)
	}
	if w.Start < w.End {
		return []string{match(days, w.Start, w.End)}
	}
	next := make([]int, len(days))
	for i, d := range days {
		next[i] = d%7 + 1
	}
	slices.Sort(next)
	return []string{match(days, w.Start, "23:59:59"), match(next, "00:00", w.End)}
}

// renderBlocks 写设备管控的规则。lanDev 为空时不限定入口网卡（开发环境）。
func renderBlocks(w func(string, ...any), t, lanDev string, blocks []Block) {
	w("add chain %s device_block { type filter hook prerouting priority raw - 1; policy accept; }", t)
	w("flush chain %s device_block", t)
	// 清理上一份策略留下的设备链：先建（已存在则无操作）再删，在同一个事务里。
	for i := len(blocks); i < MaxBlocks; i++ {
		w("add chain %s dev_%d", t, i)
		w("delete chain %s dev_%d", t, i)
	}
	in := iifMatch(lanDev)
	for i, b := range blocks {
		c := fmt.Sprintf("dev_%d", i)
		w("add chain %s %s", t, c)
		w("flush chain %s %s", t, c)
		w(`add rule %s device_block %sip saddr %s jump %s`, t, in, b.IP, c)
		w("add rule %s %s fib daddr type local return", t, c)
		w("add rule %s %s ip daddr { %s } return", t, c, privateNets)
		for _, win := range b.Windows {
			for _, m := range windowRules(win) {
				w("add rule %s %s %s return", t, c, m)
			}
		}
		w(`add rule %s %s counter drop comment "device %s"`, t, c, b.IP)
	}
}
