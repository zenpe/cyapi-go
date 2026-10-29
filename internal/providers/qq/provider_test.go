package qq

import (
	"context"
	"cyapi-go/internal/providers/platformhttp"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type routeTransport struct{ target *url.URL }

func (t routeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(copy)
}
func TestQQBatchProtocol(t *testing.T) {
	var smart atomic.Int32
	var badURL atomic.Bool
	song := func(mid string, id int) map[string]any {
		return map[string]any{"mid": mid, "id": id, "name": "Song", "album": map[string]any{"mid": "ALBUM", "name": "Album"}, "singer": []any{map[string]any{"name": "Artist"}}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "uin=123" {
			t.Error("missing cookie")
		}
		var out any
		switch r.URL.Path {
		case "/v8/fcg-bin/fcg_play_single_song.fcg":
			r.ParseForm()
			if r.Form.Get("songmid") == "" && r.Form.Get("songid") == "" {
				t.Error("missing song ID")
			}
			out = map[string]any{"code": 0, "data": []any{song("MID1", 1)}}
		case "/splcloud/fcgi-bin/smartbox_new.fcg":
			smart.Add(1)
			out = map[string]any{"code": 0, "data": map[string]any{"song": map[string]any{"itemlist": []any{song("SUGGEST", 2)}}}}
		default:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if lyric, ok := body["music.musichallSong.PlayLyricInfo.GetPlayLyricInfo"]; ok {
				_ = lyric
				out = map[string]any{"code": 0, "music.musichallSong.PlayLyricInfo.GetPlayLyricInfo": map[string]any{"code": 0, "data": map[string]any{"lyric": base64.StdEncoding.EncodeToString([]byte("[00:01]歌词")), "trans": ""}}}
				break
			}
			req := platformhttp.Obj(body["req_1"])
			param := platformhttp.Obj(req["param"])
			var data any
			switch req["method"] {
			case "DoSearchForQQMusicDesktop":
				items := []any{song("MID1", 1)}
				if param["query"] == "empty" {
					items = []any{}
				}
				if platformhttp.Obj(body["comm"])["ct"] != float64(19) {
					t.Error("search comm")
				}
				data = map[string]any{"body": map[string]any{"song": map[string]any{"list": items}}}
			case "CgiGetVkey":
				if platformhttp.Obj(body["comm"])["uin"] != "123" {
					t.Error("vkey comm uin")
				}
				sip := "http://stream.qqmusic.qq.com/"
				if badURL.Load() {
					sip = "http://127.0.0.1/"
				}
				data = map[string]any{"midurlinfo": []any{map[string]any{"purl": "song.mp3?vkey=test"}}, "sip": []string{sip}}
			case "CgiGetDiss":
				data = map[string]any{"dirinfo": map[string]any{"id": 42, "title": "Playlist", "songnum": 3}, "songlist": []any{song("MID2", 2), song("MID1", 1), song("MID2", 2)}}
			default:
				t.Error("unexpected method", req["method"])
			}
			out = map[string]any{"code": 0, "req_1": map[string]any{"code": 0, "data": data}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	client, e := New(platformhttp.Options{Cookie: "uin=123", MaxConcurrent: 4, Timeout: time.Second, Client: &http.Client{Transport: routeTransport{u}}})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	ctx := context.Background()
	full, e := client.Request(ctx, "search", map[string]string{"keywords": "full", "page": "1", "limit": "2"})
	if e != nil || full["resultKind"] != "full" || smart.Load() != 0 {
		t.Fatal(full, e)
	}
	suggestions, e := client.Request(ctx, "search", map[string]string{"keywords": "empty", "page": "1", "limit": "2"})
	if e != nil || suggestions["resultKind"] != "suggestions" || suggestions["hasMore"] != false || smart.Load() != 1 {
		t.Fatal(suggestions, e)
	}
	_, e = client.Request(ctx, "search", map[string]string{"keywords": "empty", "page": "2", "limit": "2"})
	if e != nil || smart.Load() != 1 {
		t.Fatal("later page fallback", e)
	}
	lyric, e := client.Request(ctx, "lyric", map[string]string{"id": "https://y.qq.com/n/ryqq/songDetail/MID1"})
	if e != nil || platformhttp.Obj(lyric["data"])["lyric"] != "[00:01]歌词" {
		t.Fatal(lyric, e)
	}
	s, e := client.Request(ctx, "song", map[string]string{"ids": "MID1"})
	if e != nil || len(platformhttp.Obj(s["music_urls"])) != 3 {
		t.Fatal(s, e)
	}
	if !strings.HasPrefix(platformhttp.Text(platformhttp.Obj(platformhttp.Obj(s["music_urls"])["flac"])["url"]), "https://") {
		t.Fatal(s)
	}
	snap, e := client.FetchSnapshot(ctx, "qq", "42")
	if e != nil || !snap.Complete || len(snap.Tracks) != 3 || snap.Tracks[0].TrackID != "MID2" || snap.Tracks[2].SourcePosition != 2 {
		t.Fatal(snap, e)
	}
	if _, e = client.Request(ctx, "song", map[string]string{"ids": "http://127.0.0.1/secret"}); e == nil {
		t.Fatal("accepted external URL")
	}
	badURL.Store(true)
	if _, e = client.Request(ctx, "song", map[string]string{"ids": "MID1"}); e == nil {
		t.Fatal("accepted unsafe media URL")
	}
}
