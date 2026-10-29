package netease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryBatch(t *testing.T) {
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/playlist/highquality/tags":
			writeJSON(w, map[string]any{"code": 200, "tags": []any{map[string]any{"name": "流行"}}})
		case "/api/playlist/list":
			if r.URL.Query().Get("cat") != "流行" {
				t.Error("category")
			}
			writeJSON(w, map[string]any{"code": 200, "playlists": []any{map[string]any{"id": 1, "coverImgUrl": "http://p1.music.126.net/a.jpg"}}, "total": 40, "more": true})
		case "/api/cloudsearch/pc":
			r.ParseForm()
			if r.Form.Get("type") != "100" {
				t.Error("artist type")
			}
			writeJSON(w, map[string]any{"code": 200, "result": map[string]any{"artistCount": 1, "artists": []any{map[string]any{"id": 1, "name": "Artist", "alias": []string{"A"}}}}})
		case "/api/v6/playlist/detail":
			r.ParseForm()
			var id int64
			fmt.Sscan(r.Form.Get("id"), &id)
			writeJSON(w, map[string]any{"code": 200, "playlist": map[string]any{"id": id, "name": "Chart", "trackCount": 1, "trackIds": []any{map[string]any{"id": 1}}}})
		case "/api/v3/song/detail":
			writeJSON(w, map[string]any{"code": 200, "songs": []any{raw(1)}})
		default:
			t.Error("unexpected path", r.URL.Path)
		}
	}, 4, time.Second)
	ctx := context.Background()
	out, e := c.Request(ctx, "discover", map[string]string{"category": "流行"})
	if e != nil || out["hasMore"] != true || out["nextCursor"] == nil {
		t.Fatal(out, e)
	}
	cursor := *out["nextCursor"].(*string)
	if _, e = c.Request(ctx, "discover", map[string]string{"category": "流行", "cursor": cursor}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Request(ctx, "discover", map[string]string{"cursor": "not-json"}); e == nil {
		t.Fatal("invalid cursor")
	}
	tags, e := c.Request(ctx, "discover-tags", nil)
	if e != nil || tags["items"] == nil {
		t.Fatal(tags, e)
	}
	artists, e := c.Request(ctx, "artist", map[string]string{"keywords": "Artist", "limit": "20"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(artists)
	if !strings.Contains(string(b), "Artist") {
		t.Fatal(string(b))
	}
	home, e := c.Request(ctx, "home", nil)
	if e != nil || home["partial"] != false || len(home["sections"].([]any)) != 4 {
		t.Fatal(home, e)
	}
}
