package panel

import (
	"errors"
	"net/http"

	"boxctl/internal/nodes"
	"boxctl/internal/refs"
)

func (h *handler) routeNodes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/nodes", h.authed(h.listNodes, 0))
	mux.Handle("GET /api/v1/nodes/schemas", h.authed(h.nodeSchemas, 0))
	mux.Handle("POST /api/v1/nodes", h.authed(h.createNode, 0))
	mux.Handle("POST /api/v1/nodes/import", h.authed(h.importNodes, 0))
	mux.Handle("POST /api/v1/nodes/test", h.authed(h.testNode, 0))
	mux.Handle("POST /api/v1/nodes/batch", h.authed(h.batchNodes, 0))
	mux.Handle("POST /api/v1/nodes/batch-delete", h.authed(h.batchDeleteNodes, 0))
	mux.Handle("GET /api/v1/nodes/{id}", h.authed(h.getNode, 0))
	mux.Handle("PUT /api/v1/nodes/{id}", h.authed(h.updateNode, 0))
	mux.Handle("PATCH /api/v1/nodes/{id}", h.authed(h.patchNode, 0))
	mux.Handle("DELETE /api/v1/nodes/{id}", h.authed(h.deleteNode, 0))
}

func (h *handler) listNodes(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	list, err := h.Nodes.List(r.Context())
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) nodeSchemas(w http.ResponseWriter, _ *http.Request, _ *requestSession) {
	writeJSON(w, http.StatusOK, nodes.Schemas)
}

// getNode 默认凭据为掩码；?reveal=1 返回明文（M1-08）。
func (h *handler) getNode(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	d, err := h.Nodes.Get(r.Context(), r.PathValue("id"), r.URL.Query().Get("reveal") == "1")
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *handler) createNode(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in nodes.Input
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := h.Nodes.Create(r.Context(), in)
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *handler) updateNode(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var in nodes.Input
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := h.Nodes.Update(r.Context(), r.PathValue("id"), in)
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *handler) patchNode(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var a nodes.Attrs
	if !decodeJSON(w, r, &a) {
		return
	}
	n, err := h.Nodes.Patch(r.Context(), r.PathValue("id"), a)
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (h *handler) deleteNode(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	err := h.Nodes.Delete(r.Context(), r.PathValue("id"), r.URL.Query().Get("confirm") == "1")
	if h.writeNodeError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) batchNodes(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req struct {
		IDs     []string `json:"ids"`
		Enabled bool     `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	n, err := h.Nodes.SetEnabled(r.Context(), req.IDs, req.Enabled)
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": n})
}

func (h *handler) batchDeleteNodes(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req struct {
		IDs     []string `json:"ids"`
		Confirm bool     `json:"confirm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	deleted, skipped, err := h.Nodes.DeleteMany(r.Context(), req.IDs, req.Confirm)
	if h.writeNodeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted, "skipped": skipped})
}

func (h *handler) importNodes(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, h.Nodes.Import(req.Text))
}

func (h *handler) testNode(w http.ResponseWriter, r *http.Request, _ *requestSession) {
	var req nodes.TestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := h.Nodes.Test(r.Context(), req)
	var ve *nodes.ValidationError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.As(err, &ve), errors.Is(err, nodes.ErrNotFound):
		h.writeNodeError(w, err)
	default:
		// 连接失败是测试结果而非服务端故障
		writeJSON(w, http.StatusBadGateway, apiError{Code: "test_failed", Message: err.Error(), Detail: res})
	}
}

func (h *handler) writeNodeError(w http.ResponseWriter, err error) bool {
	var ve *nodes.ValidationError
	var re *refs.Error
	switch {
	case err == nil:
		return false
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid", ve.Msg)
	case errors.As(err, &re):
		writeJSON(w, http.StatusConflict, apiError{Code: "references_exist", Message: re.Error(), Detail: re.Impacts})
	case errors.Is(err, nodes.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		h.internalError(w, err)
	}
	return true
}
