// Package neteaseaccount owns per-user platform credentials and short-lived binding attempts.
package neteaseaccount

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/library"
	"cyapi-go/internal/persistence/accountstore"
	"cyapi-go/internal/providers/netease"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Platform interface {
	AccountRequest(context.Context, string, string, map[string]any) (map[string]any, string, error)
}
type attempt struct {
	ID, Method, Key   string
	Revision          int64
	Expires, LastPoll time.Time
	State             string
	Busy              bool
}
type Service struct {
	store    *accountstore.Store
	platform Platform
	aead     cipher.AEAD
	mu       sync.Mutex
	attempts map[int64]*attempt
	cache    map[string]cacheEntry
	rates    map[int64]rateEntry
}

func New(store *accountstore.Store, platform Platform, key []byte) (*Service, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	return &Service{store: store, platform: platform, aead: a, attempts: map[int64]*attempt{}, cache: map[string]cacheEntry{}, rates: map[int64]rateEntry{}}, nil
}

// LoadKey never silently replaces an existing key. The file lives in managed data
// and must be restored together with the database. Creation uses exclusive open.
func LoadKey(path string, hasCredentials bool) ([]byte, error) {
	info, e := os.Lstat(path)
	if e == nil {
		if !info.Mode().IsRegular() || info.Size() != 32 {
			return nil, errors.New("invalid netease credential key; restore data/netease.key")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, errors.New("cannot read netease credential key")
		}
		return b, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if hasCredentials {
		return nil, errors.New("netease credential key missing; restore data/netease.key from backup")
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(e, os.ErrExist) {
		return LoadKey(path, hasCredentials)
	}
	if e != nil {
		return nil, e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return nil, e
	}
	if ce != nil {
		return nil, ce
	}
	return b, nil
}
func (s *Service) seal(user int64, cookie string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return "", e
	}
	return base64.RawStdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(cookie), []byte(fmt.Sprintf("netease:v1:%d", user)))), nil
}
func (s *Service) decrypt(b accountstore.Binding) (string, error) {
	raw, e := base64.RawStdEncoding.DecodeString(b.Credentials)
	n := s.aead.NonceSize()
	if e != nil || len(raw) < n {
		return "", errors.New("invalid netease credential ciphertext")
	}
	out, e := s.aead.Open(nil, raw[:n], raw[n:], []byte(fmt.Sprintf("netease:v1:%d", b.UserID)))
	if e != nil {
		return "", errors.New("cannot decrypt netease credentials; check restored key")
	}
	return string(out), nil
}
func public(b accountstore.Binding) map[string]any {
	out := map[string]any{"supported": true, "bound": b.Status == "bound", "status": b.Status, "revision": b.Revision}
	var p map[string]any
	_ = json.Unmarshal([]byte(b.PayloadJSON), &p)
	for _, k := range []string{"uid", "nickname", "avatarUrl"} {
		if p[k] != nil {
			out[k] = p[k]
		}
	}
	return out
}
func (s *Service) Bindings(ctx context.Context, user int64) (map[string]any, error) {
	b, e := s.store.Get(ctx, user)
	return map[string]any{"netease": public(b), "kugou": map[string]any{"supported": false, "bound": false, "status": "unsupported"}}, e
}
func (s *Service) Start(ctx context.Context, user int64, method string) (map[string]any, error) {
	if method != "web" && method != "qr" {
		return nil, library.Fail(400, "INVALID_METHOD", "登录方式无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, a := range s.attempts {
		if now.After(a.Expires) {
			delete(s.attempts, id)
		}
	}
	if old := s.attempts[user]; old != nil && now.Sub(old.Expires.Add(-3*time.Minute)) < 2*time.Second {
		return nil, library.Fail(429, "BINDING_BUSY", "请稍后重试")
	}
	if len(s.attempts) >= 256 && s.attempts[user] == nil {
		return nil, library.Fail(429, "BINDING_BUSY", "登录请求繁忙")
	}
	b, e := s.store.Get(ctx, user)
	if e != nil {
		return nil, e
	}
	raw := make([]byte, 24)
	if _, e = rand.Read(raw); e != nil {
		return nil, e
	}
	a := &attempt{ID: base64.RawURLEncoding.EncodeToString(raw), Method: method, Revision: b.Revision, Expires: now.Add(3 * time.Minute), State: "waiting"}
	s.attempts[user] = a
	return map[string]any{"id": a.ID, "method": method, "expiresAt": a.Expires.UTC().Format(time.RFC3339), "status": "waiting"}, nil
}
func (s *Service) find(user int64, id string) (*attempt, error) {
	a := s.attempts[user]
	if a == nil || a.ID != id || time.Now().After(a.Expires) {
		return nil, library.Fail(410, "BIND_SESSION_EXPIRED", "登录会话已过期，请重新打开登录")
	}
	return a, nil
}
func (s *Service) Cancel(user int64, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.attempts[user]; a != nil && a.ID == id {
		delete(s.attempts, user)
	}
}
func normalizeCookie(raw string) (string, error) {
	if len(raw) > 16384 || strings.ContainsAny(raw, "\r\n\x00") {
		return "", library.Fail(400, "INVALID_CREDENTIALS", "登录凭据格式无效")
	}
	req := &http.Request{Header: http.Header{"Cookie": []string{raw}}}
	var parts []string
	music := false
	for _, c := range req.Cookies() {
		switch c.Name {
		case "MUSIC_U", "__csrf", "NMTID":
			if c.Value != "" {
				parts = append(parts, c.Name+"="+c.Value)
				if c.Name == "MUSIC_U" {
					music = true
				}
			}
		}
	}
	if !music {
		return "", library.Fail(400, "INVALID_CREDENTIALS", "尚未取得网易云登录凭据")
	}
	return strings.Join(parts, "; "), nil
}
func (s *Service) Complete(ctx context.Context, user int64, id, cookie string) (map[string]any, error) {
	cookie, e := normalizeCookie(cookie)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	a, e := s.find(user, id)
	if e == nil && a.Busy {
		e = library.Fail(409, "BINDING_BUSY", "正在验证账号")
	}
	if e == nil {
		a.Busy = true
	}
	s.mu.Unlock()
	if e != nil {
		return nil, e
	}
	defer func() { s.mu.Lock(); a.Busy = false; s.mu.Unlock() }()
	return s.complete(ctx, user, a, cookie)
}
func (s *Service) complete(ctx context.Context, user int64, a *attempt, cookie string) (map[string]any, error) {
	out, _, e := s.platform.AccountRequest(ctx, cookie, "/w/nuser/account/get", map[string]any{})
	if e != nil {
		return nil, platformError(e)
	}
	p, _ := out["profile"].(map[string]any)
	uid, _ := p["userId"].(float64)
	name, _ := p["nickname"].(string)
	avatar, _ := p["avatarUrl"].(string)
	if out["code"] != float64(200) || uid <= 0 || name == "" {
		return nil, library.Fail(409, "NETEASE_LOGIN_INVALID", "网易云登录验证失败，请重新登录")
	}
	profile := map[string]any{"uid": fmt.Sprintf("%.0f", uid), "nickname": name, "avatarUrl": avatar}
	secret, e := s.seal(user, cookie)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, e := s.find(user, a.ID)
	if e != nil || current != a {
		return nil, library.Fail(409, "BINDING_CHANGED", "登录会话已变化")
	}
	b, e := s.store.Change(ctx, user, a.Revision, "bound", secret, profile)
	if e != nil {
		return nil, e
	}
	delete(s.attempts, user)
	s.purgeCache(user)
	return public(b), nil
}
func (s *Service) Unbind(ctx context.Context, user int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, user)
	b, e := s.store.Get(ctx, user)
	if e != nil {
		return e
	}
	_, e = s.store.Change(ctx, user, b.Revision, "unbound", "", nil)
	s.purgeCache(user)
	return e
}
func platformError(e error) error {
	if errors.Is(e, netease.ErrAccountExpired) {
		return library.Fail(409, "NETEASE_LOGIN_EXPIRED", "网易云登录已过期，请重新绑定")
	}
	return library.Fail(502, "NETEASE_UNAVAILABLE", "网易云暂时不可用，请稍后重试")
}
func (s *Service) credentials(ctx context.Context, user int64) (accountstore.Binding, string, error) {
	b, e := s.store.Get(ctx, user)
	if e != nil {
		return b, "", e
	}
	if b.Status != "bound" {
		code, msg := "NETEASE_NOT_BOUND", "请先绑定网易云账号"
		if b.Status == "expired" {
			code, msg = "NETEASE_LOGIN_EXPIRED", "网易云登录已过期，请重新绑定"
		}
		return b, "", library.Fail(409, code, msg)
	}
	cookie, e := s.decrypt(b)
	return b, cookie, e
}
func (s *Service) check(ctx context.Context, b accountstore.Binding) error {
	current, e := s.store.Get(ctx, b.UserID)
	if e != nil {
		return e
	}
	if current.Revision != b.Revision || current.Status != "bound" {
		return library.Fail(409, "BINDING_CHANGED", "账号绑定已变化，请刷新")
	}
	return nil
}
func (s *Service) request(ctx context.Context, user int64, path string, data map[string]any) (map[string]any, error) {
	b, cookie, e := s.credentials(ctx, user)
	if e != nil {
		return nil, e
	}

	return s.requestBinding(ctx, b, cookie, path, data)
}
func (s *Service) requestBinding(ctx context.Context, b accountstore.Binding, cookie, path string, data map[string]any) (map[string]any, error) {
	user := b.UserID
	encoded, _ := json.Marshal(data)
	key := fmt.Sprintf("%d:%d:%s:%s", user, b.Revision, path, encoded)
	s.mu.Lock()
	now := time.Now()
	rate := s.rates[user]
	if now.Sub(rate.Start) > time.Minute {
		rate = rateEntry{Start: now}
	}
	if rate.Count >= 60 {
		s.mu.Unlock()
		return nil, library.Fail(429, "NETEASE_RATE_LIMITED", "网易云请求过于频繁，请稍后重试")
	}
	rate.Count++
	s.rates[user] = rate
	for id, r := range s.rates {
		if now.Sub(r.Start) > 2*time.Minute {
			delete(s.rates, id)
		}
	}
	if len(s.rates) > 1024 {
		delete(s.rates, user)
		s.mu.Unlock()
		return nil, library.Fail(429, "NETEASE_RATE_LIMITED", "请求繁忙，请稍后重试")
	}
	cached, hit := s.cache[key]
	s.mu.Unlock()
	if hit && now.Before(cached.Expires) {
		if e := s.check(ctx, b); e != nil {
			return nil, e
		}
		var value map[string]any
		if json.Unmarshal(cached.Data, &value) == nil {
			return value, nil
		}
	}
	out, _, e := s.platform.AccountRequest(ctx, cookie, path, data)
	if errors.Is(e, netease.ErrAccountExpired) {
		s.mu.Lock()
		var p map[string]any
		_ = json.Unmarshal([]byte(b.PayloadJSON), &p)
		_, _ = s.store.Change(ctx, user, b.Revision, "expired", b.Credentials, p)
		s.purgeCache(user)
		s.mu.Unlock()
	}
	if e != nil {
		return nil, platformError(e)
	}
	if e := s.check(ctx, b); e != nil {
		return nil, e
	}
	if out["code"] != float64(200) {
		return nil, platformError(netease.ErrAccountUnavailable)
	}

	if path != "/v1/radio/get" {
		raw, _ := json.Marshal(out)
		if len(raw) <= 512<<10 {
			expires := time.Now().Add(3 * time.Minute)
			if path == "/v3/discovery/recommend/songs" {
				local := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
				expires = time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, local.Location())
			}
			s.mu.Lock()
			if err := s.check(ctx, b); err != nil {
				s.mu.Unlock()
				return nil, err
			}
			if len(s.cache) >= 16 {
				for k := range s.cache {
					delete(s.cache, k)
					break
				}
			}
			s.cache[key] = cacheEntry{Data: raw, Expires: expires}
			s.mu.Unlock()
		}
	}
	return out, nil
}

