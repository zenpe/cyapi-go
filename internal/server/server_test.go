package server

import (
	"bytes"
	"context"
	"cyapi-go/internal/config"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

type readiness struct{ err error }

func (r readiness) CheckSchema(context.Context) error { return r.err }
func TestHealthAndLogRedaction(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(config.Server{}, readiness{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	for _, p := range []string{"/", "/health/live", "/health/ready"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p+"?secret=supersecret", nil))
		if w.Code != 200 || w.Header().Get("X-Trace-ID") == "" {
			t.Fatal(p, w.Code)
		}
		if p == "/" && !strings.Contains(w.Body.String(), `"data":"OK"`) {
			t.Fatal("root contract changed")
		}
	}
	if strings.Contains(logs.String(), "supersecret") {
		t.Fatal("query leaked")
	}
	h = NewHandler(config.Server{}, readiness{errors.New("secret DSN")}, slog.Default())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "DSN") {
		t.Fatal("readiness leakage")
	}
}
func TestTrustedProxyIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.1:2000"
	r.Header.Set("X-Real-IP", "203.0.113.1")
	p := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	if clientIP(r, p) != "198.51.100.1" {
		t.Fatal("untrusted header accepted")
	}
	r.RemoteAddr = "127.0.0.1:2000"
	if clientIP(r, p) != "203.0.113.1" {
		t.Fatal("trusted header not accepted")
	}
	r.Header.Set("X-Real-IP", "203.0.113.1, 203.0.113.2")
	if clientIP(r, p) != "127.0.0.1" {
		t.Fatal("multiple addresses accepted")
	}
}
func TestCORSAndMissingRoutes(t *testing.T) {
	h := NewHandler(config.Server{CORSAllowedOrigins: []string{"https://app.example"}}, readiness{}, slog.Default())
	r := httptest.NewRequest(http.MethodOptions, "/auth/login", nil)
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatal("preflight")
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("origin accepted")
	}
	for _, p := range []string{"/auth/login", "/bili/proxy", "/douyin"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 404 {
			t.Fatal("unimplemented route pretends success")
		}
	}
}

func TestDeferredQRLoginNotAvailable(t *testing.T) {
	h := NewHandler(config.Server{}, readiness{}, slog.Default())
	for _, p := range []string{"/auth/qr-login/create", "/auth/qr-login/poll", "/auth/qr-login/scan", "/auth/qr-login/confirm", "/auth/qr-login/cancel", "/login/qr/key", "/login/qr/create", "/login/qr/check", "/kugou/login/qr/key", "/kugou/login/qr/check"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, p, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("deferred QR route %s %s returned %d", method, p, w.Code)
			}
		}
	}
}
