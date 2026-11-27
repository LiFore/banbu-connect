package panel

import (
	"errors"
	"net/http"

	"boxctl/internal/sub"
	"boxctl/internal/subscriptions"
)

func (h *handler) routeSubscriptions(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/subscriptions", h.authed(h.listSubscriptions, 0))
	mux.Handle("GET /api/v1/subscriptions/defaults", h.authed(h.subscriptionDefaults, 0))
	mux.Handle("POST /api/v1/subscriptions", h.authed(h.createSubscription, 0))
	mux.Handle("GET /api/v1/subscriptions/{id}", h.authed(h.getSubscription, 0))
	mux.Handle("PUT /api/v1/subscriptions/{id}", h.authed(h.updateSubscription, 0))
	mux.Handle("DELETE /api/v1/subscriptions/{id}", h.authed(h.deleteSubscription, 0))
	mux.Handle("POST /api/v1/subscriptions/{id}/preview", h.authed(h.previewSubscription, 0))
	mux.Handle("POST /api/v1/subscriptions/{id}/apply", h.authed(h.applySubscription, 0))
}

func (h *handler) listSubscriptions(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	list, err := h.Subs.List(r.Context())
	if h.writeSubError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) subscriptionDefaults(w http.ResponseWriter, _ *http.Request, _ *requestSession) {
	writeJSON(w, http.StatusOK, map[string]any{
		"options":    sub.DefaultOptions(),
		"user_agent": sub.DefaultUserAgent,
		"regions":    sub.RegionNames(),
	})
}

func (h *handler) createSubscription(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in subscriptions.Input
	if !decodeJSON(w, r, &in) {
		return
	}
	sb, err := h.Subs.Create(r.Context(), in)
	if h.writeSubError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// getSubscription 默认不返回订阅地址；?reveal=1 时返回明文（M1-08）。
func (h *handler) getSubscription(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	sb, err := h.Subs.Get(r.Context(), r.PathValue("id"), r.URL.Query().Get("reveal") == "1")
	if h.writeSubError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (h *handler) updateSubscription(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in subscriptions.Input
	if !decodeJSON(w, r, &in) {
		return
	}
	sb, err := h.Subs.Update(r.Context(), r.PathValue("id"), in)
	if h.writeSubError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (h *handler) deleteSubscription(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	err := h.Subs.Delete(r.Context(), r.PathValue("id"), r.URL.Query().Get("confirm") == "1")
	if h.writeSubError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) previewSubscription(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	p, err := h.Subs.Preview(r.Context(), r.PathValue("id"))
	if errors.Is(err, subscriptions.ErrNotFound) {
		h.writeSubError(w, err)
		return
	}
	if err != nil {
		// 拉取/解析失败是业务结果而非服务端故障，原因已记入 last_error
		writeError(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *handler) applySubscription(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req struct {
		PreviewID string `json:"preview_id"`
		Confirm   bool   `json:"confirm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	d, err := h.Subs.Apply(r.Context(), r.PathValue("id"), req.PreviewID, req.Confirm)
	if h.writeSubError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// writeSubError 映射订阅服务错误；err 为 nil 时返回 false。
func (h *handler) writeSubError(w http.ResponseWriter, err error) bool {
	var ve *subscriptions.ValidationError
	var re *subscriptions.ReferencesError
	switch {
	case err == nil:
		return false
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid", ve.Msg)
	case errors.As(err, &re):
		writeJSON(w, http.StatusConflict, apiError{Code: "references_exist", Message: re.Error(), Detail: re.Impacts})
	case errors.Is(err, subscriptions.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, subscriptions.ErrNameExists):
		writeError(w, http.StatusConflict, "name_exists", err.Error())
	case errors.Is(err, subscriptions.ErrPreviewExpired):
		writeError(w, http.StatusConflict, "preview_expired", err.Error())
	default:
		h.internalError(w, err)
	}
	return true
}
