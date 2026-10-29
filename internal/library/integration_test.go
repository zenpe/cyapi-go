package library_test

import (
	"bytes"
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/library"
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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type playlistJSON struct {
	ID                                         int64
	Name, Type                                 string
	IsDefault                                  bool
	TrackCount, ContentRevision, OrderRevision int64
}
type trackJSON struct {
	library.Track
	ItemID  int64
	AddedAt string
}
type response struct {
	Status                         int
	Code                           any
	Playlist                       playlistJSON
	Playlists                      []playlistJSON
	Tracks                         []trackJSON
	Favorites                      []library.Track
	ContentRevision, OrderRevision int64
	ItemID                         int64
	Added                          bool
	AddedCount, RemovedCount       int
	InPlaylist                     bool
	Matches                        []struct{ PlaylistID, ItemID int64 }
}
type fixture struct {
	t       *testing.T
	db      *persistence.Database
	service *library.Service
	handler http.Handler
	tokens  map[int]string
}

func open(t *testing.T, driver, dsn string) *persistence.Database {
	t.Helper()
	d, e := persistence.Open(context.Background(), config.Database{Driver: driver, DSN: dsn, BusyTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func setup(t *testing.T, driver string) *fixture {
	t.Helper()
	var db *persistence.Database
	if driver == "sqlite" {
		db = open(t, driver, filepath.Join(t.TempDir(), "library.db"))
	} else {
		dsn := os.Getenv("CYAPI_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set CYAPI_TEST_POSTGRES_DSN for PostgreSQL")
		}
		u, e := url.Parse(dsn)
		if e != nil || u.Scheme != "postgres" {
			t.Fatal("invalid test DSN")
		}
		admin := open(t, driver, dsn)
		schema := fmt.Sprintf("cyapi_library_%d", time.Now().UnixNano())
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
		db = open(t, driver, u.String())
	}
	if e := db.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	for id := 1; id <= 2; id++ {
		if e := db.ORM.Exec("INSERT INTO users(id,email,username,password_hash) VALUES (?, ?, ?, ?)", id, fmt.Sprintf("12345%d@qq.com", id), fmt.Sprintf("user%d", id), "unused-in-token-tests").Error; e != nil {
			t.Fatal(e)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	secret := strings.Repeat("L", 32)
	accounts, e := auth.New(authstore.New(db.ORM), nil, auth.Options{Secret: secret, TokenMaxAge: time.Hour, BcryptCost: 4, Logger: log})
	if e != nil {
		t.Fatal(e)
	}
	authRoutes := auth.NewHandler(accounts, server.ClientIP, log)
	service := library.New(librarystore.New(db.ORM))
	routes := library.NewHandler(service, authRoutes.Protect, log)
	handler := server.NewHandler(config.Defaults().Server, db, log, authRoutes.Register, routes.Register)
	signer, _ := auth.NewSigner(secret, time.Hour)
	tokens := map[int]string{}
	for id := 1; id <= 2; id++ {
		tokens[id], e = signer.Issue(int64(id), 0, time.Now())
		if e != nil {
			t.Fatal(e)
		}
	}
	return &fixture{t, db, service, handler, tokens}
}
func (f *fixture) request(user int, method, path string, body any, want int) response {
	f.t.Helper()
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			f.t.Fatal(e)
		}
	}
	w := f.raw(user, method, path, string(data), "application/json")
	if w.Code != want {
		f.t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	var v response
	if e = json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		f.t.Fatal(e)
	}
	if user != 0 && w.Header().Get("Cache-Control") != "private, no-store" {
		f.t.Fatal("cacheable response")
	}
	return v
}
func (f *fixture) raw(user int, method, path, body, media string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.20:1234"
	r.Header.Set("Content-Type", media)
	if user != 0 {
		r.Header.Set("Authorization", "Bearer "+f.tokens[user])
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func track(id string) map[string]any {
	return map[string]any{"trackId": id, "name": "song-" + id, "artists": "artist", "album": "album", "picUrl": "https://example.com/cover.jpg", "source": "netease"}
}
func TestLibraryHTTPContract(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver)
			f.request(0, "GET", "/playlists", nil, 401)
			f.request(0, "POST", "/favorites", track("a"), 401)
			defaults := f.request(1, "GET", "/playlists", nil, 200)
			if len(defaults.Playlists) != 1 || !defaults.Playlists[0].IsDefault {
				t.Fatal(defaults)
			}
			defaultID := defaults.Playlists[0].ID
			f.request(1, "GET", "/playlists", nil, 200)
			f.request(1, "PUT", fmt.Sprintf("/playlists/%d", defaultID), map[string]any{"name": "rename"}, 404)
			f.request(1, "DELETE", fmt.Sprintf("/playlists/%d", defaultID), nil, 404)
			a := track("a")
			a["sourceIds"] = map[string]any{"fileHash": " hash ", "emixSongId": "123", "ignored": "x"}
			a["userId"] = 2
			f.request(1, "POST", "/favorites", a, 200)
			f.request(1, "POST", "/favorites", a, 400)
			favs := f.request(1, "GET", "/favorites", nil, 200)
			if len(favs.Favorites) != 1 || favs.Favorites[0].SourceIDs["fileHash"] != "hash" || len(favs.Favorites[0].SourceIDs) != 2 {
				t.Fatal(favs)
			}
			if len(f.request(2, "GET", "/favorites", nil, 200).Favorites) != 0 {
				t.Fatal("favorite leaked")
			}
			f.request(2, "DELETE", "/favorites/a/netease", nil, 404)
			f.request(1, "DELETE", "/favorites/a/netease", nil, 200)
			f.request(1, "DELETE", "/favorites/a/netease", nil, 404)
			created := f.request(1, "POST", "/playlists", map[string]any{"name": "My list", "operationId": "create", "userId": 2}, 200)
			id := created.Playlist.ID
			base := fmt.Sprintf("/playlists/%d", id)
			replay := f.request(1, "POST", "/playlists", map[string]any{"name": "ignored retry name", "operationId": "create"}, 200)
			if replay.Playlist.ID != id || replay.Playlist.Name != "My list" {
				t.Fatal("create replay", replay)
			}
			other := f.request(2, "POST", "/playlists", map[string]any{"name": "Other", "operationId": "create"}, 200)
			if other.Playlist.ID == id {
				t.Fatal("cross-user operation replay")
			}
			// All reads and writes are scoped to the signed user, regardless of supplied body fields.
			f.request(2, "GET", base+"/tracks", nil, 404)
			f.request(2, "PUT", base, map[string]any{"name": "stolen"}, 404)
			f.request(2, "DELETE", base, nil, 404)
			bad := track("foreign")
			bad["operationId"] = "foreign"
			f.request(2, "POST", base+"/tracks", bad, 404)
			a["operationId"] = "add-a"
			added := f.request(1, "POST", base+"/tracks", a, 200)
			if !added.Added || added.ItemID == 0 || added.Playlist.TrackCount != 1 || added.Playlist.OrderRevision != 1 {
				t.Fatal(added)
			}
			alphaID := added.ItemID
			if retry := f.request(1, "POST", base+"/tracks", a, 200); retry.ItemID != alphaID || !retry.Added || retry.Playlist.OrderRevision != 1 {
				t.Fatal("add replay", retry)
			}
			a["operationId"] = "duplicate-a"
			dupe := f.request(1, "POST", base+"/tracks", a, 200)
			if dupe.Added || dupe.Playlist.ContentRevision != 1 {
				t.Fatal("duplicate revised playlist", dupe)
			}
			a["operationId"] = "create"
			if v := f.request(1, "POST", base+"/tracks", a, 409); v.Code != "OPERATION_SCOPE_MISMATCH" {
				t.Fatal(v)
			}
			batch := f.request(1, "POST", base+"/tracks/batch", map[string]any{"operationId": "batch", "tracks": []any{track("b"), track("c"), track("b"), track("a")}}, 200)
			if batch.AddedCount != 2 || batch.Playlist.OrderRevision != 2 || batch.Playlist.TrackCount != 3 {
				t.Fatal(batch)
			}
			tracks := f.request(1, "GET", base+"/tracks", nil, 200)
			if len(tracks.Tracks) != 3 || tracks.Tracks[0].TrackID != "b" || tracks.Tracks[1].TrackID != "c" || tracks.Tracks[2].ItemID != alphaID || tracks.Tracks[2].SourceIDs["fileHash"] != "hash" {
				t.Fatal(tracks)
			}
			betaID, gammaID := tracks.Tracks[0].ItemID, tracks.Tracks[1].ItemID
			f.request(2, "DELETE", fmt.Sprintf("%s/items/%d", base, alphaID), map[string]any{"operationId": "foreign-remove"}, 404)
			replaced := f.request(1, "POST", fmt.Sprintf("%s/items/%d/replace", base, betaID), map[string]any{"operationId": "replace", "track": track("d")}, 200)
			if replaced.ItemID != betaID || replaced.Playlist.OrderRevision != 2 || replaced.Playlist.ContentRevision != 3 {
				t.Fatal(replaced)
			}
			conflict := track("c")
			conflict["name"] = "must roll back this metadata"
			f.request(1, "POST", fmt.Sprintf("%s/items/%d/replace", base, alphaID), map[string]any{"operationId": "replace-conflict", "track": conflict}, 409)
			tracks = f.request(1, "GET", base+"/tracks", nil, 200)
			if tracks.Tracks[1].Name != "song-c" {
				t.Fatal("failed replacement leaked metadata")
			}
			reordered := f.request(1, "POST", base+"/items/reorder", map[string]any{"operationId": "order", "itemIds": []int64{alphaID, gammaID, betaID}, "expectedOrderRevision": 2}, 200)
			if reordered.Playlist.OrderRevision != 3 || reordered.Playlist.ContentRevision != 3 {
				t.Fatal(reordered)
			}
			tracks = f.request(1, "GET", base+"/tracks", nil, 200)
			if tracks.Tracks[0].ItemID != alphaID || tracks.Tracks[2].ItemID != betaID {
				t.Fatal("rank not applied", tracks)
			}
			stale := map[string]any{"operationId": "stale", "itemIds": []int64{betaID, gammaID, alphaID}, "expectedOrderRevision": 2}
			if v := f.request(1, "POST", base+"/items/reorder", stale, 409); v.Code != "REVISION_CONFLICT" {
				t.Fatal(v)
			}
			f.request(1, "POST", base+"/items/reorder", map[string]any{"operationId": "partial", "itemIds": []int64{alphaID}, "expectedOrderRevision": 3}, 409)
			f.request(1, "POST", base+"/items/reorder", map[string]any{"operationId": "dupes", "itemIds": []int64{alphaID, alphaID}, "expectedOrderRevision": 3}, 400)
			removed := f.request(1, "DELETE", fmt.Sprintf("%s/items/%d", base, gammaID), map[string]any{"operationId": "remove"}, 200)
			if removed.Playlist.TrackCount != 2 || removed.Playlist.OrderRevision != 4 {
				t.Fatal(removed)
			}
			removed = f.request(1, "POST", base+"/items/batch-remove", map[string]any{"operationId": "batch-remove", "itemIds": []int64{betaID, betaID, 999999}}, 200)
			if removed.RemovedCount != 1 || removed.Playlist.OrderRevision != 5 || removed.Playlist.TrackCount != 1 {
				t.Fatal(removed)
			}
			checked := f.request(1, "GET", "/playlists/check-track?trackId=a&source=netease", nil, 200)
			if !checked.InPlaylist || len(checked.Matches) != 1 || checked.Matches[0].PlaylistID != id {
				t.Fatal(checked)
			}
			if f.request(2, "GET", "/playlists/check-track?trackId=a&source=netease", nil, 200).InPlaylist {
				t.Fatal("membership leaked")
			}
			renamed := f.request(1, "PUT", base, map[string]any{"name": "Renamed"}, 200)
			if renamed.Playlist.Name != "Renamed" {
				t.Fatal(renamed)
			}
			f.request(1, "POST", base+"/delete", nil, 200)
			f.request(1, "GET", base+"/tracks", nil, 404)
			var count int64
			if e := f.db.ORM.Table("local_playlist_items").Where("playlist_id = ?", id).Count(&count).Error; e != nil || count != 0 {
				t.Fatal("cascade failed", count, e)
			}
			// Form body compatibility, including nested JSON fields and integer track IDs.
			w := f.raw(1, "POST", "/playlists", "name=Form&operationId=form", "application/x-www-form-urlencoded")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var form response
			json.Unmarshal(w.Body.Bytes(), &form)
			body := url.Values{"operationId": {"form-track"}, "track": {`{"trackId":123,"source":"qq","name":"form song"}`}}
			w = f.raw(1, "POST", fmt.Sprintf("/playlists/%d/tracks", form.Playlist.ID), body.Encode(), "application/x-www-form-urlencoded")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			for _, body := range []string{`null`, `{} {}`, `{"name":true}`, strings.Repeat("x", (1<<20)+1)} {
				w = f.raw(1, "POST", "/playlists", body, "application/json")
				if w.Code != 400 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			f.request(1, "GET", "/playlists/0/tracks", nil, 400)
			f.request(1, "GET", "/playlists/9007199254740992/tracks", nil, 400)
			if e := f.db.ORM.Exec("INSERT INTO playlists(user_id,name,type,source,source_playlist_id) VALUES (?, ?, ?, ?, ?)", 1, "Remote", "synced", "netease", "remote-id").Error; e != nil {
				t.Fatal(e)
			}
			var syncedID int64
			f.db.ORM.Table("playlists").Select("id").Where("type = ?", "synced").Scan(&syncedID)
			remote := track("x")
			remote["operationId"] = "readonly"
			if v := f.request(1, "POST", fmt.Sprintf("/playlists/%d/tracks", syncedID), remote, 409); v.Code != "SYNCED_PLAYLIST_READ_ONLY" {
				t.Fatal(v)
			}
		})
	}
}
func TestLibraryConcurrency(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver)
			ctx := context.Background()
			var wg sync.WaitGroup
			results := make(chan library.Result, 12)
			errs := make(chan error, 12)
			// Same operation concurrently must create one playlist and replay the same result.
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := f.service.Write(ctx, 1, library.Command{Kind: "create", OperationID: "same", Name: "Concurrent"})
					if e != nil {
						errs <- e
					} else {
						results <- v
					}
				}()
			}
			wg.Wait()
			close(results)
			close(errs)
			for e := range errs {
				t.Error(e)
			}
			var first string
			for v := range results {
				b, _ := json.Marshal(v)
				if first == "" {
					first = string(b)
				} else if string(b) != first {
					t.Fatal("replay differed", first, string(b))
				}
			}
			var id int64
			if e := f.db.ORM.Table("playlists").Select("id").Where("user_id = ?", 1).Scan(&id).Error; e != nil {
				t.Fatal(e)
			}
			// Different operations adding the same track must increment revisions only once.
			results = make(chan library.Result, 12)
			errs = make(chan error, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					v, e := f.service.Write(ctx, 1, library.Command{Kind: "add", PlaylistID: id, OperationID: fmt.Sprintf("add-%d", i), Track: library.Track{TrackID: "a", Name: "song", Source: "qq"}})
					if e != nil {
						errs <- e
					} else {
						results <- v
					}
				}(i)
			}
			wg.Wait()
			close(results)
			close(errs)
			for e := range errs {
				t.Error(e)
			}
			added := 0
			for v := range results {
				if v.Status != 200 {
					t.Fatal(v)
				}
				if v.Body["added"] == true {
					added++
				}
			}
			if added != 1 {
				t.Fatal("added", added)
			}
			tracks, e := f.service.Read(ctx, 1, library.Query{Kind: "tracks", PlaylistID: id})
			if e != nil {
				t.Fatal(e)
			}
			data, _ := json.Marshal(tracks.Body)
			var snapshot response
			json.Unmarshal(data, &snapshot)
			if len(snapshot.Tracks) != 1 || snapshot.OrderRevision != 1 || snapshot.ContentRevision != 1 {
				t.Fatal(snapshot)
			}
			results = make(chan library.Result, 2)
			errs = make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					v, e := f.service.Write(ctx, 1, library.Command{Kind: "reorder", PlaylistID: id, OperationID: fmt.Sprintf("order-%d", i), ItemIDs: []int64{snapshot.Tracks[0].ItemID}, ExpectedOrderRevision: 1})
					if e != nil {
						errs <- e
					} else {
						results <- v
					}
				}(i)
			}
			wg.Wait()
			close(results)
			close(errs)
			for e := range errs {
				t.Error(e)
			}
			statuses := map[int]int{}
			for v := range results {
				statuses[v.Status]++
			}
			if statuses[200] != 1 || statuses[409] != 1 {
				t.Fatal("lost update", statuses)
			}
		})
	}
}
func TestBatchFailureRollsBack(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver)
			created := f.request(1, "POST", "/playlists", map[string]any{"name": "Atomic", "operationId": "atomic-create"}, 200)
			id := created.Playlist.ID
			// Inject a DB failure after the first member insert, inside the same batch.
			if driver == "sqlite" {
				if e := f.db.ORM.Exec("CREATE TRIGGER reject_bad_track BEFORE INSERT ON local_playlist_items WHEN (SELECT source_track_id FROM playlist_tracks_catalog WHERE id = NEW.track_id) = 'bad' BEGIN SELECT RAISE(ABORT, 'injected private database detail'); END").Error; e != nil {
					t.Fatal(e)
				}
			} else {
				if e := f.db.ORM.Exec(`CREATE FUNCTION reject_bad_track() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF (SELECT source_track_id FROM playlist_tracks_catalog WHERE id = NEW.track_id) = 'bad' THEN RAISE EXCEPTION 'injected private database detail'; END IF; RETURN NEW; END $$`).Error; e != nil {
					t.Fatal(e)
				}
				if e := f.db.ORM.Exec("CREATE TRIGGER reject_bad_track BEFORE INSERT ON local_playlist_items FOR EACH ROW EXECUTE FUNCTION reject_bad_track()").Error; e != nil {
					t.Fatal(e)
				}
			}
			body := map[string]any{"operationId": "atomic-batch", "tracks": []any{track("good"), track("bad")}}
			data, _ := json.Marshal(body)
			w := f.raw(1, "POST", fmt.Sprintf("/playlists/%d/tracks/batch", id), string(data), "application/json")
			if w.Code != 500 || bytes.Contains(w.Body.Bytes(), []byte("private database detail")) {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, table := range []string{"local_playlist_items", "playlist_tracks_catalog"} {
				var count int64
				if e := f.db.ORM.Table(table).Count(&count).Error; e != nil || count != 0 {
					t.Fatal("partial batch persisted", table, count, e)
				}
			}
			var count int64
			f.db.ORM.Table("playlist_operations").Where("operation_id = ?", "atomic-batch").Count(&count)
			if count != 0 {
				t.Fatal("failed infrastructure operation cached")
			}
			v := f.request(1, "GET", fmt.Sprintf("/playlists/%d/tracks", id), nil, 200)
			if v.ContentRevision != 0 || v.OrderRevision != 0 {
				t.Fatal(v)
			}
			// Infrastructure failures are retryable using the original operationId.
			if driver == "sqlite" {
				f.db.ORM.Exec("DROP TRIGGER reject_bad_track")
			} else {
				f.db.ORM.Exec("DROP TRIGGER reject_bad_track ON local_playlist_items")
			}
			f.request(1, "POST", fmt.Sprintf("/playlists/%d/tracks/batch", id), body, 200)
		})
	}
}

