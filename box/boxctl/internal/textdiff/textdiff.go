// Package textdiff 生成按行的统一格式差异，用于配置版本对比。
package textdiff

import (
	"fmt"
	"strings"
)

// maxCells 限制 LCS 表的大小：去掉首尾相同的行后，中间部分超过它就整段按替换处理。
const maxCells = 4_000_000

type op struct {
	kind byte // ' ' 相同，'-' 删除，'+' 新增
	text string
}

// Unified 返回 a → b 的差异，每个改动块带 context 行上下文；没有差异时返回空串。
func Unified(a, b string, context int) string {
	al, bl := lines(a), lines(b)
	ops := diff(al, bl)
	var out strings.Builder
	// 找出改动所在的下标，按上下文合并成块。
	n := len(ops)
	changed := make([]bool, n)
	any := false
	for i, o := range ops {
		if o.kind != ' ' {
			changed[i], any = true, true
		}
	}
	if !any {
		return ""
	}
	ai, bi := 1, 1 // 行号从 1 开始
	type pos struct{ a, b int }
	at := make([]pos, n)
	for i, o := range ops {
		at[i] = pos{ai, bi}
		switch o.kind {
		case ' ':
			ai++
			bi++
		case '-':
			ai++
		case '+':
			bi++
		}
	}
	for i := 0; i < n; {
		if !changed[i] {
			i++
			continue
		}
		start := max(i-context, 0)
		end := i
		for j := i; j < n; j++ {
			if changed[j] {
				end = j
			} else if j-end > 2*context {
				break
			}
		}
		stop := min(end+context+1, n)
		var na, nb int
		for _, o := range ops[start:stop] {
			if o.kind != '+' {
				na++
			}
			if o.kind != '-' {
				nb++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", at[start].a, na, at[start].b, nb)
		for _, o := range ops[start:stop] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		i = stop
	}
	return out.String()
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func diff(a, b []string) []op {
	// 去掉首尾相同的部分，配置改动通常集中在几处。
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out []op
	for _, l := range a[:pre] {
		out = append(out, op{' ', l})
	}
	out = append(out, middle(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		out = append(out, op{' ', l})
	}
	return out
}

func middle(a, b []string) []op {
	var out []op
	if len(a)*len(b) > maxCells || len(a) == 0 || len(b) == 0 {
		for _, l := range a {
			out = append(out, op{'-', l})
		}
		for _, l := range b {
			out = append(out, op{'+', l})
		}
		return out
	}
	// lcs[i][j] 是 a[i:] 与 b[j:] 的最长公共子序列长度。
	w := len(b) + 1
	lcs := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, op{' ', a[i]})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			out = append(out, op{'-', a[i]})
			i++
		default:
			out = append(out, op{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, op{'-', a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, op{'+', b[j]})
	}
	return out
}
