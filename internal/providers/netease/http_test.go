package netease

import (
	"bytes"
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/library"
	"cyapi-go/internal/music"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/persistence/librarystore"
	"cyapi-go/internal/server"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthenticatedMusicHTTP(t *testing.T) {
	var requests atomic.Int32
	var partial atomic.Bool
	provider := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		r.ParseForm()
		switch r.URL.Path {
		case "/api/cloudsearch/pc":
			if r.Form.Get("s") == "fail" {
				w.WriteHeader(401)
				w.Write([]byte("MUSIC_U=test-secret private upstream data"))
				return
			}
			writeJSON(w, map[string]any{"code": 200, "result": map[string]any{"songCount": 2, "songs": []any{raw(123), raw(456)}}})
		case "/api/v3/song/detail":
			var ids []struct{ ID int64 }
			json.Unmarshal([]byte(r.Form.Get("c")), &ids)
			songs := []any{}
			for _, id := range ids {
				if partial.Load() && id.ID == 456 {
					continue
				}
				song := raw(id.ID)
				song["name"] = "<script>alert(1)</script>"
				songs = append(songs, song)
			}
			writeJSON(w, map[string]any{"code": 200, "songs": songs})
		case "/api/song/lyric":
			switch r.Form.Get("id") {
			case "2":
				writeJSON(w, map[string]any{"code": 200})
			case "3":
				writeJSON(w, map[string]any{"code": 200, "nolyric": true})
			case "4":
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(429)
			case "5":
				w.WriteHeader(401)
			default:
				writeJSON(w, map[string]any{"code": 200, "lrc": map[string]any{"lyric": "[00:01]ready"}})
			}
		case "/eapi/song/enhance/player/url/v1":
			writeJSON(w, map[string]any{"code": 200, "data": []any{map[string]any{"id": 123, "url": "http://m10.music.126.net/music.flac", "size": 1024, "level": "lossless", "expi": 1200}}})
		case "/api/v6/playlist/detail":
			writeJSON(w, map[string]any{"code": 200, "playlist": map[string]any{"id": 99, "name": "Remote playlist", "trackCount": 2, "trackIds": []any{map[string]any{"id": 456}, map[string]any{"id": 123}}}})
		case "/api/v1/album/7":
			writeJSON(w, map[string]any{"code": 200, "album": map[string]any{"id": 7, "name": "Album", "size": 1}, "songs": []any{raw(123)}})
		default:
			w.WriteHeader(404)
		}
	}, 4, time.Second)
	db, e := persistence.Open(context.Background(), config.Database{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	for id := 1; id <= 2; id++ {
		if e = db.ORM.Exec("INSERT INTO users(id,email,username,password_hash) VALUES (?, ?, ?, ?)", id, fmt.Sprintf("12345%d@qq.com", id), fmt.Sprintf("user%d", id), "unused").Error; e != nil {
			t.Fatal(e)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	secret := strings.Repeat("N", 32)
	accounts, e := auth.New(authstore.New(db.ORM), nil, auth.Options{Secret: secret, TokenMaxAge: time.Hour, BcryptCost: 4, Logger: log})
	if e != nil {
		t.Fatal(e)
	}
	guard := auth.NewHandler(accounts, server.ClientIP, log)
	service, e := music.New(provider, music.Options{MaxBytes: 1 << 20, MaxEntries: 100, MaxItemBytes: 1 << 20, PrivateCache: true, UserRequestsPerMinute: 120, IPRequestsPerMinute: 300})
	if e != nil {
		t.Fatal(e)
	}
	routes := music.NewHandler(service, func(next http.HandlerFunc) http.HandlerFunc { return guard.ProtectFor(next, 25*time.Second) })
	store := librarystore.New(db.ORM)
	libraryRoutes := library.NewHandler(library.NewWithRemote(store, store, provider), guard.Protect, log).WithRemoteProtection(func(next http.HandlerFunc) http.HandlerFunc { return guard.ProtectFor(next, 25*time.Second) })
	handler := server.NewHandler(config.Defaults().Server, db, log, routes.Register, libraryRoutes.Register, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /test-budget", guard.ProtectFor(func(w http.ResponseWriter, r *http.Request) {
			deadline, _ := r.Context().Deadline()
			if time.Until(deadline) < 20*time.Second {
				t.Error("business request inherited auth-only deadline")
			}
			w.WriteHeader(200)
		}, 25*time.Second))
	})
	signer, _ := auth.NewSigner(secret, time.Hour)
	tokens := map[int]string{}
	for id := 1; id <= 2; id++ {
		tokens[id], _ = signer.Issue(int64(id), 0, time.Now())
	}
	call := func(user int, method, path, body, media, control string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:5000"
		r.Header.Set("Content-Type", media)
		r.Header.Set("Cache-Control", control)
		if user != 0 {
			r.Header.Set("Authorization", "Bearer "+tokens[user])
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("private API response cacheable")
		}
		if bytes.Contains(w.Body.Bytes(), []byte("test-secret")) || w.Header().Get("Set-Cookie") != "" {
			t.Fatal("provider credentials exposed")
		}
		return w
	}
	call(0, "POST", "/search", `{"keywords":"hello"}`, "application/json", "", 401)
	if requests.Load() != 0 {
		t.Fatal("unauthenticated outbound request")
	}
	first := call(1, "POST", "/search", `{"keywords":"hello","page":1,"limit":20}`, "application/json", "", 200)
	if first.Header().Get("X-Cache") != "MISS" {
		t.Fatal(first.Header())
	}
	second := call(1, "POST", "/search", "keywords=hello&page=1&limit=20", "application/x-www-form-urlencoded", "", 200)
	if second.Header().Get("X-Cache") != "HIT" || first.Body.String() != second.Body.String() || requests.Load() != 1 {
		t.Fatal("normalized form cache mismatch")
	}
	call(2, "POST", "/search", `{"keywords":"hello"}`, "application/json", "", 200)
	if requests.Load() != 2 {
		t.Fatal("cookie-backed response shared across users")
	}
	call(1, "POST", "/search", `{"keywords":"hello"}`, "application/json", "no-cache", 200)
	if requests.Load() != 3 {
		t.Fatal("bypass ignored")
	}
	before := requests.Load()
	for _, body := range []string{`null`, `{} {}`, `{"keywords":"hello","page":true}`, `{"keywords":"hello","limit":51}`, `{"keywords":"hello","page":0}`, `{"keywords":"hello","page":null}`, strings.Repeat("x", 17000)} {
		call(1, "POST", "/search", body, "application/json", "", 400)
	}
	if requests.Load() != before {
		t.Fatal("invalid input reached upstream")
	}
	for i := 0; i < 2; i++ {
		w := call(1, "POST", "/search", `{"keywords":"fail"}`, "application/json", "", 502)
		if !strings.Contains(w.Body.String(), "SEARCH_UPSTREAM_FAILED") {
			t.Fatal(w.Body.String())
		}
	}
	if requests.Load() != before+2 {
		t.Fatal("failed searches cached")
	}
	ready := call(1, "GET", "/lyrics/netease?id=1", "", "", "", 200)
	if !strings.Contains(ready.Body.String(), `"state":"ready"`) {
		t.Fatal(ready.Body.String())
	}
	if w := call(1, "GET", "/lyrics/netease?ids=1", "", "", "", 200); w.Header().Get("X-Cache") != "HIT" {
		t.Fatal("lyric aliases not canonicalized")
	}
	before = requests.Load()
	for i := 0; i < 2; i++ {
		w := call(1, "GET", "/lyrics/netease?id=2", "", "", "", 200)
		if !strings.Contains(w.Body.String(), `"state":"unavailable"`) {
			t.Fatal(w.Body.String())
		}
	}
	if requests.Load() != before+2 {
		t.Fatal("ambiguous empty lyrics cached")
	}
	call(1, "GET", "/lyrics/netease?id=3", "", "", "", 200)
	if w := call(1, "GET", "/lyrics/netease?id=3", "", "", "", 200); w.Header().Get("X-Cache") != "HIT" || !strings.Contains(w.Body.String(), `"state":"empty"`) {
		t.Fatal(w.Body.String())
	}
	if w := call(1, "GET", "/lyrics/netease?id=4", "", "", "", 429); w.Header().Get("Retry-After") != "60" {
		t.Fatal(w.Header())
	}
	call(1, "GET", "/lyrics/netease?id=5", "", "", "", 502)
	call(1, "GET", "/lyrics/netease?id=1&id=2", "", "", "", 400)
	song := call(1, "POST", "/song", `{"ids":123,"level":"lossless","type":"json"}`, "application/json", "", 200)
	if !strings.Contains(song.Body.String(), `"url":"https://m10.music.126.net/music.flac"`) {
		t.Fatal(song.Body.String())
	}
	text := call(1, "POST", "/song", "ids=123&level=lossless&type=text", "application/x-www-form-urlencoded", "", 200)
	if strings.Contains(text.Body.String(), "<script>") || !strings.Contains(text.Body.String(), "&lt;script&gt;") {
		t.Fatal("unsafe HTML", text.Body.String())
	}
	if w := call(1, "POST", "/song", `{"url":"https://music.163.com/#/song?id=123","level":"lossless","type":"down"}`, "application/json", "", 302); w.Header().Get("Location") != "https://m10.music.126.net/music.flac" {
		t.Fatal(w.Header())
	}
	partial.Store(true)
	before = requests.Load()
	for i := 0; i < 2; i++ {
		w := call(1, "GET", "/playlist?id=99", "", "", "", 200)
		if !strings.Contains(w.Body.String(), `"complete":false`) || !strings.Contains(w.Body.String(), `"sourcePosition":1`) {
			t.Fatal(w.Body.String())
		}
	}
	if requests.Load() != before+4 {
		t.Fatal("partial playlist cached")
	}
	partial.Store(false)
	call(1, "GET", "/playlist?id=99", "", "", "", 200)
	if w := call(1, "GET", "/playlist?id=99", "", "", "", 200); w.Header().Get("X-Cache") != "HIT" {
		t.Fatal(w.Header())
	}
	call(1, "GET", "/album?id=7", "", "", "", 200)
	call(1, "GET", "/playlist?id=http%3A%2F%2F127.0.0.1%2Fsecret", "", "", "", 400)
	// Follow/sync call the actual platform adapter afresh, even when the read endpoint has a cached complete playlist.
	partial.Store(true)
	call(1, "POST", "/playlists/follow", `{"source":"netease","sourcePlaylistId":"99","operationId":"partial-follow"}`, "application/json", "", 422)
	var n int64
	db.ORM.Table("playlists").Count(&n)
	if n != 0 {
		t.Fatal("partial follow persisted")
	}
	partial.Store(false)
	followed := call(1, "POST", "/playlists/follow", `{"source":"netease","sourcePlaylistId":99,"operationId":"full-follow"}`, "application/json", "", 200)
	var result struct {
		Playlist struct {
			ID              int64
			TrackCount      int
			ContentRevision int64
		}
		Tracks []struct{ TrackID string }
	}
	if e := json.Unmarshal(followed.Body.Bytes(), &result); e != nil || result.Playlist.TrackCount != 2 {
		t.Fatal(followed.Body.String(), e)
	}
	base := fmt.Sprintf("/playlists/%d", result.Playlist.ID)
	members := call(1, "GET", base+"/tracks", "", "", "", 200)
	if e := json.Unmarshal(members.Body.Bytes(), &result); e != nil || len(result.Tracks) != 2 || result.Tracks[0].TrackID != "456" || result.Tracks[1].TrackID != "123" {
		t.Fatal(members.Body.String(), e)
	}
	before = requests.Load()
	call(1, "POST", "/playlists/follow", `{"source":"netease","sourcePlaylistId":99,"operationId":"full-follow"}`, "application/json", "", 200)
	if before != requests.Load() {
		t.Fatal("replayed follow contacted upstream")
	}
	partial.Store(true)
	call(1, "POST", base+"/sync", `{"operationId":"partial-sync"}`, "application/json", "", 422)
	copied := call(1, "POST", base+"/copy", "operationId=copy-old&name=Local", "application/x-www-form-urlencoded", "", 200)
	if e := json.Unmarshal(copied.Body.Bytes(), &result); e != nil || result.Playlist.TrackCount != 2 {
		t.Fatal(copied.Body.String(), e)
	}
	partial.Store(false)
	synced := call(1, "POST", base+"/sync", `{"operationId":"full-sync"}`, "application/json", "", 200)
	if e := json.Unmarshal(synced.Body.Bytes(), &result); e != nil || result.Playlist.ContentRevision != 2 {
		t.Fatal(synced.Body.String(), e)
	}
	// Authenticated music work gets its explicit business budget after bounded token lookup.
	r := httptest.NewRequest("GET", "/test-budget", nil)
	r.Header.Set("Authorization", "Bearer "+tokens[1])
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
