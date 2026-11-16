package qq

import (
	"cyapi-go/internal/music"
	"encoding/json"
	"testing"
)

func TestMappedQQKeepsDistinctOriginalIDs(t *testing.T) {
	raw := map[string]any{"mid": "MID1", "id": json.Number("12345"), "name": "Song", "album": map[string]any{"id": json.Number("999"), "mid": "ALBUM1", "name": "Album"}, "file": map[string]any{"media_mid": "MEDIA1", "size_128mp3": json.Number("4194304"), "size_flac": json.Number("0")}}
	got := music.ParseLXMetadata(mapped(raw)["lxMetadata"])
	if got.SongID != "12345" || got.MediaMID != "MEDIA1" || got.AlbumMID != "ALBUM1" || got.Qualities["128k"].Bytes != 4194304 {
		t.Fatal(got)
	}
	if _, ok := got.Qualities["flac"]; ok {
		t.Fatal("invented unavailable quality")
	}
	unknown := lxMetadata(map[string]any{"mid": "MID1"})
	if unknown.SongID != "" || unknown.MediaMID != "" {
		t.Fatal("invented IDs", unknown)
	}
}
