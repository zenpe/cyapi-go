package neteaseaccount

import (
	"bytes"
	"context"
	"cyapi-go/internal/config"
	"cyapi-go/internal/library"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/accountstore"
	"cyapi-go/internal/providers/netease"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakePlatform struct {
	handle func(context.Context, string, string, map[string]any) (map[string]any, string, error)
}

func (f fakePlatform) AccountRequest(ctx context.Context, cookie, path string, data map[string]any) (map[string]any, string, error) {
	return f.handle(ctx, cookie, path, data)
}
func testService(t *testing.T, f fakePlatform) (*Service, *persistence.Database) {
	t.Helper()
	db, e := persistence.Open(context.Background(), config.Database{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if e = db.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	s, e := New(accountstore.New(db.ORM), f, bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	return s, db
}
func profile(uid float64) map[string]any {
	return map[string]any{"code": float64(200), "profile": map[string]any{"userId": uid, "nickname": "test-user", "avatarUrl": "https://p1.music.126.net/test.jpg"}}
}
func bind(t *testing.T, s *Service, user int64, cookie string) {
	t.Helper()
	a, e := s.Start(context.Background(), user, "web")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Complete(context.Background(), user, a["id"].(string), cookie); e != nil {
		t.Fatal(e)
	}
}
func code(t *testing.T, e error, want string) {
	t.Helper()
	var f *library.Failure
	if !errors.As(e, &f) || f.Code != want {
		t.Fatalf("want %s got %v", want, e)
	}
}
func TestCredentialIsolationEncryptionAndRestore(t *testing.T) {
	s, db := testService(t, fakePlatform{func(_ context.Context, cookie, path string, _ map[string]any) (map[string]any, string, error) {
		if path != "/w/nuser/account/get" {
			t.Fatal(path)
		}
		if cookie != "MUSIC_U=private1" {
			t.Fatal("wrong cookie")
		}
		return profile(101), "", nil
	}})
	bind(t, s, 1, "MUSIC_U=private1; irrelevant=discard")
	b, e := s.store.Get(context.Background(), 1)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(b.Credentials, "private1") {
		t.Fatal("plaintext persisted")
	}
	stored, _ := json.Marshal(public(b))
	if strings.Contains(string(stored), "private1") || strings.Contains(string(stored), "credentials") {
		t.Fatal("credential response leak")
	}
	if cookie, e := s.decrypt(b); e != nil || cookie != "MUSIC_U=private1" {
		t.Fatal("decrypt failed")
	}
	s2, _ := New(accountstore.New(db.ORM), s.platform, bytes.Repeat([]byte{7}, 32))
	if cookie, e := s2.decrypt(b); e != nil || cookie != "MUSIC_U=private1" {
		t.Fatal("restore failed")
	}
	b.UserID = 2
	if _, e = s.decrypt(b); e == nil {
		t.Fatal("cross-user ciphertext accepted")
	}
	_, _, e = s.credentials(context.Background(), 2)
	code(t, e, "NETEASE_NOT_BOUND")
	if e = s.Unbind(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	next, _ := s.store.Get(context.Background(), 1)
	if next.Revision != 2 || next.Credentials != "" {
		t.Fatal(next)
	}
}
func TestCancelledVerificationCannotResurrectBinding(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, _ := testService(t, fakePlatform{func(_ context.Context, _, _ string, _ map[string]any) (map[string]any, string, error) {
		close(entered)
		<-release
		return profile(101), "", nil
	}})
	ctx := context.Background()
	a, e := s.Start(ctx, 1, "web")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Complete(ctx, 1, a["id"].(string), "MUSIC_U=test"); done <- err }()
	<-entered
	if e = s.Unbind(ctx, 1); e != nil {
		t.Fatal(e)
	}
	close(release)
	code(t, <-done, "BINDING_CHANGED")
	b, _ := s.store.Get(ctx, 1)
	if b.Status != "unbound" || b.Credentials != "" {
		t.Fatal("binding resurrected")
	}
}
func TestUpstreamExpiryAndTemporaryFailures(t *testing.T) {
	failure := error(nil)
	s, _ := testService(t, fakePlatform{func(_ context.Context, _, path string, _ map[string]any) (map[string]any, string, error) {
		if path == "/w/nuser/account/get" {
			return profile(101), "", nil
		}
		return map[string]any{"code": float64(200), "data": []any{}}, "", failure
	}})
	bind(t, s, 1, "MUSIC_U=test")
	failure = netease.ErrAccountUnavailable
	_, e := s.request(context.Background(), 1, "/v1/radio/get", nil)
	code(t, e, "NETEASE_UNAVAILABLE")
	b, _ := s.store.Get(context.Background(), 1)
	if b.Status != "bound" {
		t.Fatal("temporary failure invalidated login")
	}
	failure = netease.ErrAccountExpired
	_, e = s.request(context.Background(), 1, "/v1/radio/get", nil)
	code(t, e, "NETEASE_LOGIN_EXPIRED")
	b, _ = s.store.Get(context.Background(), 1)
	if b.Status != "expired" {
		t.Fatal("expiry not recorded")
	}
}
func TestRequestCacheDoesNotCrossUserOrBinding(t *testing.T) {
	calls := 0
	s, _ := testService(t, fakePlatform{func(_ context.Context, cookie, path string, _ map[string]any) (map[string]any, string, error) {
		if path == "/w/nuser/account/get" {
			return profile(101), "", nil
		}
		calls++
		return map[string]any{"code": float64(200), "owner": cookie}, "", nil
	}})
	bind(t, s, 1, "MUSIC_U=one")
	bind(t, s, 2, "MUSIC_U=two")
	for i := 0; i < 2; i++ {
		out, e := s.request(context.Background(), 1, "/user/playlist", nil)
		if e != nil || out["owner"] != "MUSIC_U=one" {
			t.Fatal("wrong response")
		}
	}
	out, e := s.request(context.Background(), 2, "/user/playlist", nil)
	if e != nil || out["owner"] != "MUSIC_U=two" || calls != 2 {
		t.Fatal("cross-user cache")
	}
	if e = s.Unbind(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	_, e = s.request(context.Background(), 1, "/user/playlist", nil)
	code(t, e, "NETEASE_NOT_BOUND")
}
func TestKeyMissingOrDamagedIsNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "netease.key")
	if _, e := LoadKey(path, true); e == nil {
		t.Fatal("silently recreated key")
	}
	key, e := LoadKey(path, false)
	if e != nil || len(key) != 32 {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte("damaged"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadKey(path, false); e == nil {
		t.Fatal("damaged key accepted")
	}
}
func TestInvalidCredentialsNeverContactPlatform(t *testing.T) {
	s, _ := testService(t, fakePlatform{func(context.Context, string, string, map[string]any) (map[string]any, string, error) {
		t.Fatal("invalid cookie reached upstream")
		return nil, "", nil
	}})
	a, _ := s.Start(context.Background(), 1, "web")
	for _, value := range []string{"", "MUSIC_U=x\r\nInjected: y", "other=x"} {
		_, e := s.Complete(context.Background(), 1, a["id"].(string), value)
		code(t, e, "INVALID_CREDENTIALS")
	}
}
