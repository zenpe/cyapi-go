package music

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProvider struct {
	searches    atomic.Int32
	searchError error
	lyricError  error
	songExpiry  time.Time
}

func (f *fakeProvider) Search(context.Context, string, int, int) (SearchPage, error) {
	f.searches.Add(1)
	return SearchPage{Songs: []Track{}, Page: 1, ResultKind: "full"}, f.searchError
}
func (f *fakeProvider) Lyric(context.Context, string) (Lyric, error) {
	return Lyric{State: "ready", Lyric: "text"}, f.lyricError
}
func (f *fakeProvider) Song(context.Context, string, string) (Song, error) {
	return Song{ID: 1, URL: "https://m10.music.126.net/song", ExpiresAt: f.songExpiry}, nil
}
func (f *fakeProvider) Playlist(context.Context, string, int) (Playlist, error) {
	return Playlist{Tracks: []PlaylistTrack{}, Complete: true}, nil
}
func (f *fakeProvider) Album(context.Context, string) (Album, error) {
	return Album{Songs: []AlbumTrack{}}, nil
}
func options() Options {
	return Options{MaxBytes: 1 << 20, MaxEntries: 100, MaxItemBytes: 1 << 20, UserRequestsPerMinute: 120, IPRequestsPerMinute: 300}
}
func TestMusicRateLimitsAndCacheScopes(t *testing.T) {
	f := &fakeProvider{}
	o := options()
	o.UserRequestsPerMinute = 2
	o.IPRequestsPerMinute = 3
	s, e := New(f, o)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	s.now = func() time.Time { return now }
	q := Query{Kind: "search", Keywords: "hello", Page: 1, Limit: 20}
	for i := 0; i < 2; i++ {
		r, _ := s.Execute(context.Background(), 1, "ip", q, false)
		if r.Status != 200 {
			t.Fatal(r)
		}
	}
	r, _ := s.Execute(context.Background(), 1, "ip", q, false)
	if r.Status != 429 || r.RetryAfter == "" {
		t.Fatal("cached requests bypassed user limit", r)
	}
	r, _ = s.Execute(context.Background(), 2, "ip", q, false)
	if r.Status != 200 {
		t.Fatal(r)
	}
	r, _ = s.Execute(context.Background(), 3, "ip", q, false)
	if r.Status != 429 {
		t.Fatal("IP limit not shared", r)
	}
	if f.searches.Load() != 1 {
		t.Fatal("public response not shared")
	}
	now = now.Add(2 * time.Minute)
	r, _ = s.Execute(context.Background(), 1, "ip", q, false)
	if r.Status != 200 {
		t.Fatal("rate window not reset", r)
	}
	o = options()
	o.PrivateCache = true
	s, _ = New(f, o)
	before := f.searches.Load()
	s.Execute(context.Background(), 1, "ip", q, false)
	s.Execute(context.Background(), 2, "ip", q, false)
	if f.searches.Load() != before+2 {
		t.Fatal("private provider cache crossed users")
	}
	s.mu.Lock()
	s.rates = map[string]rate{}
	for i := 0; i < 4096; i++ {
		s.rates[string(rune(i))+"key"] = rate{count: 1, expires: now.Add(time.Hour)}
	}
	s.pruneAt = now.Add(time.Hour)
	s.mu.Unlock()
	r, _ = s.Execute(context.Background(), 9, "new", q, false)
	if r.Status != 429 || len(s.rates) != 4096 {
		t.Fatal("rate key capacity ignored")
	}
}
func TestErrorContractsAndExpiringURLs(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{BadUpstream(), 502, "SEARCH_UPSTREAM_FAILED"}, {ErrTimeout, 504, "SEARCH_TIMEOUT"}, {context.Canceled, 504, "SEARCH_TIMEOUT"}} {
		f := &fakeProvider{searchError: tc.err}
		s, _ := New(f, options())
		r, _ := s.Execute(context.Background(), 1, "ip", Query{Kind: "search", Keywords: "hello", Page: 1, Limit: 20}, false)
		if r.Status != tc.status || !strings.Contains(string(r.Body), tc.code) {
			t.Fatal(string(r.Body))
		}
		s.Execute(context.Background(), 1, "ip", Query{Kind: "search", Keywords: "hello", Page: 1, Limit: 20}, false)
		if f.searches.Load() != 2 {
			t.Fatal("failed response cached")
		}
	}
	if r := failure("lyric", &UpstreamError{Status: 502, Retryable: false}); r.Status != 502 || !strings.Contains(string(r.Body), `"retryable":false`) {
		t.Fatal(r)
	}
	if r := failure("lyric", ErrTimeout); r.Status != 502 || !strings.Contains(string(r.Body), `"retryable":true`) {
		t.Fatal(r)
	}
	if r := failure("song", ErrUnavailable); r.Status != 422 {
		t.Fatal(r)
	}
	for _, expiry := range []time.Time{{}, time.Now().Add(-time.Minute), time.Now().Add(time.Hour)} {
		f := &fakeProvider{songExpiry: expiry}
		s, _ := New(f, options())
		q := Query{Kind: "song", ID: "1", Level: "lossless", Mode: "json"}
		r, _ := s.Execute(context.Background(), 1, "ip", q, false)
		_, label := s.Execute(context.Background(), 1, "ip", q, false)
		if expiry.After(time.Now()) {
			if label != "HIT" || r.TTL > 30*time.Second {
				t.Fatal(label, r.TTL)
			}
		} else if label == "HIT" {
			t.Fatal("URL without valid expiry cached")
		}
	}
	for _, id := range []string{"0", "-1", "1e3", "9007199254740992", "https://evil.example/?id=1", "https://music.163.com/?id=1&id=2"} {
		if _, e := ParseID(id); e == nil {
			t.Fatal("invalid ID accepted", id)
		}
	}
	if id, e := ParseID("https://music.163.com/#/song?id=123"); e != nil || id != "123" {
		t.Fatal(id, e)
	}
	f := &fakeProvider{searchError: errors.New("secret=private-value")}
	s, _ := New(f, options())
	r, _ := s.Execute(context.Background(), 1, "ip", Query{Kind: "search", Keywords: "hello", Page: 1, Limit: 20}, false)
	if strings.Contains(string(r.Body), "private-value") {
		t.Fatal("raw error leaked")
	}
}
