package core

import (
	"encoding/json"
	"net/http"
)

// ConnStatsRoutes 挂「服务端」页的代理连接统计上报（BXC-F0-029）：状态与待上报内容、开关、立即上报。
// 开关保存在 Settings.ConnStats。
func ConnStatsRoutes(mux *http.ServeMux, authed Authed, prefix string, stats *ConnStats, sup *Supervisor) {
	base := "/api/v1/" + prefix + "/server/conn-stats"
	mux.Handle("GET "+base, authed(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, stats.Status())
	}))
	mux.Handle("PUT "+base, authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			WriteJSON(w, http.StatusBadRequest, APIErr{"bad_request", "请求内容无效"})
			return
		}
		store := sup.Settings()
		st, err := store.Load(r.Context())
		if err == nil {
			st.ConnStats = in.Enabled
			err = store.Save(r.Context(), st)
		}
		if err != nil {
			Fail(w, err)
			return
		}
		stats.SetEnabled(in.Enabled)
		WriteJSON(w, http.StatusOK, stats.Status())
	}))
	mux.Handle("POST "+base+"/upload", authed(func(w http.ResponseWriter, r *http.Request) {
		stats.Seal()
		if err := stats.Upload(r.Context()); err != nil {
			WriteJSON(w, http.StatusBadGateway, APIErr{"failed", err.Error()})
			return
		}
		WriteJSON(w, http.StatusOK, stats.Status())
	}))
}
