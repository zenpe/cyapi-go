package music

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/server"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrInvalidQuery = errors.New("invalid platform parameters")

type Extension interface {
	Request(context.Context, string, map[string]string) (map[string]any, error)
}

func (s *Service) RegisterExtensions(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc, providers map[string]Extension) {
	specs := map[string]string{"GET /qq/search": "qq/search", "GET /qq/song": "qq/song", "GET /qq/playlist": "qq/playlist", "GET /lyrics/qq": "qq/lyric", "GET /kugou/search": "kugou/search", "GET /kugou/song": "kugou/song", "GET /lyrics/kugou": "kugou/lyric", "GET /toplists": "netease/toplists", "GET /netease/top/playlist": "netease/playlists", "GET /netease/playlist/highquality/tags": "netease/tags", "POST /artist/search": "netease/artist", "GET /v1/home/charts": "netease/home", "GET /v1/discover/playlists": "netease/discover", "GET /v1/discover/tags": "netease/discover-tags"}
	for pattern, spec := range specs {
		mux.HandleFunc(pattern, guard(func(w http.ResponseWriter, r *http.Request) {
			raw, e := parameters(w, r)
			if e != nil {
				write(w, invalid("请求格式无效"), "BYPASS")
				return
			}
			p := map[string]string{}
			for _, key := range []string{"keywords", "page", "limit", "id", "ids", "url", "emixsongid", "cat", "category", "offset", "order", "cursor"} {
				if v, ok := raw[key]; ok {
					value, valid := scalar(v)
					if !valid || len(value) > 4096 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
						write(w, invalid("参数无效"), "BYPASS")
						return
					}
					p[key] = strings.TrimSpace(value)
				}
			}
			if strings.HasSuffix(spec, "/search") || strings.HasSuffix(spec, "/artist") {
				if p["keywords"] == "" || utf8.RuneCountInString(p["keywords"]) > 200 {
					write(w, invalid("keywords 无效"), "BYPASS")
					return
				}
				for k, def := range map[string]int{"page": 1, "limit": 20} {
					v := def
					if raw, ok := p[k]; ok {
						n, err := strconv.Atoi(raw)
						if err != nil {
							write(w, invalid("分页参数无效"), "BYPASS")
							return
						}
						v = n
					}
					max := 1000
					if k == "limit" {
						max = 50
					}
					if v < 1 || v > max {
						write(w, invalid("分页参数无效"), "BYPASS")
						return
					}
					p[k] = strconv.Itoa(v)
				}
			}
			user := auth.CurrentUser(r.Context()).ID
			if allowed, retry := s.allow(user, server.ClientIP(r.Context())); !allowed {
				out := jsonResponse(429, map[string]any{"status": 429, "code": "RATE_LIMITED", "message": "请求过于频繁"})
				out.RetryAfter = strconv.Itoa(retry)
				write(w, out, "BYPASS")
				return
			}
			parts := strings.SplitN(spec, "/", 2)
			provider := providers[parts[0]]
			if provider == nil {
				write(w, jsonResponse(503, map[string]any{"status": 503, "message": "平台未配置"}), "BYPASS")
				return
			}
			keyBytes, _ := json.Marshal(p)
			key := "extra:" + spec + ":" + strconv.FormatInt(user, 10) + ":" + string(keyBytes)
			budget := 20 * time.Second
			if parts[1] == "search" || parts[1] == "artist" {
				budget = 12 * time.Second
			}
			if parts[1] == "lyric" {
				budget = 7 * time.Second
			}
			control := strings.ToLower(r.Header.Get("Cache-Control"))
			out, label := s.cache.load(r.Context(), key, strings.Contains(control, "no-cache") || strings.Contains(control, "no-store"), budget, func(ctx context.Context) Response {
				body, err := provider.Request(ctx, parts[1], p)
				if err != nil {
					if errors.Is(err, ErrInvalidQuery) {
						return invalid("参数无效")
					}
					status, code := 502, "UPSTREAM_UNAVAILABLE"
					if errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
						status, code = 504, "UPSTREAM_TIMEOUT"
					}
					var up *UpstreamError
					if errors.As(err, &up) && up.Status == 429 {
						status = 429
					}
					if parts[1] == "search" || parts[1] == "artist" {
						code = "SEARCH_UPSTREAM_FAILED"
						if status == 504 {
							code = "SEARCH_TIMEOUT"
						}
						if status == 429 {
							status = 502
						}
					}
					failure := map[string]any{"status": status, "code": code, "message": "平台服务暂时不可用"}
					if parts[1] == "lyric" {
						failure["data"] = map[string]any{"state": "unavailable", "retryable": status == 429 || status == 504 || up != nil && up.Retryable}
					}
					res := jsonResponse(status, failure)
					if status == 429 && up != nil {
						res.RetryAfter = up.RetryAfter
					}
					return res
				}
				res := jsonResponse(200, body)
				res.TTL = 5 * time.Minute
				res.Cacheable = true
				switch parts[1] {
				case "song":
					res.Cacheable = false
				case "search", "artist":
					res.TTL = 30 * time.Second
				case "lyric":
					res.TTL = 10 * time.Minute
					if data, ok := body["data"].(map[string]any); !ok || data["state"] != "ready" && data["state"] != "empty" {
						res.Cacheable = false
					}
				case "playlist":
					data, _ := body["data"].(map[string]any)
					playlist, _ := data["playlist"].(map[string]any)
					res.Cacheable = playlist["complete"] == true
				case "home", "toplists":
					res.Cacheable = false
				}
				return res
			})
			write(w, out, label)
		}))
	}
}
