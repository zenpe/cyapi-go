package userstate

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/library"
	"cyapi-go/internal/requestbody"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type Handler struct {
	s     *Service
	guard func(http.HandlerFunc) http.HandlerFunc
	log   *slog.Logger
}

func NewHandler(s *Service, guard func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) *Handler {
	return &Handler{s, guard, log}
}
func (h *Handler) Register(mux *http.ServeMux) {
	for pattern, kind := range map[string]string{
		"POST /playback/save": "save", "GET /playback/last": "last", "DELETE /playback/clear": "clear", "POST /stats/listening-time": "listening-time", "POST /stats/play-count": "play-count", "GET /stats": "stats", "GET /stats/records": "records", "GET /stats/song-memory": "song-memory", "POST /auth/update-username": "username", "POST /auth/update-location": "location", "GET /accounts/bindings": "bindings", "POST /accounts/netease/unbind": "unbind-netease", "POST /accounts/kugou/unbind": "unbind-kugou"} {
		mux.HandleFunc(pattern, h.guard(func(w http.ResponseWriter, r *http.Request) {
			b := map[string]any{}
			var e error
			if r.Method == "GET" || r.Method == "POST" && (kind != "unbind-netease" && kind != "unbind-kugou") {
				b, e = requestbody.Read(w, r, 16<<10)
			}
			out := library.Result{}
			if e != nil {
				e = library.Fail(400, "INVALID_BODY", "请求格式无效或超过16 KiB")
			} else {
				out, e = h.s.Execute(r.Context(), auth.CurrentUser(r.Context()).ID, kind, b)
			}
			if e != nil {
				var f *library.Failure
				switch {
				case errors.As(e, &f):
					out = library.Rejected(f)
				case errors.Is(e, context.DeadlineExceeded):
					out = library.Rejected(library.Fail(504, "REQUEST_TIMEOUT", "请求超时"))
				default:
					h.log.Error("user state request failed")
					out = library.Rejected(library.Fail(500, "INTERNAL_ERROR", "操作失败，请稍后重试"))
				}
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "private, no-store")
			w.WriteHeader(out.Status)
			_ = json.NewEncoder(w).Encode(out.Body)
		}))
	}
}
