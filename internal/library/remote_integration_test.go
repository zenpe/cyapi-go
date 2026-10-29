package library_test

import (
	"context"
	"cyapi-go/internal/library"
	"cyapi-go/internal/persistence/librarystore"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type snapshotFunc func(context.Context, string, string) (library.RemoteSnapshot, error)

func (f snapshotFunc) FetchSnapshot(ctx context.Context, source, id string) (library.RemoteSnapshot, error) {
	return f(ctx, source, id)
}
func remoteSnapshot(ids ...string) library.RemoteSnapshot {
	s := library.RemoteSnapshot{Name: "Remote list", ExpectedCount: len(ids), Complete: true, Tracks: []library.RemoteTrack{}}
	for i, id := range ids {
		s.Tracks = append(s.Tracks, library.RemoteTrack{Track: library.Track{TrackID: id, Name: "song-" + id, Artists: "artist", Source: "netease"}, SourcePosition: i})
	}
	return s
}
func follow(op string) map[string]any {
	return map[string]any{"source": "netease", "sourcePlaylistId": "42", "operationId": op}
}
func remoteCall(t *testing.T, f *fixture, user int64, c library.Command, want int) library.Result {
	t.Helper()
	r, e := f.service.Remote(context.Background(), user, c)
	if e != nil {
		var failure *library.Failure
		if !errors.As(e, &failure) {
			t.Fatal(e)
		}
		r = library.Rejected(failure)
	}
	if r.Status != want {
		t.Fatalf("remote %s: %+v want %d", c.Kind, r, want)
	}
	return r
}
func TestRemotePlaylistHTTP(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			var calls atomic.Int32
			var fail atomic.Bool
			f := setup(t, driver, snapshotFunc(func(ctx context.Context, source, id string) (library.RemoteSnapshot, error) {
				calls.Add(1)
				if source != "netease" || id != "42" {
					t.Errorf("bad binding: %s %s", source, id)
				}
				if fail.Load() {
					return library.RemoteSnapshot{}, errors.New("secret upstream cookie")
				}
				return remoteSnapshot("2", "1", "2"), nil
			}))
			f.request(0, "POST", "/playlists/follow", follow("x"), 401)
			f.request(1, "POST", "/playlists/follow", map[string]any{"operationId": "x", "source": "kuwo", "sourcePlaylistId": "42"}, 400)
			f.request(1, "POST", "/playlists/follow", map[string]any{"operationId": "x", "source": "netease", "sourcePlaylistId": "https://evil.invalid/"}, 400)
			created := f.request(1, "POST", "/playlists/follow", follow("follow"), 200)
			id := created.Playlist.ID
			base := fmt.Sprintf("/playlists/%d", id)
			if created.Playlist.Type != "synced" || created.Playlist.TrackCount != 3 || created.Playlist.ContentRevision != 1 {
				t.Fatal(created)
			}
			replay := f.request(1, "POST", "/playlists/follow", follow("follow"), 200)
			if replay.Playlist.ID != id || calls.Load() != 1 {
				t.Fatal("follow not replayed")
			}
			f.request(1, "POST", "/playlists/follow", follow("duplicate"), 409)
			if calls.Load() != 1 {
				t.Fatal("duplicate fetched")
			}
			tracks := f.request(1, "GET", base+"/tracks", nil, 200).Tracks
			if len(tracks) != 3 || tracks[0].TrackID != "2" || tracks[1].TrackID != "1" || tracks[2].TrackID != "2" || tracks[0].ItemID != 0 {
				t.Fatal(tracks)
			}
			f.request(2, "POST", base+"/sync", map[string]any{"operationId": "foreign"}, 404)
			f.request(2, "POST", base+"/copy", map[string]any{"operationId": "foreign-copy"}, 404)
			if calls.Load() != 1 {
				t.Fatal("unauthorized upstream call")
			}
			f.request(1, "POST", base+"/tracks", map[string]any{"operationId": "readonly", "track": track("3")}, 409)
			copied := f.request(1, "POST", base+"/copy", map[string]any{"operationId": "copy"}, 200)
			if copied.Playlist.Type != "local" || copied.Playlist.TrackCount != 2 {
				t.Fatal(copied)
			}
			copyBase := fmt.Sprintf("/playlists/%d", copied.Playlist.ID)
			copyTracks := f.request(1, "GET", copyBase+"/tracks", nil, 200).Tracks
			if len(copyTracks) != 2 || copyTracks[0].TrackID != "2" || copyTracks[1].TrackID != "1" || copyTracks[0].ItemID == 0 {
				t.Fatal(copyTracks)
			}
			again := f.request(1, "POST", base+"/copy", map[string]any{"operationId": "copy"}, 200)
			if again.Playlist.ID != copied.Playlist.ID {
				t.Fatal("duplicate copy")
			}
			f.request(1, "POST", copyBase+"/tracks", map[string]any{"operationId": "edit-copy", "track": track("3")}, 200)
			f.request(1, "POST", copyBase+"/sync", map[string]any{"operationId": "local-sync"}, 409)
			f.request(1, "POST", copyBase+"/copy", map[string]any{"operationId": "local-copy"}, 409)
			fail.Store(true)
			failed := f.raw(1, "POST", base+"/sync", `{"operationId":"failed"}`, "application/json")
			if failed.Code != 422 || strings.Contains(failed.Body.String(), "secret") {
				t.Fatal(failed.Body.String())
			}
			old := f.request(1, "GET", base+"/tracks", nil, 200)
			if len(old.Tracks) != 3 || old.ContentRevision != 1 {
				t.Fatal(old)
			}
			var state string
			if e := f.db.ORM.Raw("SELECT sync_status FROM playlists WHERE id = ?", id).Scan(&state).Error; e != nil || state != "error" {
				t.Fatal(state, e)
			}
			fail.Store(false)
			f.request(1, "POST", base+"/sync", map[string]any{"operationId": "failed"}, 422)
			if calls.Load() != 2 {
				t.Fatal("failure not replayed")
			}
			for i := 0; i < 3; i++ {
				r := f.request(1, "POST", base+"/sync", map[string]any{"operationId": fmt.Sprint("refresh", i)}, 200)
				if r.Playlist.ContentRevision != int64(i+2) {
					t.Fatal(r)
				}
			}
			var snapshots int64
			f.db.ORM.Table("playlist_snapshots").Where("playlist_id = ?", id).Count(&snapshots)
			if snapshots != 2 {
				t.Fatal("unbounded snapshots", snapshots)
			}
			f.request(1, "POST", base+"/sync", map[string]any{"operationId": "copy"}, 409)
			f.request(1, "DELETE", base, nil, 200)
			f.request(1, "GET", base+"/tracks", nil, 404)
			f.db.ORM.Table("playlist_snapshots").Where("playlist_id = ?", id).Count(&snapshots)
			if snapshots != 0 {
				t.Fatal("snapshot cascade", snapshots)
			}
		})
	}
}
func TestRemoteSnapshotValidation(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			snapshot := remoteSnapshot("1")
			f := setup(t, driver, snapshotFunc(func(context.Context, string, string) (library.RemoteSnapshot, error) { return snapshot, nil }))
			for i, mutate := range []func(){func() { snapshot.Complete = false }, func() { snapshot = remoteSnapshot("1"); snapshot.ExpectedCount = 2 }, func() { snapshot = remoteSnapshot("1"); snapshot.Tracks[0].SourcePosition = 2 }, func() { snapshot = remoteSnapshot("1"); snapshot.Tracks[0].Source = "qq" }, func() { snapshot = remoteSnapshot("1"); snapshot.Tracks[0].TrackID = "bad" }, func() { snapshot = remoteSnapshot("1"); snapshot.Name = "" }} {
				mutate()
				f.request(1, "POST", "/playlists/follow", follow(fmt.Sprint("bad", i)), 422)
			}
			var count int64
			f.db.ORM.Table("playlists").Count(&count)
			if count != 0 {
				t.Fatal("partial follow visible", count)
			}
			snapshot = remoteSnapshot()
			p := f.request(1, "POST", "/playlists/follow", follow("empty"), 200)
			if p.Playlist.TrackCount != 0 {
				t.Fatal(p)
			}
			f.request(1, "POST", fmt.Sprintf("/playlists/%d/copy", p.Playlist.ID), map[string]any{"operationId": "empty-copy"}, 200)
		})
	}
}
func TestRemoteConcurrentAndDelete(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			var block atomic.Bool
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			f := setup(t, driver, snapshotFunc(func(ctx context.Context, _ string, _ string) (library.RemoteSnapshot, error) {
				if block.Load() {
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return library.RemoteSnapshot{}, ctx.Err()
					}
				}
				return remoteSnapshot("1"), nil
			}))
			p := f.request(1, "POST", "/playlists/follow", follow("create"), 200)
			base := fmt.Sprintf("/playlists/%d", p.Playlist.ID)
			block.Store(true)
			done := make(chan library.Result, 1)
			go func() {
				r, e := f.service.Remote(context.Background(), 1, library.Command{Kind: "sync", PlaylistID: p.Playlist.ID, OperationID: "slow"})
				if e != nil {
					r = library.Rejected(library.Fail(500, "FAILED", ""))
				}
				done <- r
			}()
			<-entered
			same := remoteCall(t, f, 1, library.Command{Kind: "sync", PlaylistID: p.Playlist.ID, OperationID: "slow"}, 409)
			if same.Body["code"] != "OPERATION_PENDING" {
				t.Fatal(same)
			}
			other := remoteCall(t, f, 1, library.Command{Kind: "sync", PlaylistID: p.Playlist.ID, OperationID: "other"}, 409)
			if other.Body["code"] != "SYNC_IN_PROGRESS" {
				t.Fatal(other)
			}
			// Reads and unrelated local writes complete while upstream remains blocked.
			f.request(1, "GET", base+"/tracks", nil, 200)
			f.request(1, "POST", "/playlists", map[string]any{"name": "unblocked", "operationId": "local"}, 200)
			f.request(1, "DELETE", base, nil, 200)
			close(release)
			select {
			case r := <-done:
				if r.Status != 404 {
					t.Fatal(r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("completion blocked")
			}
			var n int64
			f.db.ORM.Table("playlist_snapshots").Count(&n)
			if n != 0 {
				t.Fatal("deleted playlist resurrected")
			}
		})
	}
}
func TestRemoteStaleLeaseFencing(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver, snapshotFunc(func(context.Context, string, string) (library.RemoteSnapshot, error) { return remoteSnapshot("1"), nil }))
			p := f.request(1, "POST", "/playlists/follow", follow("create"), 200)
			store := librarystore.New(f.db.ORM)
			ctx := context.Background()
			c := library.Command{Kind: "sync", PlaylistID: p.Playlist.ID, OperationID: "abandoned"}
			old, _, e := store.BeginRemote(ctx, 1, c)
			if e != nil {
				t.Fatal(e)
			}
			stale := time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
			f.db.ORM.Exec("UPDATE playlists SET updated_at = ? WHERE id = ?", stale, p.Playlist.ID)
			f.db.ORM.Exec("UPDATE playlist_operations SET expires_at = ? WHERE user_id = 1 AND operation_id = ?", stale, c.OperationID)
			fresh, _, e := store.BeginRemote(ctx, 1, c)
			if e != nil {
				t.Fatal(e)
			}
			if fresh.SyncRevision <= old.SyncRevision {
				t.Fatal("missing fence")
			}
			r, e := store.FinishRemote(ctx, 1, old, remoteSnapshot("9"), nil)
			if e == nil || r.Status == 200 {
				t.Fatal("old task activated", r, e)
			}
			r, e = store.FinishRemote(ctx, 1, fresh, remoteSnapshot("2"), nil)
			if e != nil || r.Status != 200 {
				t.Fatal(r, e)
			}
			tracks := f.request(1, "GET", fmt.Sprintf("/playlists/%d/tracks", p.Playlist.ID), nil, 200).Tracks
			if len(tracks) != 1 || tracks[0].TrackID != "2" {
				t.Fatal(tracks)
			}
		})
	}
}
func TestRemoteStorageFailureRollback(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver, snapshotFunc(func(context.Context, string, string) (library.RemoteSnapshot, error) {
				return remoteSnapshot("2", "1", "2"), nil
			}))
			p := f.request(1, "POST", "/playlists/follow", follow("create"), 200)
			if driver == "sqlite" {
				if e := f.db.ORM.Exec("CREATE TRIGGER fail_snapshot BEFORE INSERT ON playlist_snapshot_items BEGIN SELECT RAISE(ABORT, 'forced'); END").Error; e != nil {
					t.Fatal(e)
				}
			} else {
				if e := f.db.ORM.Exec("CREATE FUNCTION fail_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced'; END $$").Error; e != nil {
					t.Fatal(e)
				}
				if e := f.db.ORM.Exec("CREATE TRIGGER fail_snapshot BEFORE INSERT ON playlist_snapshot_items FOR EACH ROW EXECUTE FUNCTION fail_snapshot()").Error; e != nil {
					t.Fatal(e)
				}
			}
			base := fmt.Sprintf("/playlists/%d", p.Playlist.ID)
			f.request(1, "POST", base+"/sync", map[string]any{"operationId": "retry"}, 500)
			before := f.request(1, "GET", base+"/tracks", nil, 200)
			if len(before.Tracks) != 3 || before.ContentRevision != 1 {
				t.Fatal(before)
			}
			var n int64
			f.db.ORM.Table("playlist_snapshots").Count(&n)
			if n != 1 {
				t.Fatal("leaked failed snapshot", n)
			}
			f.db.ORM.Table("playlist_operations").Where("operation_id = ?", "retry").Count(&n)
			if n != 0 {
				t.Fatal("operation not retryable")
			}
			sql := "DROP TRIGGER fail_snapshot"
			if driver == "postgres" {
				sql += " ON playlist_snapshot_items"
			}
			if e := f.db.ORM.Exec(sql).Error; e != nil {
				t.Fatal(e)
			}
			f.request(1, "POST", base+"/sync", map[string]any{"operationId": "retry"}, 200)
		})
	}
}
func TestRemoteCancellation(t *testing.T) {
	var wait atomic.Bool
	entered := make(chan struct{}, 1)
	f := setup(t, "sqlite", snapshotFunc(func(ctx context.Context, _ string, _ string) (library.RemoteSnapshot, error) {
		if wait.Load() {
			entered <- struct{}{}
			<-ctx.Done()
			return library.RemoteSnapshot{}, ctx.Err()
		}
		return remoteSnapshot("1"), nil
	}))
	p := f.request(1, "POST", "/playlists/follow", follow("create"), 200)
	wait.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan library.Result, 1)
	go func() {
		r, _ := f.service.Remote(ctx, 1, library.Command{Kind: "sync", PlaylistID: p.Playlist.ID, OperationID: "cancelled"})
		done <- r
	}()
	<-entered
	cancel()
	r := <-done
	if r.Status != 422 {
		t.Fatal(r)
	}
	var status string
	f.db.ORM.Raw("SELECT sync_status FROM playlists WHERE id = ?", p.Playlist.ID).Scan(&status)
	if status != "error" {
		t.Fatal("lease retained", status)
	}
}