// PersonalSnapshot reuses the immutable public provider with a request-local
// credential view. Public playlists remain accessible when no account is bound.
func (s *Service) PersonalSnapshot(ctx context.Context, client *netease.Client, source, id string) (library.RemoteSnapshot, error) {
	u := auth.CurrentUser(ctx)
	if u == nil {
		return client.FetchSnapshot(ctx, source, id)
	}
	b, e := s.store.Get(ctx, u.ID)
	if e != nil {
		return library.RemoteSnapshot{}, e
	}
	if b.Status != "bound" {
		return client.FetchSnapshot(ctx, source, id)
	}
	cookie, e := s.decrypt(b)
	if e != nil {
		return library.RemoteSnapshot{}, e
	}
	out, e := client.WithAccountCookie(cookie).FetchSnapshot(ctx, source, id)
	if e != nil {
		return out, e
	}
	out.AccountRevision = &b.Revision
	return out, s.check(ctx, b)
}

// Caller holds mu. Revoke cached personal data immediately on account changes.
func (s *Service) purgeCache(user int64) {
	prefix := fmt.Sprintf("%d:", user)
	for key := range s.cache {
		if strings.HasPrefix(key, prefix) {
			delete(s.cache, key)
		}
	}
}

type cacheEntry struct {
	Data    []byte
	Expires time.Time
}
type rateEntry struct {
	Start time.Time
	Count int
}
