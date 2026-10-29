package music

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Options struct {
	MaxBytes, MaxItemBytes                     int64
	MaxEntries                                 int
	PrivateCache                               bool
	UserRequestsPerMinute, IPRequestsPerMinute int
}
type Query struct {
	Kind, Keywords, ID, Level, Mode string
	Page, Limit                     int
}
type rate struct {
	count   int
	expires time.Time
}
type Service struct {
	provider Provider
	cache    *cache
	o        Options
	mu       sync.Mutex
	rates    map[string]rate
	pruneAt  time.Time
	now      func() time.Time
}

func New(p Provider, o Options) (*Service, error) {
	if p == nil || o.MaxBytes < 0 || o.MaxItemBytes < 0 || o.MaxEntries < 0 || o.MaxItemBytes > o.MaxBytes || o.UserRequestsPerMinute < 1 || o.IPRequestsPerMinute < 1 {
		return nil, errors.New("invalid music cache or request limits")
	}
	return &Service{provider: p, cache: newCache(o.MaxBytes, o.MaxEntries, o.MaxItemBytes), o: o, rates: map[string]rate{}, now: time.Now}, nil
}
func jsonResponse(status int, body any) Response {
	b, e := json.Marshal(body)
	if e != nil {
		return Response{Status: 500, ContentType: "application/json; charset=utf-8", Body: []byte(`{"status":500,"message":"服务暂不可用"}`)}
	}
	return Response{Status: status, Body: b, ContentType: "application/json; charset=utf-8"}
}
func invalid(message string) Response {
	return jsonResponse(400, map[string]any{"status": 400, "error": message, "message": message})
}
func (s *Service) allow(user int64, ip string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !now.Before(s.pruneAt) {
		for key, v := range s.rates {
			if !now.Before(v.expires) {
				delete(s.rates, key)
			}
		}
		s.pruneAt = now.Add(time.Minute)
	}
	hash := sha256.Sum256([]byte(ip))
	keys := []string{"u:" + strconv.FormatInt(user, 10), "i:" + hex.EncodeToString(hash[:])}
	limits := []int{s.o.UserRequestsPerMinute, s.o.IPRequestsPerMinute}
	newEntries := 0
	for i, key := range keys {
		v, ok := s.rates[key]
		if !ok {
			newEntries++
		}
		if ok && now.Before(v.expires) && v.count >= limits[i] {
			retry := int(v.expires.Sub(now).Seconds()) + 1
			return false, retry
		}
	}
	if len(s.rates)+newEntries > 4096 {
		return false, 60
	}
	for _, key := range keys {
		v := s.rates[key]
		if !now.Before(v.expires) {
			v = rate{expires: now.Add(time.Minute)}
		}
		v.count++
		s.rates[key] = v
	}
	return true, 0
}
func normalize(q Query) (Query, error) {
	switch q.Kind {
	case "search":
		q.Keywords = strings.TrimSpace(q.Keywords)
		if q.Keywords == "" || utf8.RuneCountInString(q.Keywords) > 200 || !utf8.ValidString(q.Keywords) || strings.ContainsRune(q.Keywords, 0) || q.Page < 1 || q.Page > 1000 || q.Limit < 1 || q.Limit > 50 {
			return q, errors.New("keywords、page 或 limit 参数无效")
		}
	case "song", "lyric", "playlist", "album":
		id, e := ParseID(q.ID)
		if e != nil {
			return q, errors.New("歌曲、歌单或专辑 ID 无效")
		}
		q.ID = id
		if q.Kind == "song" && (!ValidLevel(q.Level) || q.Mode == "" || len(q.Mode) > 16) {
			return q, errors.New("level 或 type 参数无效")
		}
		if q.Kind == "playlist" && (q.Limit < 0 || q.Limit > 10000) {
			return q, errors.New("歌单 limit 需为0至10000")
		}
	default:
		return q, errors.New("请求无效")
	}
	return q, nil
}
func (s *Service) Execute(ctx context.Context, user int64, ip string, q Query, bypass bool) (Response, string) {
	if user <= 0 {
		return jsonResponse(401, map[string]any{"status": 401, "message": "请先登录"}), "BYPASS"
	}
	normalized, e := normalize(q)
	if e != nil {
		return invalid(e.Error()), "BYPASS"
	}
	q = normalized
	if ok, retry := s.allow(user, ip); !ok {
		r := jsonResponse(429, map[string]any{"status": 429, "code": "MUSIC_RATE_LIMITED", "message": "请求过于频繁，请稍后重试", "retryAfterSeconds": retry})
		r.RetryAfter = strconv.Itoa(retry)
		return r, "BYPASS"
	}
	keyBytes, _ := json.Marshal(q)
	namespace := "public:"
	if s.o.PrivateCache {
		namespace = "user:" + strconv.FormatInt(user, 10) + ":"
	}
	hash := sha256.Sum256(append([]byte(namespace), keyBytes...))
	key := hex.EncodeToString(hash[:])
	timeout := 20 * time.Second
	switch q.Kind {
	case "search":
		timeout = 12 * time.Second
	case "lyric":
		timeout = 7 * time.Second
	case "song":
		timeout = 15 * time.Second
	}
	response, cacheStatus := s.cache.load(ctx, key, bypass, timeout, func(work context.Context) Response { return s.build(work, q) })
	if ctx.Err() != nil {
		return failure(q.Kind, ErrTimeout), cacheStatus
	}
	return response, cacheStatus
}
func failure(kind string, e error) Response {
	timeout := errors.Is(e, ErrTimeout) || errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled)
	var upstream *UpstreamError
	known := errors.As(e, &upstream)
	if kind == "search" {
		status, code, message := 502, "SEARCH_UPSTREAM_FAILED", "搜索服务暂时不可用，请稍后重试"
		if timeout {
			status, code, message = 504, "SEARCH_TIMEOUT", "搜索超时，请重试"
		}
		return jsonResponse(status, map[string]any{"status": status, "code": code, "message": message})
	}
	if kind == "lyric" {
		status, retry := 502, timeout
		if known {
			status = upstream.Status
			retry = upstream.Retryable
		}
		r := jsonResponse(status, map[string]any{"status": status, "message": "歌词服务暂时不可用", "data": map[string]any{"state": "unavailable", "retryable": retry}})
		if known {
			r.RetryAfter = upstream.RetryAfter
		}
		return r
	}
	status, code, message := 502, "MUSIC_UPSTREAM_FAILED", "音乐服务暂时不可用，请稍后重试"
	if timeout {
		status, code, message = 504, "MUSIC_TIMEOUT", "音乐服务请求超时，请重试"
	}
	if errors.Is(e, ErrUnavailable) {
		status, code, message = 422, "SONG_UNAVAILABLE", "歌曲暂不可播放或当前平台账号无权限"
	}
	r := jsonResponse(status, map[string]any{"status": status, "code": code, "success": false, "msg": message})
	if known {
		r.RetryAfter = upstream.RetryAfter
	}
	return r
}

