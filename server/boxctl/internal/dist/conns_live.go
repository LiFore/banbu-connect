package dist

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// ---- 实时代理连接查看（BXC-F0-030）----
//
// 管理页面打开某台客户端的实时连接时，服务端记下「想看」（1 分钟内有效，页面每几秒刷新一次就一直有效）；
// 客户端每 15 秒问一次（或在每分钟的上报结果里得知），想看的期间每 3 秒推送当前经代理组的连接。
// 推送的连接只放在内存里，不入库。

const maxLiveConns = 300

type liveState struct {
	wantedUntil time.Time
	at          time.Time
	conns       []ConnRecord
}

// liveWanted 表示管理页面正在看这台客户端的实时连接。
func (s *Server) liveWanted(deviceID int64) bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	l := s.live[deviceID]
	return l != nil && time.Now().Before(l.wantedUntil)
}

// LiveView 是管理页面看到的实时连接。At 为空表示客户端还没推送过（可能要等十几秒）。
type LiveView struct {
	At    *time.Time   `json:"at,omitempty"`
	Conns []ConnRecord `json:"conns"`
}

// WatchLive 记下管理页面想看这台客户端的实时连接，返回最近一次推送。
func (s *Server) WatchLive(deviceID int64) LiveView {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live == nil {
		s.live = map[int64]*liveState{}
	}
	l := s.live[deviceID]
	if l == nil {
		l = &liveState{}
		s.live[deviceID] = l
	}
	l.wantedUntil = time.Now().Add(connLiveRequested)
	v := LiveView{Conns: []ConnRecord{}}
	// 超过 15 秒没有新推送的旧数据不显示，避免把断开的连接当成还在。
	if !l.at.IsZero() && time.Since(l.at) < 15*time.Second {
		t := l.at
		v.At, v.Conns = &t, l.conns
	}
	return v
}

// PushLive 保存客户端推送的当前连接。
func (s *Server) PushLive(deviceID int64, conns []ConnRecord) {
	if len(conns) > maxLiveConns {
		sort.Slice(conns, func(i, j int) bool { return conns[i].Start.After(conns[j].Start) })
		conns = conns[:maxLiveConns]
	}
	for i := range conns {
		conns[i].Host, conns[i].Destination, conns[i].Process = clipConn(conns[i].Host), clipConn(conns[i].Destination), clipConn(conns[i].Process)
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live == nil {
		s.live = map[int64]*liveState{}
	}
	l := s.live[deviceID]
	if l == nil {
		l = &liveState{}
		s.live[deviceID] = l
	}
	l.at, l.conns = time.Now(), conns
}

func (s *Server) connLiveClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler) {
	mux.Handle("GET /api/v1/client/conns/live", device(func(w http.ResponseWriter, _ *http.Request, d Device) {
		writeJSON(w, http.StatusOK, map[string]bool{"live": s.liveWanted(d.ID)})
	}))
	mux.Handle("POST /api/v1/client/conns/live", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in struct {
			Conns []ConnRecord `json:"conns"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{"bad_request", "请求内容无效"})
			return
		}
		s.PushLive(d.ID, in.Conns)
		writeJSON(w, http.StatusOK, map[string]bool{"live": s.liveWanted(d.ID)})
	}))
}

// ---- 客户端 ----

// ConnLiveWanted 问管理服务是否有人在看本机的实时连接。
func (c *Client) ConnLiveWanted(ctx context.Context) (bool, error) {
	var out struct {
		Live bool `json:"live"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/client/conns/live", nil, &out, requestTimeout)
	return out.Live, err
}

// PushConnLive 推送当前经代理组的连接，返回是否还要继续推送。
func (c *Client) PushConnLive(ctx context.Context, conns []ConnRecord) (bool, error) {
	var out struct {
		Live bool `json:"live"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/client/conns/live", map[string]any{"conns": conns}, &out, requestTimeout)
	return out.Live, err
}
