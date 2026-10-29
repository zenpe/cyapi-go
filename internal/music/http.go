package music

import (
	"cyapi-go/internal/auth"
	"cyapi-go/internal/server"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Handler struct {
	service *Service
	protect func(http.HandlerFunc) http.HandlerFunc
}

func NewHandler(s *Service, protect func(http.HandlerFunc) http.HandlerFunc) *Handler {
	return &Handler{s, protect}
}
func (h *Handler) Register(mux *http.ServeMux) {
	for pattern, kind := range map[string]string{"POST /search": "search", "POST /song": "song", "GET /lyrics/netease": "lyric", "GET /playlist": "playlist", "GET /album": "album"} {
		mux.HandleFunc(pattern, h.protect(h.endpoint(kind)))
	}
}
func scalar(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case json.Number:
		return string(v), true
	}
	return "", false
}
func parameters(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	if r.Method == "GET" {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for k, v := range q {
			if len(v) != 1 {
				return nil, io.ErrUnexpectedEOF
			}
			out[k] = v[0]
		}
		return out, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil {
		return nil, e
	}
	result := map[string]any{}
	switch media {
	case "application/json":
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if e = decoder.Decode(&result); e != nil || result == nil {
			return nil, io.ErrUnexpectedEOF
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
	case "application/x-www-form-urlencoded":
		if e = r.ParseForm(); e != nil {
			return nil, e
		}
		for k, v := range r.PostForm {
			if len(v) != 1 {
				return nil, io.ErrUnexpectedEOF
			}
			result[k] = v[0]
		}
	default:
		return nil, io.ErrUnexpectedEOF
	}
	return result, nil
}
func (h *Handler) endpoint(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values, e := parameters(w, r)
		if e != nil {
			write(w, invalid("请求格式无效或超过16 KiB"), "BYPASS")
			return
		}
		q := Query{Kind: kind}
		get := func(key string) string { s, _ := scalar(values[key]); return s }
		number := func(key string, def int) (int, bool) {
			value, exists := values[key]
			if !exists {
				return def, true
			}
			s, ok := scalar(value)
			if !ok || s == "" {
				return 0, false
			}
			n, e := strconv.Atoi(s)
			return n, e == nil
		}
		switch kind {
		case "search":
			q.Keywords, _ = values["keywords"].(string)
			var ok bool
			q.Page, ok = number("page", 1)
			if !ok {
				write(w, invalid("page 或 limit 参数无效"), "BYPASS")
				return
			}
			q.Limit, ok = number("limit", 20)
			if !ok {
				write(w, invalid("page 或 limit 参数无效"), "BYPASS")
				return
			}
		case "song":
			q.ID = get("ids")
			if q.ID == "" {
				q.ID = get("url")
			}
			q.Level = get("level")
			q.Mode = get("type")
		case "lyric":
			q.ID = get("id")
			if q.ID == "" {
				q.ID = get("ids")
			}
			if q.ID == "" {
				q.ID = get("url")
			}
		case "playlist", "album":
			q.ID = get("id")
			if kind == "playlist" {
				var ok bool
				q.Limit, ok = number("limit", 0)
				if !ok {
					write(w, invalid("歌单 limit 参数无效"), "BYPASS")
					return
				}
			}
		}
		control := strings.ToLower(r.Header.Get("Cache-Control"))
		bypass := strings.Contains(control, "no-cache") || strings.Contains(control, "no-store")
		out, label := h.service.Execute(r.Context(), auth.CurrentUser(r.Context()).ID, server.ClientIP(r.Context()), q, bypass)
		write(w, out, label)
	}
}
func write(w http.ResponseWriter, r Response, label string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Cache", label)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.ContentType != "" {
		w.Header().Set("Content-Type", r.ContentType)
	}
	if r.RetryAfter != "" {
		w.Header().Set("Retry-After", r.RetryAfter)
	}
	if r.Location != "" {
		w.Header().Set("Location", r.Location)
	}
	w.WriteHeader(r.Status)
	_, _ = w.Write(r.Body)
}
