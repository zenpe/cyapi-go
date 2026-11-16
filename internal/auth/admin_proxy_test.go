package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cyapi-go/internal/admin"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/server"
	"cyapi-go/internal/sourcecheck"
)

func TestAdminLoginBehindHTTPProxies(t *testing.T) {
	db := database(t, "sqlite")
	mail := &fakeMail{}
	now := time.Now().UTC()
	accounts := newService(t, db, mail, &now)
	ctx := context.Background()
	var adminID int64
	for _, name := range []string{"proxyadmin", "proxyuser"} {
		email := name + "@example.com"
		if err := accounts.SendCode(ctx, email, name, "register", "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
		user, err := accounts.Register(ctx, email, name, "TestPassword123!", mail.code(), "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if name == "proxyadmin" {
			adminID = user.ID
		}
	}
	routes := auth.NewHandler(accounts, server.ClientIP, log)
	checks := &sourcecheck.Service{Directory: t.TempDir()}
	controller := sourcecheck.NewController(ctx, checks)
	t.Cleanup(func() { _ = controller.Close(context.Background()) })
	management := admin.New(checks, controller, nil, routes.Protect, []int64{adminID}, nil, log)
	h := server.NewHandler(config.Server{}, db, log, routes.Register, management.Register)
	call := func(method, path, origin, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		// Edge HTTPS -> HTTP proxy -> HTTP Go hop, with no trusted proxies
		// and a backend Host that does not match the browser's public URL.
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:34567"
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Forwarded-Proto", "http")
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unconfigured origin received a CORS grant")
		}
		return w
	}
	for _, origin := range []string{"https://api.example", "https://renamed.example"} {
		t.Run(origin, func(t *testing.T) {
			login := func(name string) string {
				t.Helper()
				w := call("POST", "/auth/login", origin, "", `{"account":"`+name+`","password":"TestPassword123!"}`)
				var response struct {
					Data struct{ Token string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || response.Data.Token == "" {
					t.Fatalf("proxied login failed: %d %s", w.Code, w.Body.String())
				}
				if len(w.Result().Cookies()) != 0 {
					t.Fatal("login must not establish cookie authentication")
				}
				return response.Data.Token
			}
			adminToken, userToken := login("proxyadmin"), login("proxyuser")
			cookieRequest := httptest.NewRequest("GET", "http://127.0.0.1:8080/admin/source-checks/samples", nil)
			cookieRequest.Header.Set("Origin", origin)
			cookieRequest.AddCookie(&http.Cookie{Name: "token", Value: adminToken})
			cookieResponse := httptest.NewRecorder()
			h.ServeHTTP(cookieResponse, cookieRequest)
			if cookieResponse.Code != 401 {
				t.Fatal("cookie alone must not authenticate an admin request")
			}
			for _, tc := range []struct {
				token string
				want  int
			}{{"", 401}, {"invalid-token", 401}, {userToken, 403}, {adminToken, 200}} {
				if w := call("GET", "/admin/source-checks/samples", origin, tc.token, ""); w.Code != tc.want {
					t.Fatalf("admin access: got %d, want %d", w.Code, tc.want)
				}
			}
			for _, tc := range []struct {
				token string
				want  int
			}{{"", 401}, {userToken, 403}, {adminToken, 400}} {
				// An authorized malformed write reaches validation; other users
				// are rejected before the request body can mutate samples.
				if w := call("POST", "/admin/source-checks/samples", origin, tc.token, `{}`); w.Code != tc.want {
					t.Fatalf("admin write: got %d, want %d", w.Code, tc.want)
				}
			}
			if w := call("GET", "/admin/source-checks/samples", "", adminToken, ""); w.Code != 200 {
				t.Fatalf("native request failed: %d", w.Code)
			}
		})
	}
}
