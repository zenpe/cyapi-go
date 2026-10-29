package kugou

import (
	"context"
	"crypto/md5"
	"cyapi-go/internal/providers/platformhttp"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testTransport struct{ target *url.URL }

func (t testTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	u.Host = t.target.Host
	u.Scheme = t.target.Scheme
	return http.DefaultTransport.RoundTrip(copy)
}
func TestKugouSignedBatch(t *testing.T) {
	var malicious atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		signature := q.Get("signature")
		q.Del("signature")
		keys := []string{}
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		raw := "NVPh5oo715z5DIWAeQlhMDsWXXQV4hwt"
		for _, k := range keys {
			raw += k + "=" + q.Get(k)
		}
		raw += "NVPh5oo715z5DIWAeQlhMDsWXXQV4hwt"
		hash := md5.Sum([]byte(raw))
		if hex.EncodeToString(hash[:]) != signature || q.Get("userid") != "3" || q.Get("dfid") != "d" {
			t.Error("invalid signature/device params")
		}
		var out any
		if r.URL.Path == "/v2/search/song" {
			out = map[string]any{"status": 1, "data": map[string]any{"total": 3, "lists": []any{map[string]any{"SongName": "Song", "FileHash": "HASH", "EMixSongID": "EMIX", "Image": "http://img.kugou.com/{size}/image.jpg"}}}}
		} else {
			address := "http://fs.kugou.com/music.mp3"
			if malicious.Load() {
				address = "http://127.0.0.1/secret"
			}
			out = map[string]any{"status": 1, "data": map[string]any{"song_name": "Song", "play_url": address, "lyrics": "[00:01]歌词", "timelength": 123000}}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	c, e := New(platformhttp.Options{Cookie: "mid=m; kg_dfid=d; KugooID=3; token=t", MaxConcurrent: 2, Timeout: time.Second, Client: &http.Client{Transport: testTransport{u}}})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	ctx := context.Background()
	s, e := c.Request(ctx, "search", map[string]string{"keywords": "test", "page": "1", "limit": "1"})
	if e != nil || s["hasMore"] != true {
		t.Fatal(s, e)
	}
	song, e := c.Request(ctx, "song", map[string]string{"emixsongid": "EMIX"})
	if e != nil || !strings.HasPrefix(platformhttp.Text(platformhttp.Obj(song["song"])["url"]), "https://") {
		t.Fatal(song, e)
	}
	l, e := c.Request(ctx, "lyric", map[string]string{"emixsongid": "EMIX"})
	if e != nil || platformhttp.Obj(l["data"])["state"] != "ready" {
		t.Fatal(l, e)
	}
	malicious.Store(true)
	if _, e = c.Request(ctx, "song", map[string]string{"emixsongid": "EMIX"}); e == nil {
		t.Fatal("unsafe URL")
	}
}
