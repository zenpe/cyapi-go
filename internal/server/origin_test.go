package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"cyapi-go/internal/config"
)

func TestRequestOrigin(t *testing.T) {
	tests := []struct {
		name, target, peer, host, proto, want string
		trusted                               bool
		status                                int
	}{
		{"LAN", "http://192.168.3.100:8080/origin", "192.168.3.20:1000", "", "", "http://192.168.3.100:8080", false, 200},
		{"TLS", "https://music.example/origin", "192.168.3.20:1000", "", "", "https://music.example", false, 200},
		{"IPv6", "http://[::1]:8080/origin", "[::1]:1000", "", "", "http://[::1]:8080", false, 200},
		{"proxy", "http://localhost:8080/origin", "127.0.0.1:1000", "api.example:8443", "https", "https://api.example:8443", true, 200},
		{"preserved host", "http://api.example/origin", "127.0.0.1:1000", "", "https", "https://api.example", true, 200},
		{"untrusted spoof", "http://nas.local:8080/origin", "192.168.3.20:1000", "evil.example", "https", "http://nas.local:8080", true, 200},
		{"loopback not implicitly trusted", "http://localhost:8080/origin", "127.0.0.1:1000", "evil.example", "https", "http://localhost:8080", false, 200},
		{"host list", "http://localhost/origin", "127.0.0.1:1000", "api.example, evil.example", "https", "", true, 400},
		{"host userinfo", "http://localhost/origin", "127.0.0.1:1000", "user@api.example", "https", "", true, 400},
		{"host path", "http://localhost/origin", "127.0.0.1:1000", "api.example/path", "https", "", true, 400},
		{"proto list", "http://localhost/origin", "127.0.0.1:1000", "api.example", "https,http", "", true, 400},
		{"bad proto", "http://localhost/origin", "127.0.0.1:1000", "api.example", "javascript", "", true, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Server{}
			if tt.trusted {
				cfg.TrustedProxies = []string{"127.0.0.1/32"}
			}
			h := NewHandler(cfg, readiness{}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(m *http.ServeMux) {
				m.HandleFunc("GET /origin", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, RequestOrigin(r)) })
			})
			req := httptest.NewRequest("GET", tt.target, nil)
			req.RemoteAddr = tt.peer
			if tt.host != "" {
				req.Header.Set("X-Forwarded-Host", tt.host)
			}
			if tt.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.status || (w.Code == 200 && w.Body.String() != tt.want) {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), tt.status, tt.want)
			}
		})
	}
}

func TestDuplicateProxyOriginHeaders(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost/", nil)
	req.RemoteAddr = "127.0.0.1:1000"
	req.Header.Add("X-Forwarded-Host", "api.example")
	req.Header.Add("X-Forwarded-Host", "evil.example")
	h := NewHandler(config.Server{TrustedProxies: []string{"127.0.0.1/32"}}, readiness{}, slog.Default())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("duplicate headers: %d", w.Code)
	}
}
