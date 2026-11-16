package library_test

import (
	"fmt"
	"testing"
)

func TestLXMetadataSurvivesLegacyPartialCatalogWrites(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := setup(t, driver)
			a := track("lx-metadata")
			a["sourceIds"] = map[string]any{"fileHash": "HASH"}
			a["lxMetadata"] = map[string]any{"songId": "12345", "strMediaMid": "MEDIA1", "qualities": map[string]any{"128k": map[string]any{"size": "4.00 MiB", "bytes": 4194304}}}
			f.request(1, "POST", "/favorites", a, 200)
			fav := f.request(1, "GET", "/favorites", nil, 200).Favorites[0]
			if fav.LXMetadata.MediaMID != "MEDIA1" {
				t.Fatal(fav)
			}
			p := f.request(1, "POST", "/playlists", map[string]any{"name": "First", "operationId": "lx-first"}, 200)
			path := fmt.Sprintf("/playlists/%d/tracks", p.Playlist.ID)
			a["operationId"] = "lx-add"
			f.request(1, "POST", path, a, 200)
			for i, partial := range []map[string]any{track("lx-metadata"), track("lx-metadata")} {
				if i == 1 {
					partial["sourceIds"] = map[string]any{"emixSongId": "OTHER"}
					partial["lxMetadata"] = map[string]any{"albumMid": "ALBUM1"}
				}
				other := f.request(1, "POST", "/playlists", map[string]any{"name": "Other", "operationId": fmt.Sprintf("lx-create-%d", i)}, 200)
				partial["operationId"] = fmt.Sprintf("lx-partial-%d", i)
				f.request(1, "POST", fmt.Sprintf("/playlists/%d/tracks", other.Playlist.ID), partial, 200)
				read := f.request(1, "GET", path, nil, 200).Tracks[0]
				if read.LXMetadata.MediaMID != "MEDIA1" || read.LXMetadata.SongID != "12345" || read.LXMetadata.Qualities["128k"].Bytes != 4194304 || read.SourceIDs["fileHash"] != "HASH" {
					t.Fatal(read)
				}
				if i == 1 && read.LXMetadata.AlbumMID != "ALBUM1" {
					t.Fatal(read)
				}
			}
		})
	}
}
