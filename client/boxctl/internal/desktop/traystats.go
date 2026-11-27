package desktop

import (
	"context"
	"net/http"
	"sync"
	"time"

	"boxctl/internal/core"
)

// 托盘面板（client/app/ui/tray.html）的实时数据：上下行速率、本次运行以来的流量和连接数。
// 面板打开时外壳每秒读一次；速率取与上一次读取之间的差值，两次读取间隔太久（面板刚打开）时为 0。

// TrayStats 是 GET /api/v1/desktop/tray-stats 的响应。
type TrayStats struct {
	Running       bool  `json:"running"`
	Up            int64 `json:"up"`   // 字节/秒
	Down          int64 `json:"down"` // 字节/秒
	UploadTotal   int64 `json:"upload_total"`
	DownloadTotal int64 `json:"download_total"`
	Connections   int   `json:"connections"`
	Proxied       int   `json:"proxied"` // 经代理出去的连接
}

const trayRateWindow = 10 * time.Second

type trayStats struct {
	mu   sync.Mutex
	last core.ConnSnapshot
}

// rate 用两次快照算每秒字节数；内核重启后累计值回退时按 0 算。
func rate(prev, cur int64, d time.Duration) int64 {
	if cur < prev || d <= 0 {
		return 0
	}
	return int64(float64(cur-prev) / d.Seconds())
}

func (t *trayStats) next(snap core.ConnSnapshot) TrayStats {
	t.mu.Lock()
	prev := t.last
	t.last = snap
	t.mu.Unlock()
	out := TrayStats{Running: snap.Running, UploadTotal: snap.UploadTotal, DownloadTotal: snap.DownloadTotal, Connections: len(snap.Conns)}
	for _, c := range snap.Conns {
		if c.IsProxied() {
			out.Proxied++
		}
	}
	if d := snap.Time.Sub(prev.Time); snap.Running && prev.Running && d > 0 && d <= trayRateWindow {
		out.Up = rate(prev.UploadTotal, snap.UploadTotal, d)
		out.Down = rate(prev.DownloadTotal, snap.DownloadTotal, d)
	}
	return out
}

func (t *trayStats) routes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, s *core.Supervisor) {
	mux.Handle("GET /api/v1/desktop/tray-stats", authed(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		raw, ok := s.FetchConnections(ctx)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, t.next(raw.Snapshot(time.Now(), ok)))
	}))
}
