package mail

import (
	"context"
	"cyapi-go/internal/auth"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrevoDelivery(t *testing.T) {
	status := 201
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.Header.Get("api-key") != "test-secret" {
			t.Error("bad request")
		}
		var p struct {
			HTML    string `json:"htmlContent"`
			To      []struct{ Email string }
			Subject string
		}
		if e := json.NewDecoder(r.Body).Decode(&p); e != nil {
			t.Error(e)
		}
		if strings.Contains(p.HTML, "<script>") || !strings.Contains(p.HTML, "&lt;script&gt;") || !strings.Contains(p.HTML, "123456") || len(p.To) != 1 || p.To[0].Email != "123456@qq.com" {
			t.Error(p)
		}
		w.WriteHeader(status)
		w.Write([]byte("provider test-secret detail"))
	}))
	defer srv.Close()
	b := NewBrevo(Options{APIKey: "test-secret", SenderEmail: "sender@example.com", Endpoint: srv.URL})
	m := auth.Message{To: "123456@qq.com", Username: "<script>", Code: "123456", Purpose: "register", Expires: 10 * time.Minute}
	if e := b.Send(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	status = 500
	if e := b.Send(context.Background(), m); !errors.Is(e, auth.ErrMailDelivery) || strings.Contains(e.Error(), "test-secret") {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	if NewBrevo(Options{}).Ready() {
		t.Fatal("missing credentials ready")
	}
}
func TestBrevoRefusesRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer srv.Close()
	b := NewBrevo(Options{APIKey: "secret", SenderEmail: "sender@example.com", Endpoint: srv.URL})
	if e := b.Send(context.Background(), auth.Message{}); !errors.Is(e, auth.ErrMailDelivery) {
		t.Fatal(e)
	}
	if leaked.Load() {
		t.Fatal("followed redirect")
	}
}