func TestRemoteConcurrentFollow(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			f := setup(t, driver, snapshotFunc(func(ctx context.Context, _ string, _ string) (library.RemoteSnapshot, error) {
				entered <- struct{}{}
				select {
				case <-release:
					return remoteSnapshot("1"), nil
				case <-ctx.Done():
					return library.RemoteSnapshot{}, ctx.Err()
				}
			}))
			done := make(chan int, 2)
			for i := 0; i < 2; i++ {
				go func(i int) {
					r, e := f.service.Remote(context.Background(), 1, library.Command{Kind: "follow", Source: "netease", SourcePlaylistID: "42", OperationID: fmt.Sprint("follow", i)})
					if e != nil {
						var fail *library.Failure
						if errors.As(e, &fail) {
							done <- fail.Status
						} else {
							done <- 500
						}
						return
					}
					done <- r.Status
				}(i)
			}
			<-entered
			<-entered
			close(release)
			a, b := <-done, <-done
			if !((a == 200 && b == 409) || (a == 409 && b == 200)) {
				t.Fatal(a, b)
			}
			var n int64
			f.db.ORM.Table("playlists").Count(&n)
			if n != 1 {
				t.Fatal(n)
			}
			f.db.ORM.Table("playlist_snapshots").Count(&n)
			if n != 1 {
				t.Fatal(n)
			}
		})
	}
}
func TestRemoteMaximumSnapshotAndCleanup(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ids := make([]string, library.MaxItems)
			for i := range ids {
				ids[i] = fmt.Sprint(i + 1)
			}
			f := setup(t, driver, snapshotFunc(func(context.Context, string, string) (library.RemoteSnapshot, error) {
				return remoteSnapshot(ids...), nil
			}))
			expired := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
			for i := 0; i < 101; i++ {
				if e := f.db.ORM.Exec("INSERT INTO playlist_operations(user_id,operation_id,scope,status,created_at,expires_at) VALUES (1,?,?, 'pending',?,?)", fmt.Sprint("expired", i), "test", expired, expired).Error; e != nil {
					t.Fatal(e)
				}
			}
			began := time.Now()
			p := f.request(1, "POST", "/playlists/follow", follow("large"), 200)
			t.Logf("%s 10000-track follow took %s", driver, time.Since(began))
			if p.Playlist.TrackCount != library.MaxItems {
				t.Fatal(p)
			}
			var n int64
			f.db.ORM.Table("playlist_operations").Where("operation_id LIKE 'expired%'").Count(&n)
			if n != 1 {
				t.Fatal("bounded expired operation cleanup", n)
			}
			base := fmt.Sprintf("/playlists/%d", p.Playlist.ID)
			copied := f.request(1, "POST", base+"/copy", map[string]any{"operationId": "large-copy"}, 200)
			if copied.Playlist.TrackCount != library.MaxItems {
				t.Fatal(copied)
			}
			tracks := f.request(1, "GET", fmt.Sprintf("/playlists/%d/tracks", copied.Playlist.ID), nil, 200).Tracks
			if len(tracks) != library.MaxItems || tracks[0].TrackID != "1" || tracks[len(tracks)-1].TrackID != "10000" {
				t.Fatal("large copy order")
			}
		})
	}
}
