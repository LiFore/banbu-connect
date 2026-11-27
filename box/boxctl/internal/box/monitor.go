package box

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"boxctl/internal/core"
	"boxctl/internal/gen"
)

// 运行观测（M11-02、设备流量统计、规则命中统计）：每秒从内核取一次连接快照，
// 算出实时速率，并按来源 IP 和规则累计——连接关闭后内核就不再报告它，累计值由这里维护。
// 只统计经过内核的流量：绕过内核的设备不在其中。累计值在内存里，面板重启后清零。

const (
	monitorPeriod  = time.Second
	liveMaxConns   = 500 // 推送给页面的连接数上限，按开始时间取最新的
	maxRuleEntries = 500
)

// Conn 是一条连接（与桌面端共用，见 core.Conn）。
type Conn = core.Conn

// DeviceUsage 是一个来源 IP 经内核的用量。
type DeviceUsage struct {
	IP          string `json:"ip"`
	Connections int    `json:"connections"` // 当前连接数
	Entry       int    `json:"entry"`       // 其中经客户端代理入口（gen.ClientListener）进来的
	Upload      int64  `json:"upload"`      // 累计（含已关闭的连接），面板启动以来
	Download    int64  `json:"download"`
}

// RuleHit 是一条规则的命中次数（新连接数），面板启动以来。
type RuleHit struct {
	Rule    string `json:"rule"`
	Payload string `json:"payload"`
	Target  string `json:"target"` // 最近一次命中时的代理组
	Hits    int64  `json:"hits"`
}

// Live 是推送给页面的一帧。
type Live struct {
	Time          time.Time     `json:"time"`
	Running       bool          `json:"running"`
	Up            int64         `json:"up"` // 字节/秒
	Down          int64         `json:"down"`
	UploadTotal   int64         `json:"upload_total"`
	DownloadTotal int64         `json:"download_total"`
	Connections   []Conn        `json:"connections"`
	Total         int           `json:"total"` // 当前连接总数（Connections 可能被截断）
	Devices       []DeviceUsage `json:"devices"`
}

var connIDRE = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)

type connSeen struct{ up, down int64 }

type monitor struct {
	sup   *core.Supervisor
	stats *core.ConnStats // 代理连接统计（S1.8），和页面共用同一次采样

	mu      sync.Mutex
	live    Live
	seen    map[string]connSeen // 连接 ID → 上一次看到的累计值
	devices map[string]*DeviceUsage
	rules   map[string]*RuleHit
	notify  chan struct{} // 每采样一次关闭并换新，WebSocket 据此推送
}

func newMonitor(sup *core.Supervisor) *monitor {
	return &monitor{sup: sup, seen: map[string]connSeen{}, devices: map[string]*DeviceUsage{}, rules: map[string]*RuleHit{},
		notify: make(chan struct{}), live: Live{Connections: []Conn{}, Devices: []DeviceUsage{}}}
}

func (m *monitor) Run(ctx context.Context) {
	t := time.NewTicker(monitorPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sample(ctx)
		}
	}
}

type rawConns = core.RawConnections

func (m *monitor) sample(ctx context.Context) {
	now := time.Now()
	raw, running := m.sup.FetchConnections(ctx)
	m.ingest(now, running, raw)
	if m.stats != nil {
		m.stats.Observe(raw.Snapshot(now, running))
	}
}

