package library

import (
	"context"
	"cyapi-go/internal/auth"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

type Handler struct {
	service *Service
	protect func(http.HandlerFunc) http.HandlerFunc
	log     *slog.Logger
}

func NewHandler(s *Service, protect func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) *Handler {
	return &Handler{s, protect, log}
}
func (h *Handler) Register(mux *http.ServeMux) {
	for pattern, kind := range map[string]string{"GET /favorites": "favorites", "GET /playlists": "playlists", "GET /playlists/check-track": "check", "GET /playlists/{playlistId}/tracks": "tracks"} {
		mux.HandleFunc(pattern, h.protect(h.read(kind)))
	}
	mux.HandleFunc("POST /favorites", h.protect(h.favorite))
	mux.HandleFunc("DELETE /favorites/{trackId}/{source}", h.protect(h.unfavorite))
	for pattern, kind := range map[string]string{"POST /playlists": "create", "PUT /playlists/{playlistId}": "rename", "DELETE /playlists/{playlistId}": "delete", "POST /playlists/{playlistId}/delete": "delete", "POST /playlists/{playlistId}/tracks": "add", "POST /playlists/{playlistId}/tracks/batch": "batch-add", "DELETE /playlists/{playlistId}/items/{itemId}": "remove", "POST /playlists/{playlistId}/items/batch-remove": "batch-remove", "POST /playlists/{playlistId}/items/{itemId}/replace": "replace", "POST /playlists/{playlistId}/items/reorder": "reorder"} {
		mux.HandleFunc(pattern, h.protect(h.write(kind)))
	}
}
func (h *Handler) reply(w http.ResponseWriter, result Result, err error) {
	if err != nil {
		var failure *Failure
		switch {
		case errors.As(err, &failure):
			result = Rejected(failure)
		case errors.Is(err, context.DeadlineExceeded):
			result = Rejected(Fail(504, "REQUEST_TIMEOUT", "请求超时，请稍后重试"))
		default:
			h.log.Error("library request failed")
			result = Rejected(Fail(500, "INTERNAL_ERROR", "操作失败，请稍后重试"))
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(result.Status)
	_ = json.NewEncoder(w).Encode(result.Body)
}
func (h *Handler) read(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := Query{Kind: kind, TrackID: r.URL.Query().Get("trackId"), Source: r.URL.Query().Get("source")}
		if kind == "tracks" {
			id, e := ParseID(r.PathValue("playlistId"))
			if e != nil {
				h.reply(w, Result{}, e)
				return
			}
			q.PlaylistID = id
		}
		out, e := h.service.Read(r.Context(), auth.CurrentUser(r.Context()).ID, q)
		h.reply(w, out, e)
	}
}
func decodeBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	bad := Fail(400, "INVALID_BODY", "请求格式无效或超过1 MiB")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil {
		return nil, bad
	}
	var body map[string]any
	switch media {
	case "application/json":
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if e = dec.Decode(&body); e != nil || body == nil {
			return nil, bad
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return nil, bad
		}
	case "application/x-www-form-urlencoded":
		if e = r.ParseForm(); e != nil {
			return nil, bad
		}
		body = map[string]any{}
		for k, v := range r.PostForm {
			if len(v) != 1 {
				return nil, bad
			}
			body[k] = v[0]
		}
		for _, k := range []string{"track", "tracks", "itemIds", "sourceIds"} {
			if raw, ok := body[k].(string); ok {
				dec := json.NewDecoder(strings.NewReader(raw))
				dec.UseNumber()
				var value any
				if dec.Decode(&value) != nil || dec.Decode(new(any)) != io.EOF {
					return nil, bad
				}
				body[k] = value
			}
		}
	default:
		return nil, bad
	}
	return body, nil
}
func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	default:
		return ""
	}
}
func trackFrom(body map[string]any) Track {
	if nested, ok := body["track"].(map[string]any); ok {
		body = nested
	}
	ids := map[string]string{}
	if raw, ok := body["sourceIds"].(map[string]any); ok {
		for key, value := range raw {
			if s, ok := value.(string); ok {
				ids[key] = s
			}
		}
	}
	return Track{TrackID: text(body["trackId"]), Name: text(body["name"]), Artists: text(body["artists"]), Album: text(body["album"]), PicURL: text(body["picUrl"]), Source: text(body["source"]), SourceIDs: ids}
}
func (h *Handler) favorite(w http.ResponseWriter, r *http.Request) {
	b, e := decodeBody(w, r)
	if e != nil {
		h.reply(w, Result{}, e)
		return
	}
	out, e := h.service.Favorite(r.Context(), auth.CurrentUser(r.Context()).ID, trackFrom(b))
	h.reply(w, out, e)
}
func (h *Handler) unfavorite(w http.ResponseWriter, r *http.Request) {
	out, e := h.service.Unfavorite(r.Context(), auth.CurrentUser(r.Context()).ID, r.PathValue("trackId"), r.PathValue("source"))
	h.reply(w, out, e)
}
func (h *Handler) write(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := Command{Kind: kind}
		var e error
		if kind != "create" {
			c.PlaylistID, e = ParseID(r.PathValue("playlistId"))
			if e != nil {
				h.reply(w, Result{}, e)
				return
			}
		}
		if kind == "remove" || kind == "replace" {
			c.ItemID, e = ParseID(r.PathValue("itemId"))
			if e != nil {
				h.reply(w, Result{}, e)
				return
			}
		}
		b := map[string]any{}
		if kind != "delete" {
			b, e = decodeBody(w, r)
			if e != nil {
				h.reply(w, Result{}, e)
				return
			}
		}
		c.Name = text(b["name"])
		c.OperationID = text(b["operationId"])
		c.Track = trackFrom(b)
		if kind == "batch-add" {
			raw, ok := b["tracks"].([]any)
			if !ok || len(raw) > MaxBatch {
				h.reply(w, Result{}, Fail(400, "INVALID_TRACKS", "歌曲列表无效"))
				return
			}
			for _, v := range raw {
				m, ok := v.(map[string]any)
				if !ok {
					h.reply(w, Result{}, Fail(400, "INVALID_TRACK", "歌曲信息无效"))
					return
				}
				c.Tracks = append(c.Tracks, trackFrom(m))
			}
		}
		if kind == "batch-remove" || kind == "reorder" {
			raw, ok := b["itemIds"].([]any)
			if !ok || len(raw) > MaxItems {
				h.reply(w, Result{}, Fail(400, "INVALID_ITEMS", "条目列表无效"))
				return
			}
			for _, v := range raw {
				id, e := ParseID(text(v))
				if e != nil {
					h.reply(w, Result{}, e)
					return
				}
				c.ItemIDs = append(c.ItemIDs, id)
			}
			if kind == "reorder" {
				revision, err := strconv.ParseInt(text(b["expectedOrderRevision"]), 10, 64)
				if err != nil {
					h.reply(w, Result{}, Fail(400, "INVALID_ORDER", "缺少有效的排序版本"))
					return
				}
				c.ExpectedOrderRevision = revision
			}
		}
		out, e := h.service.Write(r.Context(), auth.CurrentUser(r.Context()).ID, c)
		h.reply(w, out, e)
	}
}
