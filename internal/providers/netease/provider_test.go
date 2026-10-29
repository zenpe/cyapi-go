package netease

import (
	"context"
	"crypto/aes"
	"cyapi-go/internal/music"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func raw(id int64) map[string]any {
	return map[string]any{"id": id, "name": "Song " + strconv.FormatInt(id, 10), "ar": []any{map[string]any{"name": "Artist"}}, "al": map[string]any{"name": "Album", "picUrl": "http://p1.music.126.net/cover.jpg"}, "dt": 180000}
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type routingTransport struct {
	target    *url.URL
	transport *http.Transport
}

func (t routingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || (r.URL.Host != "music.163.com" && r.URL.Host != "interface3.music.163.com") {
		return nil, errors.New("unexpected external request")
	}
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	copy.Host = r.URL.Host
	copy.URL.Scheme = t.target.Scheme
	copy.URL.Host = t.target.Host
	return t.transport.RoundTrip(copy)
}
func mockClient(t *testing.T, handler http.HandlerFunc, concurrent int, timeout time.Duration) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	client, e := New(Options{Cookie: "MUSIC_U=test-secret", MaxConcurrent: concurrent, RequestTimeout: timeout, Client: &http.Client{Transport: routingTransport{u, transport}}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(client.Close)
	return client
}
func decrypt(t *testing.T, params string) map[string]any {
	t.Helper()
	data, e := hex.DecodeString(params)
	if e != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		t.Fatal("invalid params")
	}
	cipher, _ := aes.NewCipher([]byte("e82ckenh8dichen8"))
	for i := 0; i < len(data); i += aes.BlockSize {
		cipher.Decrypt(data[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	pad := int(data[len(data)-1])
	parts := strings.Split(string(data[:len(data)-pad]), "-36cd479b6b5-")
	if len(parts) != 3 || parts[0] != "/api/song/enhance/player/url/v1" {
		t.Fatal(parts)
	}
	var payload map[string]any
	if e = json.Unmarshal([]byte(parts[1]), &payload); e != nil {
		t.Fatal(e)
	}
	return payload
}
func TestEAPIMatchesNodeReference(t *testing.T) {
	payload := struct {
		IDs        string `json:"ids"`
		Level      string `json:"level"`
		EncodeType string `json:"encodeType"`
	}{`["123"]`, "lossless", "flac"}
	// Captured independently with Node crypto using the existing TypeScript protocol.
	const expected = "fa90b329e9614f79e79598f37dc2edb487f00d1bc4c9b24cd57e6c318b9073569338432cd7d98d1a3626e997a2c531214d76eadf397e44b1591ce026c63832bfdbb39474742fb80c6e996943b37bae3feb58fc8ed8d0f44509cfba79d049bd3353557988f0792bc6b3aaa916e74f51c2c5d3b816fb1885a11f5b6cf1b805b2c1ba1b477f356e8026ec97e59b16a41f79c3d66ec35371277b46ca38811ab12f8e"
	actual, e := eapi("/api/song/enhance/player/url/v1", payload)
	if e != nil || actual != expected {
		t.Fatal(actual, e)
	}
}
func TestNeteaseProviderFlow(t *testing.T) {
	var omitted atomic.Bool
	var noURL atomic.Bool
	var unsafeURL atomic.Bool
	var emptyLyric atomic.Bool
	var unavailableLyric atomic.Bool
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "MUSIC_U=test-secret" || r.Header.Get("Referer") != "https://music.163.com/" {
			t.Error("provider headers missing")
		}
		r.ParseForm()
		switch r.URL.Path {
		case "/api/cloudsearch/pc":
			if r.Method != "POST" || r.Form.Get("s") != "hello" || r.Form.Get("offset") != "20" || r.Form.Get("type") != "1" {
				t.Error(r.Form)
			}
			writeJSON(w, map[string]any{"code": 200, "result": map[string]any{"songs": []any{raw(123), raw(456)}, "songCount": 41}})
		case "/api/v3/song/detail":
			var ids []struct{ ID int64 }
			if e := json.Unmarshal([]byte(r.Form.Get("c")), &ids); e != nil {
				t.Error(e)
			}
			songs := []any{}
			for i := len(ids) - 1; i >= 0; i-- {
				if omitted.Load() && ids[i].ID == 456 {
					continue
				}
				songs = append(songs, raw(ids[i].ID))
			}
			writeJSON(w, map[string]any{"code": 200, "songs": songs})
		case "/api/song/lyric":
			if unavailableLyric.Load() {
				writeJSON(w, map[string]any{"code": 200})
				return
			}
			if emptyLyric.Load() {
				writeJSON(w, map[string]any{"code": 200, "nolyric": true})
				return
			}
			writeJSON(w, map[string]any{"code": 200, "lrc": map[string]any{"lyric": "[00:01]hello"}, "tlyric": map[string]any{"lyric": "[00:01]翻译"}})
		case "/eapi/song/enhance/player/url/v1":
			p := decrypt(t, r.Form.Get("params"))
			if p["ids"] != `["123"]` || p["level"] != "sky" || p["immerseType"] != "c51" {
				t.Error(p)
			}
			var address any = "http://m10.music.126.net/song.flac"
			if noURL.Load() {
				address = nil
			}
			if unsafeURL.Load() {
				address = "http://127.0.0.1/admin"
			}
			writeJSON(w, map[string]any{"code": 200, "data": []any{map[string]any{"id": 123, "url": address, "level": "sky", "size": 1048576, "expi": 1200}}})
		case "/api/v6/playlist/detail":
			writeJSON(w, map[string]any{"code": 200, "playlist": map[string]any{"id": 99, "name": "Playlist", "trackCount": 3, "trackIds": []any{map[string]any{"id": 456}, map[string]any{"id": 123}, map[string]any{"id": 456}}, "creator": map[string]any{"nickname": "creator"}}})
		case "/api/v1/album/7":
			writeJSON(w, map[string]any{"code": 200, "album": map[string]any{"id": 7, "name": "Album", "picUrl": "cover", "size": 1, "artist": map[string]any{"name": "Artist"}}, "songs": []any{raw(123)}})
		default:
			t.Error("unexpected route", r.URL.Path)
			w.WriteHeader(404)
		}
	}, 4, time.Second)
	ctx := context.Background()
	page, e := c.Search(ctx, "hello", 2, 20)
	if e != nil || page.Page != 2 || !page.HasMore || len(page.Songs) != 2 || page.Songs[0].PicURL != "https://p1.music.126.net/cover.jpg" || page.PageFingerprint == nil || *page.PageFingerprint != "489c7f6567cc586f3f37474c32dd9076" {
		t.Fatal(page, e)
	}
	lyric, e := c.Lyric(ctx, "123")
	if e != nil || lyric.State != "ready" || lyric.Retryable {
		t.Fatal(lyric, e)
	}
	emptyLyric.Store(true)
	lyric, e = c.Lyric(ctx, "123")
	if e != nil || lyric.State != "empty" || lyric.Retryable {
		t.Fatal(lyric, e)
	}
	unavailableLyric.Store(true)
	lyric, e = c.Lyric(ctx, "123")
	if e != nil || lyric.State != "unavailable" || !lyric.Retryable {
		t.Fatal(lyric, e)
	}
	unavailableLyric.Store(false)
	emptyLyric.Store(false)
	song, e := c.Song(ctx, "123", "sky")
	if e != nil || song.URL != "https://m10.music.126.net/song.flac" || song.ID != 123 || song.Level != "沉浸环绕声" || song.Size != "1 MB" || song.ExpiresAt.IsZero() || song.Lyric == "" {
		t.Fatal(song, e)
	}
	playlist, e := c.Playlist(ctx, "99", 0)
	if e != nil || !playlist.Complete || len(playlist.Tracks) != 3 || playlist.Tracks[0].ID != 456 || playlist.Tracks[1].ID != 123 || playlist.Tracks[2].ID != 456 || playlist.Tracks[2].SourcePosition != 2 {
		t.Fatal(playlist, e)
	}
	omitted.Store(true)
	playlist, e = c.Playlist(ctx, "99", 0)
	if e != nil || playlist.Complete || playlist.ExpectedTrackCount != 3 || playlist.ResolvedTrackCount != 1 || playlist.Tracks[0].SourcePosition != 1 {
		t.Fatal("partial source positions", playlist, e)
	}
	omitted.Store(false)
	playlist, e = c.Playlist(ctx, "99", 1)
	if e != nil || !playlist.Complete || playlist.ExpectedTrackCount != 1 || playlist.TrackCount != 3 {
		t.Fatal(playlist, e)
	}
	album, e := c.Album(ctx, "7")
	if e != nil || len(album.Songs) != 1 || album.ID != 7 {
		t.Fatal(album, e)
	}
	noURL.Store(true)
	if _, e = c.Song(ctx, "123", "sky"); !errors.Is(e, music.ErrUnavailable) {
		t.Fatal(e)
	}
	noURL.Store(false)
	unsafeURL.Store(true)
	if _, e = c.Song(ctx, "123", "sky"); e == nil {
		t.Fatal("unsafe media redirect accepted")
	}
}
func TestProviderFailuresAndResponseLimit(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 302} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "60")
				w.Header().Set("Location", "https://evil.example/secret")
				w.WriteHeader(status)
				w.Write([]byte("private-cookie-value"))
			}, 2, time.Second)
			_, e := c.Lyric(context.Background(), "123")
			var upstream *music.UpstreamError
			if !errors.As(e, &upstream) || strings.Contains(e.Error(), "private-cookie-value") || calls.Load() != 1 {
				t.Fatal(e, calls.Load())
			}
			want := 502
			if status == 429 {
				want = 429
			}
			if upstream.Status != want || upstream.Retryable != (status >= 500) {
				t.Fatal(upstream)
			}
		})
	}
	for _, body := range []string{"<html>private-cookie-value</html>", `{"code":401}`, `{"code":200,"result":{}}`, strings.Repeat("x", (8<<20)+1)} {
		t.Run(strconv.Itoa(len(body)), func(t *testing.T) {
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }, 2, time.Second)
			if _, e := c.Search(context.Background(), "hello", 1, 20); e == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 200, "result": map[string]any{"songCount": 0}})
	}, 2, time.Second)
	empty, e := c.Search(context.Background(), "none", 1, 20)
	if e != nil || empty.PageFingerprint != nil || empty.Songs == nil || empty.HasMore {
		t.Fatal(empty, e)
	}
}
func TestProviderConcurrencyAndTimeout(t *testing.T) {
	var active, max atomic.Int32
	entered := make(chan struct{}, 20)
	release := make(chan struct{})
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			previous := max.Load()
			if n <= previous || max.CompareAndSwap(previous, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			writeJSON(w, map[string]any{"code": 200, "result": map[string]any{"songCount": 0}})
		case <-r.Context().Done():
		}
	}, 2, time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := c.Search(context.Background(), "hello", 1, 20); errs <- e }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("requests did not enter")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Error(e)
		}
	}
	if max.Load() != 2 {
		t.Fatal("concurrency limit", max.Load())
	}
	timed := mockClient(t, func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }, 1, 20*time.Millisecond)
	_, e := timed.Search(context.Background(), "hello", 1, 20)
	if !errors.Is(e, music.ErrTimeout) {
		t.Fatal(e)
	}
}
