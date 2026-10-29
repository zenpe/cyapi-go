// Package userstate owns playback, listening statistics and account profile operations.
package userstate

import (
	"context"
	"cyapi-go/internal/library"
	"cyapi-go/internal/requestbody"
	"encoding/json"
	"math"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Store interface {
	Execute(context.Context, int64, string, map[string]any) (library.Result, error)
}
type Service struct{ store Store }

func New(s Store) *Service { return &Service{s} }

var username = regexp.MustCompile(`^[a-zA-Z0-9_]{4,20}$`)

func bad() error { return library.Fail(400, "INVALID_PARAMETERS", "参数无效或超出限制") }
func (s *Service) Execute(ctx context.Context, user int64, kind string, b map[string]any) (library.Result, error) {
	if user <= 0 {
		return library.Result{}, library.Fail(401, "UNAUTHORIZED", "请先登录")
	}
	for _, key := range []string{"trackId", "source", "trackName", "artists", "album", "picUrl", "platform", "location", "newUsername"} {
		if v, ok := b[key]; ok {
			value := requestbody.Text(v)
			switch v.(type) {
			case string, json.Number:
			default:
				return library.Result{}, bad()
			}
			max := 8192
			if key == "trackId" {
				max = 512
			}
			if key == "source" || key == "platform" {
				max = 64
			}
			if !utf8.ValidString(value) || len(value) > max || strings.ContainsRune(value, 0) {
				return library.Result{}, bad()
			}
			b[key] = value
		}
	}
	switch kind {
	case "save", "play-count", "song-memory":
		if strings.TrimSpace(requestbody.Text(b["trackId"])) == "" || strings.TrimSpace(requestbody.Text(b["source"])) == "" {
			return library.Result{}, bad()
		}
	}
	switch kind {
	case "listening-time":
		v, e := strconv.ParseFloat(requestbody.Text(b["seconds"]), 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 86400 {
			return library.Result{}, bad()
		}
		b["seconds"] = v
	case "save":
		v := int64(0)
		if raw, ok := b["position"]; ok {
			var e error
			v, e = strconv.ParseInt(requestbody.Text(raw), 10, 64)
			if e != nil || v < 0 || v > 604800 {
				return library.Result{}, bad()
			}
		}
		b["position"] = v
		flag := 0
		switch x := b["isFromPlaylist"].(type) {
		case bool:
			if x {
				flag = 1
			}
		case string:
			if x == "true" || x == "1" {
				flag = 1
			} else if x != "false" && x != "0" && x != "" {
				return library.Result{}, bad()
			}
		case nil:
		default:
			return library.Result{}, bad()
		}
		b["isFromPlaylist"] = flag
	case "records":
		for key, def := range map[string]int{"page": 1, "limit": 20} {
			v := def
			raw, exists := b[key]
			if exists {
				n, e := strconv.Atoi(requestbody.Text(raw))
				if e != nil {
					return library.Result{}, bad()
				}
				v = n
			}
			max := 100000
			if key == "limit" {
				max = 50
			}
			if v < 1 || v > max {
				return library.Result{}, bad()
			}
			b[key] = v
		}
	case "username":
		name := strings.TrimSpace(requestbody.Text(b["newUsername"]))
		if !username.MatchString(name) {
			return library.Result{}, bad()
		}
		b["newUsername"] = name
	case "location":
		if _, e := netip.ParseAddr(requestbody.Text(b["ip"])); e != nil || strings.TrimSpace(requestbody.Text(b["location"])) == "" {
			return library.Result{}, bad()
		}
	}
	return s.store.Execute(ctx, user, kind, b)
}
