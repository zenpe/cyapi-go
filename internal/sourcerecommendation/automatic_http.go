package sourcerecommendation

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func automaticError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
func (a *Automatic) Register(m *http.ServeMux) {
	m.HandleFunc("GET /lx/recommendation-plan", func(w http.ResponseWriter, r *http.Request) {
		_, p, e := a.Store.AutomaticCurrent(r.Context())
		if errors.Is(e, ErrUnpublished) {
			automaticError(w, 404, "recommendation_unpublished")
			return
		}
		if e != nil {
			automaticError(w, 503, "recommendation_unavailable")
			return
		}
		etag := `"` + p.PlanID + `"`
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	m.HandleFunc("GET /lx/recommendation-status", func(w http.ResponseWriter, r *http.Request) {
		value, e := a.Status(r.Context(), false)
		if e != nil {
			automaticError(w, 503, "recommendation_unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(value)
	})
	m.HandleFunc("GET /lx/recommendation-scripts/{file}", func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		if len(file) != 67 || file[64:] != ".js" || !hashPattern.MatchString(file[:64]) {
			automaticError(w, 400, "invalid_recommendation_hash")
			return
		}
		hash := file[:64]
		unlock, e := a.Store.lock(r.Context())
		if e != nil {
			automaticError(w, 503, "recommendation_unavailable")
			return
		}
		refs, _, e := a.Store.automaticReferences()
		var b []byte
		if e == nil && refs[hash] {
			b, e = a.Store.archive(hash)
		}
		unlock()
		if e != nil {
			automaticError(w, 503, "recommendation_unavailable")
			return
		}
		if b == nil {
			automaticError(w, 404, "recommendation_hash_unknown")
			return
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"`+hash+`"`)
		http.ServeContent(w, r, file, time.Time{}, bytes.NewReader(b))
	})
}