// This crosses both rank-update chunks and exercises the largest allowed batch.
func TestLargeBatchAndReorder(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver)
			ctx := context.Background()
			create, e := f.service.Write(ctx, 1, library.Command{Kind: "create", OperationID: "large-create", Name: "Large"})
			if e != nil {
				t.Fatal(e)
			}
			encoded, _ := json.Marshal(create.Body)
			var initial response
			json.Unmarshal(encoded, &initial)
			id := initial.Playlist.ID
			tracks := make([]library.Track, library.MaxBatch)
			for i := range tracks {
				tracks[i] = library.Track{TrackID: fmt.Sprintf("track-%03d", i), Name: "Song", Source: "qq"}
			}
			added, e := f.service.Write(ctx, 1, library.Command{Kind: "batch-add", PlaylistID: id, OperationID: "large-add", Tracks: tracks})
			if e != nil || added.Status != 200 {
				t.Fatal(added, e)
			}
			read := f.request(1, "GET", fmt.Sprintf("/playlists/%d/tracks", id), nil, 200)
			if len(read.Tracks) != library.MaxBatch {
				t.Fatal(len(read.Tracks))
			}
			ids := make([]int64, len(read.Tracks))
			for i, v := range read.Tracks {
				ids[len(ids)-1-i] = v.ItemID
			}
			reordered, e := f.service.Write(ctx, 1, library.Command{Kind: "reorder", PlaylistID: id, OperationID: "large-order", ItemIDs: ids, ExpectedOrderRevision: 1})
			if e != nil || reordered.Status != 200 {
				t.Fatal(reordered, e)
			}
			read = f.request(1, "GET", fmt.Sprintf("/playlists/%d/tracks", id), nil, 200)
			for i, v := range read.Tracks {
				if v.ItemID != ids[i] {
					t.Fatal("chunk rank mismatch at", i)
				}
			}
			if _, e = f.service.Write(ctx, 1, library.Command{Kind: "batch-add", PlaylistID: id, OperationID: "too-many", Tracks: append(tracks, tracks[0])}); e == nil {
				t.Fatal("oversized batch accepted")
			}
			// Expired operations permit a fresh execution with the same operationId.
			if e = f.db.ORM.Exec("UPDATE playlist_operations SET expires_at = ? WHERE user_id = ? AND operation_id = ?", "2000-01-01T00:00:00.000000000Z", 1, "large-create").Error; e != nil {
				t.Fatal(e)
			}
			renewed, e := f.service.Write(ctx, 1, library.Command{Kind: "create", OperationID: "large-create", Name: "Renewed"})
			if e != nil || renewed.Status != 200 {
				t.Fatal(renewed, e)
			}
			encoded, _ = json.Marshal(renewed.Body)
			var fresh response
			json.Unmarshal(encoded, &fresh)
			if fresh.Playlist.ID == id || fresh.Playlist.Name != "Renewed" {
				t.Fatal("expired operation replayed", fresh)
			}
		})
	}
}
