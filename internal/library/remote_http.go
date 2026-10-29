package library

import (
	"cyapi-go/internal/auth"
	"net/http"
)

func (h *Handler) WithRemoteProtection(guard func(http.HandlerFunc) http.HandlerFunc) *Handler {
	h.remoteProtect = guard
	return h
}
func (h *Handler) RegisterRemote(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	for pattern, kind := range map[string]string{"POST /playlists/follow": "follow", "POST /playlists/{playlistId}/sync": "sync", "POST /playlists/{playlistId}/copy": "copy"} {
		mux.HandleFunc(pattern, guard(func(w http.ResponseWriter, r *http.Request) {
			c := Command{Kind: kind}
			if kind != "follow" {
				id, e := ParseID(r.PathValue("playlistId"))
				if e != nil {
					h.reply(w, Result{}, e)
					return
				}
				c.PlaylistID = id
			}
			b, e := decodeBody(w, r)
			if e != nil {
				h.reply(w, Result{}, e)
				return
			}
			c.Name = text(b["name"])
			c.OperationID = text(b["operationId"])
			c.Source = text(b["source"])
			c.SourcePlaylistID = text(b["sourcePlaylistId"])
			out, e := h.service.Remote(r.Context(), auth.CurrentUser(r.Context()).ID, c)
			h.reply(w, out, e)
		}))
	}
}
