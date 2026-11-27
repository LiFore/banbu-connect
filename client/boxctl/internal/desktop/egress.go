package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"boxctl/internal/core"
	"boxctl/internal/killswitch"
	"boxctl/internal/trust"
)

// egressRoutes 挂出口检测：每个站点 × 每条路看到的出口（见 trust.Scan）。
func egressRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, s *core.Supervisor, ks *killswitch.Engine) {
	mux.Handle("POST /api/v1/desktop/egress/scan", authed(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, trust.Scan(ctx, scanSites(ks.Config()), scanPaths(s), nil))
	}))
	// 线路预览：指定域名命中的规则、出站链、出口和延迟（见 routetest.go）。
	mux.Handle("POST /api/v1/desktop/route/test", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, TestRoute(ctx, s, in.Target))
	}))
	// 出口检测里的 Google、GitHub：它们没有 Cloudflare trace，按规则实测。
	mux.Handle("POST /api/v1/desktop/egress/services", authed(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		targets := make([]string, len(ServiceTargets))
		for i, t := range ServiceTargets {
			targets[i] = t.Target
		}
		results := TestRoutes(ctx, s, targets)
		type item struct {
			Name string `json:"name"`
			RouteTest
		}
		out := make([]item, len(results))
		for i, r := range results {
			out[i] = item{Name: ServiceTargets[i].Name, RouteTest: r}
		}
		writeJSON(w, http.StatusOK, out)
	}))
}

// scanSites 是默认站点加上受保护服务自己的地址；已在默认列表里的只标记为受保护。
func scanSites(c killswitch.Config) []trust.ScanSite {
	sites := trust.DefaultScanSites()
	for group, u := range gateProbeURLs(c) {
		found := false
		for i := range sites {
			if sites[i].URL == u {
				sites[i].Protected, found = true, true
			}
		}
		if !found {
			sites = append(sites, trust.ScanSite{Name: group, URL: u, Protected: true})
		}
	}
	return sites
}

func scanPaths(s *core.Supervisor) []trust.ScanPath {
	const local = "和本机应用一样经系统路由：TUN 接管时按内核规则分流，否则直接从网卡出去"
	paths := []trust.ScanPath{
		trust.NewScanPath("local4", "本机 IPv4", local, nil, "tcp4"),
		trust.NewScanPath("local6", "本机 IPv6", local, nil, "tcp6"),
	}
	if d := s.GateDirectURL(); d != nil {
		paths = append(paths, trust.NewScanPath("direct", "内核直连", "闸门直连时受保护服务走的路：经物理网卡交给路由盒", d, ""))
	}
	for _, t := range s.StrictTargets() {
		u := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(t.Port)}
		paths = append(paths, trust.NewScanPath("group:"+t.Group, "代理组「"+t.Group+"」", "经这个严格组当前选中的线路", u, ""))
	}
	return paths
}
