package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"cyapi-go/internal/config"
)

type Readiness interface{ CheckSchema(context.Context) error }
type clientIPKey struct{}

func ClientIP(ctx context.Context) string { v, _ := ctx.Value(clientIPKey{}).(string); return v }

// Only a trusted direct peer may supply X-Real-IP. The edge must overwrite it.
func clientIP(r *http.Request, prefixes []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	for _, p := range prefixes {
		if p.Contains(ip) {
			if real, e := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); e == nil {
				return real.Unmap().String()
			}
			break
		}
	}
	return ip.String()
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(s int) {
	if r.status != 0 {
		return
	}
	r.status = s
	r.ResponseWriter.WriteHeader(s)
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(200)
	}
	return r.ResponseWriter.Write(b)
}
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func NewHandler(c config.Server, db Readiness, log *slog.Logger, register ...func(*http.ServeMux)) http.Handler {
	var prefixes []netip.Prefix
	for _, raw := range c.TrustedProxies {
		p, err := netip.ParsePrefix(raw)
		if err == nil {
			prefixes = append(prefixes, p)
		}
	}
	origins := map[string]bool{}
	for _, o := range c.CORSAllowedOrigins {
		origins[o] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"status": 200, "data": "OK"})
	})
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		respond(w, 200, map[string]any{"status": 200, "message": "alive"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.CheckSchema(ctx); err != nil {
			respond(w, 503, map[string]any{"status": 503, "message": "not ready"})
			return
		}
		respond(w, 200, map[string]any{"status": 200, "message": "ready"})
	})
	for _, addRoutes := range register {
		addRoutes(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 404, map[string]any{"status": 404, "code": 404, "message": "Not Found", "traceId": w.Header().Get("X-Trace-ID")})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &recorder{ResponseWriter: w}
		var b [16]byte
		_, _ = rand.Read(b[:])
		trace := hex.EncodeToString(b[:])
		rec.Header().Set("X-Trace-ID", trace)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("request panic", "trace", trace)
				if rec.status == 0 {
					respond(rec, 500, map[string]any{"status": 500, "message": "Internal error", "traceId": trace})
				}
			}
			status := rec.status
			if status == 0 {
				status = 200
			}
			log.Info("http request", "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds(), "trace", trace)
		}()
		// Vary applies also to requests without Origin, preventing cache mixing.
		rec.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" {
			if !origins[origin] {
				respond(rec, 403, map[string]any{"status": 403, "message": "Origin not allowed"})
				return
			}
			rec.Header().Set("Access-Control-Allow-Origin", origin)
			rec.Header().Set("Access-Control-Allow-Credentials", "true")
			rec.Header().Set("Access-Control-Expose-Headers", "X-Trace-ID, Retry-After")
		}
		if r.Method == http.MethodOptions {
			rec.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			rec.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Cache-Control")
			rec.WriteHeader(http.StatusNoContent)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), clientIPKey{}, clientIP(r, prefixes)))
		r.Body = http.MaxBytesReader(rec, r.Body, 1<<20)
		mux.ServeHTTP(rec, r)
	})
}
