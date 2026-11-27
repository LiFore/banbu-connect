// Package logring 把日志同时留一份在内存里（最近 N 条），供面板查看和诊断包导出（M11-03）。
// 写入前脱敏：订阅地址里的令牌、密码等字段不会进入内存。
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Entry 是一条日志。
type Entry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // DEBUG / INFO / WARN / ERROR
	Source  string    `json:"source"`
	Message string    `json:"message"`
}

// Ring 保存最近的日志。
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int
	full bool
}

func New(size int) *Ring { return &Ring{buf: make([]Entry, size)} }

func (r *Ring) Add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	r.full = r.full || r.next == 0
}

// Entries 按时间顺序返回级别不低于 min 的日志（min 为空表示全部）。
func (r *Ring) Entries(min string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []Entry
	if r.full {
		all = append(all, r.buf[r.next:]...)
	}
	all = append(all, r.buf[:r.next]...)
	out := []Entry{}
	floor := LevelRank(min)
	for _, e := range all {
		if LevelRank(e.Level) >= floor {
			out = append(out, e)
		}
	}
	return out
}

// LevelRank 把级别名换成可比较的数，未知的当作最低。
func LevelRank(l string) int {
	switch strings.ToUpper(l) {
	case "ERROR", "ERR", "FATAL":
		return 3
	case "WARN", "WARNING":
		return 2
	case "INFO":
		return 1
	}
	return 0
}

var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	// URL 查询参数里的令牌：订阅地址 ?token=xxx、&key=xxx
	{regexp.MustCompile(`(?i)([?&](?:token|key|secret|password|passwd|auth|sig|signature|access_token|uuid)=)[^&\s"']+`), "${1}***"},
	// URL 里的用户名密码：scheme://user:pass@host
	{regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/\s:@"']+:[^/\s@"']+@`), "${1}***:***@"},
	// key=value 或 key: value 形式的凭据
	{regexp.MustCompile(`(?i)\b(password|passwd|secret|token|uuid|private-key|psk|authorization)(["']?\s*[:=]\s*["']?)[^\s"',}&]+`), "${1}${2}***"},
	// Bearer 令牌
	{regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]+`), "${1}***"},
}

// Redact 脱敏一行日志。
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Handler 包装一个 slog.Handler：照常输出，同时把脱敏后的内容存进 Ring。
type Handler struct {
	next   slog.Handler
	ring   *Ring
	source string
	attrs  string
}

func NewHandler(next slog.Handler, ring *Ring, source string) *Handler {
	return &Handler{next: next, ring: ring, source: source}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.ring.Add(Entry{Time: r.Time.UTC(), Level: r.Level.String(), Source: h.source, Message: Redact(b.String())})
	return h.next.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		if a.Key == "role" {
			continue // 已经体现在 source 里
		}
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	c.attrs = b.String()
	return &c
}

func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	return &c
}
