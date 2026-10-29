package auth_test

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/server"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeMail struct {
	messages []auth.Message
	fail     bool
	disabled bool
}

func (m *fakeMail) Ready() bool { return !m.disabled }
func (m *fakeMail) Send(_ context.Context, v auth.Message) error {
	m.messages = append(m.messages, v)
	if m.fail {
		return errors.New("provider secret must not leak")
	}
	return nil
}
func (m *fakeMail) code() string { return m.messages[len(m.messages)-1].Code }

var log = slog.New(slog.NewTextHandler(io.Discard, nil))

func openDB(t *testing.T, driver, dsn string) *persistence.Database {
	t.Helper()
	d, e := persistence.Open(context.Background(), config.Database{Driver: driver, DSN: dsn, BusyTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func database(t *testing.T, driver string) *persistence.Database {
	t.Helper()
	var d *persistence.Database
	if driver == "sqlite" {
		d = openDB(t, driver, filepath.Join(t.TempDir(), "app.db"))
	} else {
		dsn := os.Getenv("CYAPI_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set CYAPI_TEST_POSTGRES_DSN to test PostgreSQL")
		}
		u, e := url.Parse(dsn)
		if e != nil || u.Scheme != "postgres" {
			t.Fatal("invalid test DSN")
		}
		admin := openDB(t, driver, dsn)
		schema := fmt.Sprintf("cyapi_auth_%d", time.Now().UnixNano())
		if e = admin.ORM.Exec("CREATE SCHEMA " + schema).Error; e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if e := admin.ORM.Exec("DROP SCHEMA " + schema + " CASCADE").Error; e != nil {
				t.Error(e)
			}
		})
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		d = openDB(t, driver, u.String())
	}
	if e := d.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	return d
}
func newService(t *testing.T, d *persistence.Database, m *fakeMail, now *time.Time) *auth.Service {
	t.Helper()
	s, e := auth.New(authstore.New(d.ORM), m, auth.Options{Secret: strings.Repeat("x", 32), TokenMaxAge: time.Hour, BcryptCost: 4, Now: func() time.Time { return *now }, Logger: log})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestAccountContract(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			d := database(t, driver)
			m := &fakeMail{}
			now := time.Now().UTC()
			s := newService(t, d, m, &now)
			ctx := context.Background()
			const email = "123456@qq.com"
			const password = "password-123"
			must := func(e error) {
				t.Helper()
				if e != nil {
					t.Fatal(e)
				}
			}
			must(s.SendCode(ctx, email, "alice", "register", "ip1"))
			code := m.code()
			var saved string
			must(d.ORM.Raw("SELECT code FROM verification_codes WHERE email = ?", email).Scan(&saved).Error)
			if saved == code || len(saved) != 64 {
				t.Fatal("OTP must be stored as keyed digest")
			}
			if _, e := s.Register(ctx, email, "alice", password, "000000", "ip1"); !errors.Is(e, auth.ErrCode) {
				t.Fatal(e)
			}
			if e := s.ResetPassword(ctx, email, password, code, "ip1"); !errors.Is(e, auth.ErrCode) {
				t.Fatal("cross-purpose OTP", e)
			}
			u, e := s.Register(ctx, email, "alice", password, code, "ip1")
			must(e)
			if !u.IsVerified || u.ID <= 0 {
				t.Fatal(u)
			}
			if _, e = s.Register(ctx, email, "alice", password, code, "ip1"); !errors.Is(e, auth.ErrCode) {
				t.Fatal("reused OTP", e)
			}
			_, token, e := s.Login(ctx, "alice", password, "ip1")
			must(e)
			_, e = s.Authenticate(ctx, token)
			must(e)
			_, _, e = s.Login(ctx, "123456@QQ.COM", password, "ip1")
			must(e)
			must(s.SendCode(ctx, email, "", "reset_password", "ip1"))
			reset := m.code()
			// Parallel redemption must commit exactly one password reset.
			var successes atomic.Int32
			errs := make(chan error, 8)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := s.ResetPassword(ctx, email, "new-password", reset, "ip1")
					if err == nil {
						successes.Add(1)
					} else if !errors.Is(err, auth.ErrCode) {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Error(e)
			}
			if successes.Load() != 1 {
				t.Fatal("OTP redeemed", successes.Load(), "times")
			}
			if _, e = s.Authenticate(ctx, token); !errors.Is(e, auth.ErrUnauthorized) {
				t.Fatal("old token accepted", e)
			}
			if _, _, e = s.Login(ctx, "alice", password, "ip1"); !errors.Is(e, auth.ErrUnauthorized) {
				t.Fatal("old password accepted", e)
			}
			_, newToken, e := s.Login(ctx, "alice", "new-password", "ip1")
			must(e)
			_, e = s.Authenticate(ctx, newToken)
			must(e)
			var version int64
			must(d.ORM.Raw("SELECT token_version FROM users WHERE id = ?", u.ID).Scan(&version).Error)
			if version != 1 {
				t.Fatal(version)
			}
			// A unique conflict rolls back code consumption so a different username can retry.
			must(s.SendCode(ctx, "234567@qq.com", "bob1", "register", "ip1"))
			second := m.code()
			if _, e = s.Register(ctx, "234567@qq.com", "alice", password, second, "ip1"); !errors.Is(e, auth.ErrConflict) {
				t.Fatal(e)
			}
			_, e = s.Register(ctx, "234567@qq.com", "bob1", password, second, "ip1")
			must(e)
			// Successful login refunds only its reservation, not earlier failed attempts.
			for i := 0; i < 8; i++ {
				_, _, e = s.Login(ctx, "alice", "incorrect", "ip1")
				if !errors.Is(e, auth.ErrUnauthorized) {
					t.Fatal(i, e)
				}
			}
			_, _, e = s.Login(ctx, "alice", "new-password", "ip1")
			must(e)
			_, _, e = s.Login(ctx, "alice", "incorrect", "ip1")
			if !errors.Is(e, auth.ErrUnauthorized) {
				t.Fatal(e)
			}
			_, _, e = s.Login(ctx, "alice", "new-password", "ip1")
			var rate *auth.RateError
			if !errors.As(e, &rate) {
				t.Fatal("login lockout missing", e)
			}
			now = now.Add(16 * time.Minute)
			_, _, e = s.Login(ctx, "alice", "new-password", "ip1")
			must(e)
			// Expired, undelivered codes never create accounts.
			must(s.SendCode(ctx, "345678@qq.com", "expire1", "register", "ip1"))
			expired := m.code()
			now = now.Add(11 * time.Minute)
			if _, e = s.Register(ctx, "345678@qq.com", "expire1", password, expired, "ip1"); !errors.Is(e, auth.ErrCode) {
				t.Fatal(e)
			}
			m.fail = true
			if e = s.SendCode(ctx, "456789@qq.com", "failed1", "register", "ip1"); !errors.Is(e, auth.ErrMailDelivery) {
				t.Fatal(e)
			}
			if _, e = s.Register(ctx, "456789@qq.com", "failed1", password, m.code(), "ip1"); !errors.Is(e, auth.ErrCode) {
				t.Fatal("undelivered code active", e)
			}
			m.fail = false
			before := len(m.messages)
			must(s.SendCode(ctx, "999999@qq.com", "", "reset_password", "ip1"))
			if len(m.messages) != before {
				t.Fatal("sent to nonexistent user")
			}
			m.disabled = true
			if s.RegistrationEnabled() {
				t.Fatal("mail disabled")
			}
			if e = s.SendCode(ctx, email, "alice", "register", "ip1"); !errors.Is(e, auth.ErrMailUnavailable) {
				t.Fatal(e)
			}
		})
	}
}
func TestRateStoreConcurrency(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			d := database(t, driver)
			store := authstore.New(d.ORM)
			ctx := context.Background()
			var admitted atomic.Int32
			var wg sync.WaitGroup
			errs := make(chan error, 30)
			for i := 0; i < 30; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ok, _, e := store.Reserve(ctx, "concurrent", 2000, 1000, 10)
					if e != nil {
						errs <- e
					}
					if ok {
						admitted.Add(1)
					}
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Error(e)
			}
			if admitted.Load() != 10 {
				t.Fatal(admitted.Load())
			}
			if ok, expiry, e := store.Reserve(ctx, "concurrent", 4000, 3000, 10); e != nil || !ok || expiry != 4000 {
				t.Fatal(ok, expiry, e)
			}
			if e := store.Refund(ctx, auth.Reservation{Key: "concurrent", ExpiresAt: 2000}); e != nil {
				t.Fatal(e)
			}
			var attempts int
			if e := d.ORM.Raw("SELECT attempts FROM auth_rate_limits WHERE key = ?", "concurrent").Scan(&attempts).Error; e != nil || attempts != 1 {
				t.Fatal("stale refund", attempts, e)
			}
		})
	}
}
func TestHTTPAccountFlow(t *testing.T) {
	d := database(t, "sqlite")
	m := &fakeMail{}
	now := time.Now()
	s := newService(t, d, m, &now)
	routes := auth.NewHandler(s, server.ClientIP, log)
	c := config.Defaults().Server
	handler := server.NewHandler(c, d, log, routes.Register)
	call := func(method, path, body, content, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Content-Type", content)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	check := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	check(call("GET", "/auth/registration-status", "", "", ""), 200)
	w := call("POST", "/auth/register/send-code", `{"email":"123456@qq.com","username":"alice"}`, "application/json", "")
	check(w, 200)
	if strings.Contains(w.Body.String(), m.code()) {
		t.Fatal("OTP leaked")
	}
	w = call("POST", "/auth/register", "email=123456%40qq.com&username=alice&password=password123&code="+m.code(), "application/x-www-form-urlencoded", "")
	check(w, 200)
	w = call("POST", "/auth/login", `{"account":"alice","password":"password123"}`, "application/json", "")
	check(w, 200)
	var response struct{ Data struct{ Token string } }
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), "$2a$") {
		t.Fatal("password leaked")
	}
	check(call("GET", "/auth/validate-token", "", "", response.Data.Token), 200)
	check(call("POST", "/auth/reset-password/send-code", `{"email":"123456@qq.com"}`, "application/json", ""), 200)
	check(call("POST", "/auth/reset-password", `{"email":"123456@qq.com","newPassword":"new-password123","code":"`+m.code()+`"}`, "application/json", ""), 200)
	check(call("GET", "/auth/validate-token", "", "", response.Data.Token), 401)
	check(call("POST", "/auth/login", `{"account":"alice","password":"new-password123"}`, "application/json", ""), 200)
	check(call("GET", "/auth/validate-token", "", "", "legacy.token"), 401)
	w = call("POST", "/auth/register/send-code", `{"email":"123456@qq.com","username":"alice"}`, "application/json", "")
	check(w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry header")
	}
	for _, tc := range []struct{ body, content string }{{`{} {}`, "application/json"}, {`{"account":1}`, "application/json"}, {`null`, "application/json"}, {strings.Repeat("x", 17000), "application/json"}, {"account=a&account=b", "application/x-www-form-urlencoded"}, {"{}", "text/plain"}} {
		check(call("POST", "/auth/login", tc.body, tc.content, ""), 400)
	}
	for _, path := range []string{"/auth/qr-login/create", "/login/qr/key", "/kugou/login/qr/key"} {
		check(call("POST", path, "{}", "application/json", ""), 404)
	}
	if w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("account response cacheable")
	}
}
