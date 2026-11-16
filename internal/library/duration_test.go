package library_test

import (
	"fmt"
	"testing"
)

func TestSongDurationRoundTripAndLegacyWrites(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver)
			a := track("duration")
			a["durationMs"] = 212000
			f.request(1, "POST", "/favorites", a, 200)
			fav := f.request(1, "GET", "/favorites", nil, 200).Favorites[0]
			if fav.DurationMS == nil || *fav.DurationMS != 212000 {
				t.Fatalf("%+v", fav)
			}
			created := f.request(1, "POST", "/playlists", map[string]any{"name": "Durations", "operationId": "create-duration"}, 200)
			path := fmt.Sprintf("/playlists/%d/tracks", created.Playlist.ID)
			a["operationId"] = "add-duration"
			f.request(1, "POST", path, a, 200)
			read := f.request(1, "GET", path, nil, 200).Tracks[0]
			if read.DurationMS == nil || *read.DurationMS != 212000 {
				t.Fatalf("%+v", read)
			}
			// An older client can write the same catalog entry without erasing duration.
			older := track("duration")
			older["operationId"] = "legacy-duration"
			second := f.request(1, "POST", "/playlists", map[string]any{"name": "Legacy", "operationId": "create-legacy"}, 200)
			f.request(1, "POST", fmt.Sprintf("/playlists/%d/tracks", second.Playlist.ID), older, 200)
			read = f.request(1, "GET", path, nil, 200).Tracks[0]
			if read.DurationMS == nil || *read.DurationMS != 212000 {
				t.Fatal("legacy write erased duration")
			}
			for i, bad := range []any{-1, 0, 86400001, 1.5, "212000"} {
				v := track(fmt.Sprintf("bad-%d", i))
				v["durationMs"] = bad
				f.request(1, "POST", "/favorites", v, 400)
			}
		})
	}
}
