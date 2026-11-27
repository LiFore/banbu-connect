package dist

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// aiOnlineAdminRoutes：AI 账号在线时长的统计页和账号卡片。
func (s *Server) aiOnlineAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) { writeErr(w, err, log) }
	parse := func(v string, def time.Time) time.Time {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		return def
	}

	// 统计页：from / to 是 RFC 3339（页面按浏览器的本地日期算好），tz 是按天切分用的时区（IANA 名称，默认服务器时区）。
	mux.Handle("GET /api/v1/dist/ai-online", authed(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		now := time.Now()
		to := parse(q.Get("to"), now)
		from := parse(q.Get("from"), to.Add(-7*24*time.Hour))
		if !from.Before(to) || to.Sub(from) > aiOnlineKeep {
			fail(w, &InputError{"时间范围无效"})
			return
		}
		loc := time.Local
		if tz := q.Get("tz"); tz != "" {
			if l, err := time.LoadLocation(tz); err == nil {
				loc = l
			}
		}
		f := AIOnlineFilter{Kind: q.Get("kind")}
		f.DeviceID, _ = strconv.ParseInt(q.Get("device"), 10, 64)
		f.AccountID, _ = strconv.ParseInt(q.Get("account"), 10, 64)
		out, err := s.AIOnlineStatsFor(r.Context(), from, to, loc, f, now)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	// 账号卡片：today 是浏览器本地今天的零点（RFC 3339）。
	mux.Handle("GET /api/v1/dist/ai-online/summary", authed(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		today := parse(r.URL.Query().Get("today"), dayStart(now, time.Local))
		if today.After(now) || now.Sub(today) > 48*time.Hour {
			today = dayStart(now, time.Local)
		}
		out, err := s.AIOnlineSummaries(r.Context(), today, now)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
}

// aiOnlineClientRoutes：客户端上报此刻在用的 AI 账号。
func (s *Server) aiOnlineClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	mux.Handle("POST /api/v1/client/ai/online", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		var in struct {
			Accounts []AIOnlineAccount `json:"accounts"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.RecordAIOnline(r.Context(), d.ID, in.Accounts, time.Now()); err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}
