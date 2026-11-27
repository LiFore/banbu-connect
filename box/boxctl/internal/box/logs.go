package box

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/guard"
	"boxctl/internal/ipc"
	"boxctl/internal/logring"
)

// 日志查看（M11-03）：panel、guard 各自在内存里留最近的日志（已脱敏）；内核日志读 core/core.log 的末尾。

const coreLogTail = 512 << 10

var coreLogRE = regexp.MustCompile(`^time="([^"]+)" level=(\w+) msg="(.*)"$`)

// coreLogs 读内核日志末尾，解析成条目。
func (b *Box) coreLogs(level string) []logring.Entry {
	f, err := os.Open(filepath.Join(b.opts.DataDir, "core", "core.log"))
	if err != nil {
		return []logring.Entry{}
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > coreLogTail {
		_, _ = f.Seek(-coreLogTail, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	lines := strings.Split(string(data), "\n")
	if len(data) >= coreLogTail && len(lines) > 0 {
		lines = lines[1:] // 第一行可能不完整
	}
	out := []logring.Entry{}
	floor := logring.LevelRank(level)
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		e := logring.Entry{Level: "INFO", Source: "core", Message: l}
		if m := coreLogRE.FindStringSubmatch(l); m != nil {
			e.Time, _ = time.Parse(time.RFC3339Nano, m[1])
			e.Level = strings.ToUpper(m[2])
			if e.Level == "WARNING" {
				e.Level = "WARN"
			}
			e.Message = strings.ReplaceAll(m[3], `\"`, `"`)
		}
		if logring.LevelRank(e.Level) < floor {
			continue
		}
		e.Message = logring.Redact(e.Message)
		out = append(out, e)
	}
	return out
}

func (b *Box) guardLogs(ctx context.Context, level string) ([]logring.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, ipcTimeout)
	defer cancel()
	var out []logring.Entry
	err := ipc.Call(ctx, b.opts.GuardSocket, guard.MethodLogs, map[string]string{"level": level}, &out)
	if out == nil {
		out = []logring.Entry{}
	}
	return out, err
}

func (b *Box) panelLogs(level string) []logring.Entry {
	if b.opts.Logs == nil {
		return []logring.Entry{}
	}
	return b.opts.Logs.Entries(level)
}

func (b *Box) logRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/v1/box/logs", authed(func(w http.ResponseWriter, r *http.Request) {
		level := strings.ToUpper(r.URL.Query().Get("level"))
		switch r.URL.Query().Get("source") {
		case "guard":
			list, err := b.guardLogs(r.Context(), level)
			if err != nil {
				core.WriteJSON(w, http.StatusBadGateway, core.APIErr{Code: "guard", Message: "读取 guard 日志失败：" + err.Error()})
				return
			}
			core.WriteJSON(w, http.StatusOK, list)
		case "core":
			core.WriteJSON(w, http.StatusOK, b.coreLogs(level))
		default:
			core.WriteJSON(w, http.StatusOK, b.panelLogs(level))
		}
	}))
}
