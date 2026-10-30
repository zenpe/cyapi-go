package neteaseaccount

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/providers/netease"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPAccountOwnershipAndExpiry(t *testing.T) {
	expired := false
	s, db := testService(t, fakePlatform{func(_ context.Context, cookie, path string, data map[string]any) (map[string]any, string, error) {
		if path == "/w/nuser/account/get" {
			return profile(101), "", nil
		}
		if expired {
			return nil, "", netease.ErrAccountExpired
		}
		return map[string]any{"code": float64(200), "recommend": []any{}}, "", nil
	}})
	if e := db.ORM.Exec("INSERT INTO users(id,email,username,password_hash) VALUES (1,'12345@qq.com','user1','unused'),(2,'12346@qq.com','user2','unused')").Error; e != nil {
		t.Fatal(e)
	}
	secret := strings.Repeat("X", 32)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts, e := auth.New(authstore.New(db.ORM), nil, auth.Options{Secret: secret, TokenMaxAge: time.Hour, BcryptCost: 4, Logger: log})
	if e != nil {
		t.Fatal(e)
	}
	guard := auth.NewHandler(accounts, nil, log)
	mux := http.NewServeMux()
	NewHandler(s, guard.Protect).Register(mux)
	signer, _ := auth.NewSigner(secret, time.Hour)
	call := func(user int64, method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if user != 0 {
			token, _ := signer.Issue(user, 0, time.Now())
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var out map[string]any
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("invalid response")
		}
		return out
	}
	call(0, "GET", "/accounts/bindings", "", 401)
	started := call(1, "POST", "/accounts/netease/bind-sessions", `{"method":"web"}`, 200)
	id := started["data"].(map[string]any)["id"].(string)
	call(2, "POST", "/accounts/netease/bind-sessions/"+id+"/complete", `{"cookie":"MUSIC_U=private"}`, 410)
	call(1, "POST", "/accounts/netease/bind-sessions/"+id+"/complete", `{"cookie":"MUSIC_U=private"}`, 200)
	call(1, "GET", "/netease/user/playlists?limit=bad&offset=0", "", 400)
	call(2, "GET", "/recommend/resource", "", 409)
	expired = true
	response := call(1, "GET", "/recommend/resource", "", 409)
	if response["error"] != "NETEASE_LOGIN_EXPIRED" {
		t.Fatal(response)
	}
	binding := call(1, "GET", "/accounts/bindings", "", 200)["data"].(map[string]any)["netease"].(map[string]any)
	if binding["status"] != "expired" {
		t.Fatal(binding)
	}
}
func TestQRCancellationCannotBind(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, _ := testService(t, fakePlatform{func(_ context.Context, cookie, path string, _ map[string]any) (map[string]any, string, error) {
		switch path {
		case "/login/qrcode/unikey":
			return map[string]any{"code": float64(200), "unikey": "test-key"}, "", nil
		case "/login/qrcode/client/login":
			close(entered)
			<-release
			return map[string]any{"code": float64(803)}, "MUSIC_U=private", nil
		default:
			return profile(101), "", nil
		}
	}})
	a, e := s.Start(context.Background(), 1, "qr")
	if e != nil {
		t.Fatal(e)
	}
	id := a["id"].(string)
	done := make(chan error, 1)
	go func() { _, e := s.Poll(context.Background(), 1, id); done <- e }()
	<-entered
	s.Cancel(1, id)
	close(release)
	code(t, <-done, "BINDING_CHANGED")
	b, _ := s.Bindings(context.Background(), 1)
	if b["netease"].(map[string]any)["bound"] != false {
		t.Fatal("canceled QR resurrected binding")
	}
}

func TestQRBindingStatesAndPublicResult(t *testing.T) {
	count := 0
	s, _ := testService(t, fakePlatform{func(_ context.Context, cookie, path string, _ map[string]any) (map[string]any, string, error) {
		switch path {
		case "/login/qrcode/unikey":
			return map[string]any{"code": float64(200), "unikey": "test-key"}, "", nil
		case "/login/qrcode/client/login":
			count++
			if count == 3 {
				return map[string]any{"code": float64(803)}, "MUSIC_U=private", nil
			}
			return map[string]any{"code": float64(800 + count)}, "", nil
		default:
			return profile(101), "", nil
		}
	}})
	a, e := s.Start(context.Background(), 1, "qr")
	if e != nil {
		t.Fatal(e)
	}
	id := a["id"].(string)
	if _, e = s.Poll(context.Background(), 2, id); e == nil {
		t.Fatal("foreign QR session accessible")
	}
	for _, want := range []string{"waiting", "scanned", "bound"} {
		s.mu.Lock()
		s.attempts[1].LastPoll = time.Time{}
		s.mu.Unlock()
		out, e := s.Poll(context.Background(), 1, id)
		if e != nil || out["status"] != want {
			t.Fatal(out, e)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "MUSIC_U") {
			t.Fatal("credential response leak")
		}
	}
	b, _ := s.store.Get(context.Background(), 1)
	if b.Status != "bound" || b.Revision != 1 {
		t.Fatal("QR binding not committed")
	}
}
