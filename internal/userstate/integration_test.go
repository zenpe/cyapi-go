package userstate_test

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/compat"
	"cyapi-go/internal/config"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/persistence/userstatestore"
	"cyapi-go/internal/server"
	"cyapi-go/internal/userstate"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func database(t *testing.T, driver string) *persistence.Database {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "state.db")
	if driver == "postgres" {
		dsn = os.Getenv("CYAPI_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set CYAPI_TEST_POSTGRES_DSN for PostgreSQL")
		}
		admin, e := persistence.Open(context.Background(), config.Database{Driver: driver, DSN: dsn})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { admin.Close() })
		schema := fmt.Sprintf("cyapi_state_%d", time.Now().UnixNano())
		if e = admin.ORM.Exec("CREATE SCHEMA " + schema).Error; e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { admin.ORM.Exec("DROP SCHEMA " + schema + " CASCADE") })
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	db, e := persistence.Open(context.Background(), config.Database{Driver: driver, DSN: dsn, BusyTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if e = db.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 2; i++ {
		if e = db.ORM.Exec("INSERT INTO users(id,email,username,password_hash) VALUES (?,?,?,?)", i, fmt.Sprintf("12345%d@qq.com", i), fmt.Sprint("user", i), "unused").Error; e != nil {
			t.Fatal(e)
		}
	}
	return db
}
func TestBatchUserDataAndCompatibility(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := database(t, driver)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			secret := strings.Repeat("S", 32)
			accounts, e := auth.New(authstore.New(db.ORM), nil, auth.Options{Secret: secret, TokenMaxAge: time.Hour, BcryptCost: 4, Logger: log})
			if e != nil {
				t.Fatal(e)
			}
			guard := auth.NewHandler(accounts, server.ClientIP, log)
			routes := userstate.NewHandler(userstate.New(userstatestore.New(db.ORM)), guard.Protect, log)
			h := server.NewHandler(config.Defaults().Server, db, log, routes.Register, func(mux *http.ServeMux) { compat.Register(mux, guard.Protect) })
			signer, _ := auth.NewSigner(secret, time.Hour)
			tokens := map[int]string{}
			for i := 1; i <= 2; i++ {
				tokens[i], _ = signer.Issue(int64(i), 0, time.Now())
			}
			call := func(user int, method, path string, body any, want int) map[string]any {
				t.Helper()
				b, _ := json.Marshal(body)
				req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
				req.Header.Set("Content-Type", "application/json")
				if user > 0 {
					req.Header.Set("Authorization", "Bearer "+tokens[user])
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("%s %s: %d want %d %s", method, path, w.Code, want, w.Body.String())
				}
				out := map[string]any{}
				if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
					t.Fatal(e)
				}
				if user > 0 && w.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("cacheable account response")
				}
				return out
			}
			call(0, "GET", "/stats", nil, 401)
			call(1, "GET", "/playback/last", nil, 200)
			payload := map[string]any{"trackId": "1", "source": "netease", "trackName": "Song", "position": 10, "isFromPlaylist": true, "userId": 2}
			call(1, "POST", "/playback/save", payload, 200)
			data := call(1, "GET", "/playback/last", nil, 200)["data"].(map[string]any)
			if data["position"] != float64(10) || data["isFromPlaylist"] != true || data["updatedAt"].(float64) <= 0 {
				t.Fatal(data)
			}
			payload["source"] = "qq"
			payload["position"] = 11
			call(1, "POST", "/playback/save", payload, 200)
			data = call(1, "GET", "/playback/last", nil, 200)["data"].(map[string]any)
			if data["source"] != "qq" {
				t.Fatal("source throttled")
			}
			if call(2, "GET", "/playback/last", nil, 200)["data"] != nil {
				t.Fatal("cross-user playback")
			}
			call(1, "DELETE", "/playback/clear", nil, 200)
			call(1, "POST", "/playback/save", payload, 200)
			if call(1, "GET", "/playback/last", nil, 200)["data"] == nil {
				t.Fatal("clear retained guard")
			}
			for _, seconds := range []any{-1, 0, "NaN", 86401, "+Inf"} {
				call(1, "POST", "/stats/listening-time", map[string]any{"seconds": seconds}, 400)
			}
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					call(1, "POST", "/stats/listening-time", map[string]any{"seconds": 2.5}, 200)
					call(1, "POST", "/stats/play-count", payload, 200)
				}()
			}
			wg.Wait()
			for i := 2; i <= 12; i++ {
				call(1, "POST", "/stats/play-count", map[string]any{"trackId": fmt.Sprint(i), "source": "netease"}, 200)
			}
			data = call(1, "GET", "/stats", nil, 200)["data"].(map[string]any)
			if data["totalListeningTime"] != float64(50) || data["totalPlayCount"] != float64(31) || len(data["playCounts"].([]any)) != 10 {
				t.Fatal(data)
			}
			data = call(1, "GET", "/stats/records?page=1&limit=10", nil, 200)["data"].(map[string]any)
			if data["hasMore"] != true || len(data["records"].([]any)) != 10 {
				t.Fatal(data)
			}
			call(1, "GET", "/stats/records?page=1.5", nil, 400)
			data = call(1, "GET", "/stats/song-memory?trackId=1&source=qq", nil, 200)["data"].(map[string]any)
			if data["playCount"] != float64(20) || data["firstPlayedAt"] == nil {
				t.Fatal(data)
			}
			data = call(2, "GET", "/stats", nil, 200)["data"].(map[string]any)
			if data["totalPlayCount"] != float64(0) {
				t.Fatal("cross-user stats")
			}
			call(1, "POST", "/auth/update-username", map[string]any{"newUsername": "user2"}, 400)
			call(1, "POST", "/auth/update-username", map[string]any{"newUsername": "renamed"}, 200)
			call(1, "POST", "/auth/update-location", map[string]any{"ip": "192.0.2.4", "location": "test"}, 200)
			call(1, "GET", "/accounts/bindings", nil, 200)
			call(1, "POST", "/accounts/netease/unbind", nil, 200)
			call(1, "POST", "/accounts/kugou/unbind", nil, 200)
			call(0, "GET", "/auth/linuxdo-status", nil, 200)
			call(0, "POST", "/auth/linuxdo/login", nil, 400)
			call(1, "GET", "/audio/proxy", nil, 501)
			call(0, "GET", "/audio/proxy", nil, 401)
			for _, path := range []string{"/sponsors/create", "/pay/create"} {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
				if w.Code != 404 {
					t.Fatal("deferred sponsorship registered", path, w.Code)
				}
			}
		})
	}
}
