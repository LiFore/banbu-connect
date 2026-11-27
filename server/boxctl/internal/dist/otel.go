package dist

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"boxctl/internal/otel"
)

// Claude Code 遥测：客户端把本机收到的 OTel 数据（不含提示词和助手回复原文）分批上报，
// 管理员在「客户端」页面看每台客户端的 Claude Code 用量。数据存在管理服务数据目录的 otel.db。

const (
	maxOTelUpload = 32 << 20 // 解压后的大小上限
	otelKeepDays  = 90       // 服务端保留天数
)

// ---- 客户端 ----

// ReportOTel 上报一批遥测数据（gzip 压缩）。没有绑定时什么也不做，返回 false。
func (c *Client) ReportOTel(ctx context.Context, b otel.Batch) (bool, error) {
	c.mu.Lock()
	bind := c.binding
	c.mu.Unlock()
	if bind == nil {
		return false, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return true, err
	}
	if err := zw.Close(); err != nil {
		return true, err
	}
	resp, err := c.doMethod(ctx, *bind, http.MethodPost, "/api/v1/client/otel", &buf, func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
	})
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, responseError(resp)
	}
	return true, nil
}

// ---- 服务端 ----

func (s *Server) otelClientRoutes(mux *http.ServeMux, device func(func(http.ResponseWriter, *http.Request, Device)) http.Handler, log *slog.Logger) {
	mux.Handle("POST /api/v1/client/otel", device(func(w http.ResponseWriter, r *http.Request, d Device) {
		if s.OTel == nil {
			writeJSON(w, http.StatusNotFound, apiError{"not_found", "管理服务没有开启遥测接收"})
			return
		}
		var body io.Reader = http.MaxBytesReader(w, r.Body, maxOTelUpload)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, apiError{"bad_request", "请求内容无效"})
				return
			}
			defer zr.Close()
			body = io.LimitReader(zr, maxOTelUpload)
		}
		var in otel.Batch
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{"bad_request", "请求内容无效"})
			return
		}
		if _, err := s.OTel.Import(r.Context(), d.ID, in); err != nil {
			if errors.Is(err, otel.ErrInvalidBatch) {
				writeJSON(w, http.StatusBadRequest, apiError{"invalid", err.Error()})
				return
			}
			writeErr(w, err, log)
			return
		}
		s.pruneOTel(r.Context(), log)
		_ = s.Touch(r.Context(), d.ID, remoteIP(r), clientInfo(r), 0)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

// pruneOTel 每小时最多清理一次过期数据。
func (s *Server) pruneOTel(ctx context.Context, log *slog.Logger) {
	now := time.Now().Unix()
	last := s.otelPruned.Load()
	if now-last < 3600 || !s.otelPruned.CompareAndSwap(last, now) {
		return
	}
	if _, err := s.OTel.Prune(ctx, otelKeepDays); err != nil && log != nil {
		log.Warn("清理过期遥测失败", "err", err)
	}
}

func (s *Server) otelAdminRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, log *slog.Logger) {
	days := func(r *http.Request) int {
		d, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if d <= 0 || d > otelKeepDays {
			d = 7
		}
		return d
	}
	mux.Handle("GET /api/v1/dist/otel/devices", authed(func(w http.ResponseWriter, r *http.Request) {
		if s.OTel == nil {
			writeJSON(w, http.StatusOK, []otel.DeviceUsage{})
			return
		}
		list, err := s.OTel.Devices(r.Context(), days(r))
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("GET /api/v1/dist/devices/{id}/otel", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 || s.OTel == nil {
			writeErr(w, ErrNotFound, log)
			return
		}
		sum, err := s.OTel.Summarize(r.Context(), id, days(r))
		if err != nil {
			writeErr(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, sum)
	}))
}
