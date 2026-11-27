package dist

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"boxctl/internal/otel"
)

// telemetryAdminRoutes：管理服务「遥测」页（BXC-F0-050～053、BXC-F0-028、BXC-F0-030）。
func (s *Server) telemetryAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	rng := func(r *http.Request) TelemetryRange { return ParseRange(r.URL.Query().Get("range"), time.Now()) }
	filter := func(r *http.Request) TelemetryFilter {
		q := r.URL.Query()
		return TelemetryFilter{Client: q.Get("client"), Host: q.Get("host"), Rule: q.Get("rule"), Node: q.Get("node"),
			Group: q.Get("group"), Hop: q.Get("hop")}
	}

	mux.Handle("GET /api/v1/dist/telemetry/overview", authed(func(w http.ResponseWriter, r *http.Request) {
		o, err := s.TelemetryOverview(r.Context(), rng(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, o)
	}))
	mux.Handle("GET /api/v1/dist/telemetry/topology", authed(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 50 {
			limit = 12
		}
		t, err := s.Topology(r.Context(), rng(r), filter(r), limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}))
	mux.Handle("GET /api/v1/dist/telemetry/detail", authed(func(w http.ResponseWriter, r *http.Request) {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		d, err := s.TelemetryDetail(r.Context(), rng(r), filter(r), before)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
	// 流量用量（长期保留）：days 天内每小时或每天，按客户端（路由盒细到设备）。
	mux.Handle("GET /api/v1/dist/telemetry/usage", authed(func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 || days > 366 {
			days = 30
		}
		step := r.URL.Query().Get("step")
		if step != "hour" {
			step = "day"
		}
		now := time.Now()
		u, err := s.Usage(r.Context(), now.AddDate(0, 0, -days), now, step)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, u)
	}))
	// Claude Code 用量总览：所有客户端合计（按模型、按天、最近会话）和按客户端的汇总。
	mux.Handle("GET /api/v1/dist/telemetry/claude", authed(func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 || days > otelKeepDays {
			days = 7
		}
		out := struct {
			Summary otel.Summary       `json:"summary"`
			Devices []otel.DeviceUsage `json:"devices"`
		}{Devices: []otel.DeviceUsage{}}
		if s.OTel != nil {
			var err error
			if out.Summary, err = s.OTel.Summarize(r.Context(), otel.AllDevices, days); err != nil {
				fail(w, err)
				return
			}
			if out.Devices, err = s.OTel.Devices(r.Context(), days); err != nil {
				fail(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, out)
	}))
	// 实时连接：页面每几秒请求一次，客户端在这期间推送。
	mux.Handle("GET /api/v1/dist/devices/{id}/live", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			fail(w, ErrNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, s.WatchLive(id))
	}))
}
