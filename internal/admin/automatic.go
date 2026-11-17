package admin

import (
	"cyapi-go/internal/sourcerecommendation"
	"errors"
	"net/http"
)

func (h *Handler) WithAutomatic(a *sourcerecommendation.Automatic) *Handler {
	h.automatic = a
	return h
}
func (h *Handler) automaticRoutes(routes map[string]http.HandlerFunc) {
	routes["GET /admin/source-recommendations/automatic"] = func(w http.ResponseWriter, r *http.Request) {
		out, e := h.automatic.Status(r.Context(), true)
		if e != nil {
			h.failure(w, e)
			return
		}
		reply(w, 200, out)
	}
	routes["GET /admin/source-recommendations/automatic/history"] = func(w http.ResponseWriter, r *http.Request) {
		history, e := h.recommendations.AutomaticHistory(r.Context())
		if e != nil {
			h.failure(w, e)
			return
		}
		reply(w, 200, map[string]any{"history": history})
	}
	for _, action := range []string{"control", "check", "rollback"} {
		action := action
		routes["POST /admin/source-recommendations/automatic/"+action] = func(w http.ResponseWriter, r *http.Request) {
			var in sourcerecommendation.ControlInput
			if e := decode(w, r, &in); e != nil {
				h.failure(w, e)
				return
			}
			switch action {
			case "control":
				if !h.automatic.Options.Enabled && !in.Paused {
					h.failure(w, sourcerecommendation.ErrConflict)
					return
				}
				state, e := h.recommendations.AutomaticControl(r.Context(), in)
				if e != nil {
					h.failure(w, e)
					return
				}
				h.automatic.CancelOwned()
				reply(w, 200, map[string]any{"state": state})
			case "check":
				id, e := h.automatic.Request(r.Context(), in)
				if e != nil {
					h.failure(w, e)
					return
				}
				reply(w, 202, map[string]any{"requestId": id})
			case "rollback":
				state, e := h.recommendations.AutomaticRollback(r.Context(), in, h.userID(r))
				if errors.Is(e, sourcerecommendation.ErrGone) {
					reply(w, 404, map[string]any{"message": "回退版本已清理"})
					return
				}
				if e != nil {
					h.failure(w, e)
					return
				}
				h.automatic.CancelOwned()
				reply(w, 200, map[string]any{"state": state})
			}
		}
	}
}
