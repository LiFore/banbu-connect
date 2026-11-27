package panel

import (
	"errors"
	"net/http"

	"boxctl/internal/groups"
	"boxctl/internal/refs"
)

func (h *handler) routeGroups(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/groups", h.authed(h.listGroups, 0))
	mux.Handle("POST /api/v1/groups", h.authed(h.createGroup, 0))
	mux.Handle("POST /api/v1/groups/resolve", h.authed(h.resolveGroup, 0))
	mux.Handle("POST /api/v1/groups/templates", h.authed(h.applyGroupTemplate, 0))
	mux.Handle("POST /api/v1/groups/order", h.authed(h.reorderGroups, 0))
	mux.Handle("GET /api/v1/groups/{id}", h.authed(h.getGroup, 0))
	mux.Handle("PUT /api/v1/groups/{id}", h.authed(h.updateGroup, 0))
	mux.Handle("DELETE /api/v1/groups/{id}", h.authed(h.deleteGroup, 0))
}

func (h *handler) listGroups(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	list, err := h.Groups.List(r.Context())
	if h.writeGroupError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) getGroup(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	v, err := h.Groups.Get(r.Context(), r.PathValue("id"))
	if h.writeGroupError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handler) createGroup(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in groups.Group
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := h.Groups.Create(r.Context(), in)
	if h.writeGroupError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (h *handler) updateGroup(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in groups.Group
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := h.Groups.Update(r.Context(), r.PathValue("id"), in)
	if h.writeGroupError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handler) deleteGroup(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	err := h.Groups.Delete(r.Context(), r.PathValue("id"), r.URL.Query().Get("confirm") == "1")
	if h.writeGroupError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveGroup 预览未保存的组会得到哪些成员。
func (h *handler) resolveGroup(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in groups.Group
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := h.Groups.Resolve(r.Context(), in)
	if h.writeGroupError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handler) applyGroupTemplate(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req groups.TemplateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := h.Groups.ApplyTemplate(r.Context(), req)
	if h.writeGroupError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) reorderGroups(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if h.writeGroupError(w, h.Groups.Reorder(r.Context(), req.IDs)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) writeGroupError(w http.ResponseWriter, err error) bool {
	var ve *groups.ValidationError
	var re *refs.Error
	switch {
	case err == nil:
		return false
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid", ve.Msg)
	case errors.As(err, &re):
		writeJSON(w, http.StatusConflict, apiError{Code: "references_exist", Message: re.Error(), Detail: re.Impacts})
	case errors.Is(err, groups.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		h.internalError(w, err)
	}
	return true
}