var songTemplate = template.Must(template.New("song").Parse(`歌曲名称：{{.Name}}<br>歌曲图片：{{.Pic}}<br>歌手：{{.Artist}}<br>歌曲专辑：{{.Album}}<br>歌曲音质：{{.Level}}<br>歌曲大小：{{.Size}}<br>音乐地址：{{.URL}}`))

func (s *Service) build(ctx context.Context, q Query) Response {
	switch q.Kind {
	case "search":
		result, e := s.provider.Search(ctx, q.Keywords, q.Page, q.Limit)
		if e != nil {
			return failure(q.Kind, e)
		}
		r := jsonResponse(200, struct {
			Status int `json:"status"`
			SearchPage
		}{200, result})
		r.Cacheable = true
		r.TTL = 30 * time.Second
		return r
	case "lyric":
		result, e := s.provider.Lyric(ctx, q.ID)
		if e != nil {
			return failure(q.Kind, e)
		}
		r := jsonResponse(200, map[string]any{"status": 200, "data": result})
		if result.State == "ready" {
			r.Cacheable = true
			r.TTL = 10 * time.Minute
		} else if result.State == "empty" {
			r.Cacheable = true
			r.TTL = 5 * time.Minute
		}
		return r
	case "song":
		result, e := s.provider.Song(ctx, q.ID, q.Level)
		if e != nil {
			return failure(q.Kind, e)
		}
		if q.Mode == "down" {
			return Response{Status: 302, Location: result.URL}
		}
		var r Response
		if q.Mode == "text" {
			var b bytes.Buffer
			if e = songTemplate.Execute(&b, result); e != nil {
				return failure(q.Kind, e)
			}
			r = Response{Status: 200, Body: b.Bytes(), ContentType: "text/html; charset=utf-8"}
		} else {
			r = jsonResponse(200, struct {
				Status int `json:"status"`
				Song
			}{200, result})
		}
		if result.ExpiresAt.After(time.Now().Add(5 * time.Second)) {
			ttl := time.Until(result.ExpiresAt) - 5*time.Second
			if ttl > 30*time.Second {
				ttl = 30 * time.Second
			}
			if ttl > 0 {
				r.Cacheable = true
				r.TTL = ttl
			}
		}
		return r
	case "playlist":
		result, e := s.provider.Playlist(ctx, q.ID, q.Limit)
		if e != nil {
			return failure(q.Kind, e)
		}
		r := jsonResponse(200, map[string]any{"status": 200, "success": true, "data": map[string]any{"playlist": result}})
		r.Cacheable = result.Complete
		r.TTL = 10 * time.Minute
		return r
	case "album":
		result, e := s.provider.Album(ctx, q.ID)
		if e != nil {
			return failure(q.Kind, e)
		}
		r := jsonResponse(200, map[string]any{"status": 200, "success": true, "data": map[string]any{"album": result}})
		r.Cacheable = true
		r.TTL = 15 * time.Minute
		return r
	}
	return invalid("请求无效")
}