// ingest 处理一次快照，拆出来便于测试。
func (m *monitor) ingest(now time.Time, running bool, raw rawConns) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.live
	live := Live{Time: now, Running: running, UploadTotal: raw.UploadTotal, DownloadTotal: raw.DownloadTotal, Connections: []Conn{}}
	if running && prev.Running && !prev.Time.IsZero() {
		if dt := now.Sub(prev.Time).Seconds(); dt > 0 {
			live.Up = max(int64(float64(raw.UploadTotal-prev.UploadTotal)/dt), 0)
			live.Down = max(int64(float64(raw.DownloadTotal-prev.DownloadTotal)/dt), 0)
		}
	}
	current := map[string]int{}
	entry := map[string]int{}
	present := map[string]bool{}
	for _, c := range raw.Conns() {
		live.Connections = append(live.Connections, c)
		present[c.ID] = true
		src := c.Source
		if src != "" {
			current[src]++
			if c.Inbound == gen.ClientListener {
				entry[src]++
			}
		}
		old, known := m.seen[c.ID]
		if src != "" {
			d := m.device(src)
			d.Upload += max(c.Upload-old.up, 0)
			d.Download += max(c.Download-old.down, 0)
		}
		key := c.Rule + "\x00" + c.RulePayload
		if !known && c.Rule != "" {
			h := m.rules[key]
			if h == nil && len(m.rules) < maxRuleEntries {
				h = &RuleHit{Rule: c.Rule, Payload: c.RulePayload}
				m.rules[key] = h
			}
			if h != nil {
				h.Hits++
				if n := len(c.Chains); n > 0 {
					h.Target = c.Chains[n-1]
				}
			}
		}
		m.seen[c.ID] = connSeen{up: c.Upload, down: c.Download}
	}
	for id := range m.seen {
		if !present[id] {
			delete(m.seen, id) // 连接已关闭：最后一次看到的累计值已计入
		}
	}
	live.Total = len(live.Connections)
	sort.Slice(live.Connections, func(i, j int) bool { return live.Connections[i].Start.After(live.Connections[j].Start) })
	if len(live.Connections) > liveMaxConns {
		live.Connections = live.Connections[:liveMaxConns]
	}
	live.Devices = make([]DeviceUsage, 0, len(m.devices))
	for ip, d := range m.devices {
		u := *d
		u.Connections = current[ip]
		u.Entry = entry[ip]
		live.Devices = append(live.Devices, u)
	}
	sort.Slice(live.Devices, func(i, j int) bool {
		a, b := live.Devices[i], live.Devices[j]
		return a.Upload+a.Download > b.Upload+b.Download
	})
	m.live = live
	close(m.notify)
	m.notify = make(chan struct{})
}

func (m *monitor) device(ip string) *DeviceUsage {
	d := m.devices[ip]
	if d == nil {
		d = &DeviceUsage{IP: ip}
		m.devices[ip] = d
	}
	return d
}

// Live 返回最近一帧和下一帧的通知。
func (m *monitor) Live() (Live, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live, m.notify
}

// Devices 返回按来源 IP 的用量。
func (m *monitor) Devices() map[string]DeviceUsage {
	live, _ := m.Live()
	out := map[string]DeviceUsage{}
	for _, d := range live.Devices {
		out[d.IP] = d
	}
	return out
}

// RuleHits 返回规则命中统计，按次数降序。
func (m *monitor) RuleHits() []RuleHit {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RuleHit, 0, len(m.rules))
	for _, h := range m.rules {
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b RuleHit) int {
		if a.Hits != b.Hits {
			return int(b.Hits - a.Hits)
		}
		return strings.Compare(a.Rule+a.Payload, b.Rule+b.Payload)
	})
	return out
}

func (b *Box) monitorRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
	// 实时流量与连接（M11-02）：WebSocket 每秒推送一帧。
	mux.Handle("GET /api/v1/box/live", authed(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil) // 默认只接受同源
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := c.CloseRead(r.Context()) // 不读客户端消息；客户端关闭时 ctx 结束
		for {
			live, next := b.monitor.Live()
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(wctx, c, live)
			cancel()
			if err != nil {
				return
			}
			select {
			case <-ctx.Done():
				c.Close(websocket.StatusNormalClosure, "")
				return
			case <-next:
			}
		}
	}))
	mux.Handle("GET /api/v1/box/rule-hits", authed(func(w http.ResponseWriter, r *http.Request) {
		core.WriteJSON(w, http.StatusOK, b.monitor.RuleHits())
	}))
	mux.Handle("DELETE /api/v1/box/connections/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		if !connIDRE.MatchString(r.PathValue("id")) {
			core.WriteJSON(w, http.StatusBadRequest, core.APIErr{Code: "bad_request", Message: "连接 ID 无效"})
			return
		}
		code, body, err := b.sup.CallCore(r.Context(), http.MethodDelete, "/connections/"+r.PathValue("id"), nil)
		if err != nil {
			core.Fail(w, err)
			return
		}
		if code >= 300 {
			core.WriteJSON(w, http.StatusBadGateway, core.APIErr{Code: "core", Message: strings.TrimSpace(string(body))})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
