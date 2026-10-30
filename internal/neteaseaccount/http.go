package neteaseaccount

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/library"
	"cyapi-go/internal/requestbody"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type Handler struct {
	s     *Service
	guard func(http.HandlerFunc) http.HandlerFunc
}

func NewHandler(s *Service, guard func(http.HandlerFunc) http.HandlerFunc) *Handler {
	return &Handler{s, guard}
}
func (h *Handler) Register(mux *http.ServeMux) {
	for _, route := range []string{"GET /accounts/bindings", "POST /accounts/netease/bind-sessions", "GET /accounts/netease/bind-sessions/{id}", "POST /accounts/netease/bind-sessions/{id}/complete", "DELETE /accounts/netease/bind-sessions/{id}", "POST /accounts/netease/unbind", "GET /netease/user/playlists", "GET /recommend/songs", "GET /recommend/resource", "GET /personal_fm"} {
		mux.HandleFunc(route, h.guard(h.handle))
	}
}
func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	user := auth.CurrentUser(r.Context()).ID
	ctx := r.Context()
	var data any
	var e error
	switch r.Pattern {
	case "GET /accounts/bindings":
		data, e = h.s.Bindings(ctx, user)
	case "POST /accounts/netease/bind-sessions":
		var b map[string]any
		b, e = requestbody.Read(w, r, 20<<10)
		if e != nil {
			e = library.Fail(400, "INVALID_BODY", "请求格式无效或超过20 KiB")
		}
		if e == nil {
			method, _ := b["method"].(string)
			data, e = h.s.Start(ctx, user, method)
		}
	case "GET /accounts/netease/bind-sessions/{id}":
		data, e = h.s.Poll(ctx, user, r.PathValue("id"))
	case "POST /accounts/netease/bind-sessions/{id}/complete":
		var b map[string]any
		b, e = requestbody.Read(w, r, 20<<10)
		if e != nil {
			e = library.Fail(400, "INVALID_BODY", "请求格式无效或超过20 KiB")
		}
		if e == nil {
			cookie, _ := b["cookie"].(string)
			data, e = h.s.Complete(ctx, user, r.PathValue("id"), cookie)
		}
	case "DELETE /accounts/netease/bind-sessions/{id}":
		h.s.Cancel(user, r.PathValue("id"))
		data = map[string]any{"cancelled": true}
	case "POST /accounts/netease/unbind":
		e = h.s.Unbind(ctx, user)
		data = map[string]any{"unbound": true}
	case "GET /netease/user/playlists":
		limit, offset := 50, 0
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, e = strconv.Atoi(v)
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			var offsetErr error
			offset, offsetErr = strconv.Atoi(v)
			if offsetErr != nil {
				e = offsetErr
			}
		}
		if e != nil || limit < 1 || limit > 100 || offset < 0 || offset > 10000 {
			e = library.Fail(400, "INVALID_PAGE", "分页参数无效")
		} else {
			data, e = h.s.Playlists(ctx, user, limit, offset)
		}
	case "GET /recommend/songs":
		var out map[string]any
		out, e = h.s.request(ctx, user, "/v3/discovery/recommend/songs", map[string]any{"offset": 0, "limit": 30, "total": true})
		if e == nil {
			d, _ := out["data"].(map[string]any)
			songs, ok := d["dailySongs"].([]any)
			if !ok {
				e = platformError(errors.New("invalid songs"))
			} else {
				h.reply(w, 200, map[string]any{"code": 200, "recommend": songs})
				return
			}
		}
	case "GET /recommend/resource":
		var out map[string]any
		out, e = h.s.request(ctx, user, "/v1/discovery/recommend/resource", map[string]any{})
		if e == nil {
			items, ok := out["recommend"].([]any)
			if !ok {
				e = platformError(errors.New("invalid playlists"))
			} else {
				h.reply(w, 200, map[string]any{"code": 200, "recommend": items})
				return
			}
		}
	case "GET /personal_fm":
		var out map[string]any
		out, e = h.s.request(ctx, user, "/v1/radio/get", map[string]any{})
		if e == nil {
			data = out["data"]
			if _, ok := data.([]any); !ok {
				e = platformError(errors.New("invalid fm"))
			}
		}
	}
	if e != nil {
		status, code, msg := 500, "INTERNAL_ERROR", "操作失败，请稍后重试"
		var f *library.Failure
		if errors.As(e, &f) {
			status, code, msg = f.Status, f.Code, f.Message
		} else if errors.Is(e, context.DeadlineExceeded) {
			status, code, msg = 504, "REQUEST_TIMEOUT", "请求超时"
		}
		h.reply(w, status, map[string]any{"code": status, "error": code, "message": msg})
		return
	}
	h.reply(w, 200, map[string]any{"code": 200, "data": data})
}
func (h *Handler) reply(w http.ResponseWriter, status int, data any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func (s *Service) Poll(ctx context.Context, user int64, id string) (map[string]any, error) {
	s.mu.Lock()
	a, e := s.find(user, id)
	if e != nil {
		s.mu.Unlock()
		return nil, e
	}
	if a.Busy || time.Since(a.LastPoll) < 2*time.Second {
		out := map[string]any{"id": id, "status": a.State}
		s.mu.Unlock()
		return out, nil
	}
	if a.Method == "web" {
		s.mu.Unlock()
		return map[string]any{"id": id, "status": "waiting"}, nil
	}
	a.Busy = true
	a.LastPoll = time.Now()
	key := a.Key
	s.mu.Unlock()
	defer func() { s.mu.Lock(); a.Busy = false; s.mu.Unlock() }()
	if key == "" {
		out, _, e := s.platform.AccountRequest(ctx, "", "/login/qrcode/unikey", map[string]any{"type": 1})
		if e != nil {
			return nil, platformError(e)
		}
		d, _ := out["data"].(map[string]any)
		key, _ = d["unikey"].(string)
		if key == "" {
			key, _ = out["unikey"].(string)
		}
		if key == "" {
			return nil, platformError(errors.New("invalid qr"))
		}
		s.mu.Lock()
		if current, err := s.find(user, id); err != nil || current != a {
			s.mu.Unlock()
			return nil, library.Fail(410, "BIND_SESSION_EXPIRED", "登录已取消")
		}
		a.Key = key
		s.mu.Unlock()
	}
	out, cookie, e := s.platform.AccountRequest(ctx, "", "/login/qrcode/client/login", map[string]any{"key": key, "type": 1})
	if e != nil {
		return nil, platformError(e)
	}
	code, _ := out["code"].(float64)
	state := "waiting"
	switch int(code) {
	case 800:
		state = "expired"
	case 801:
		state = "waiting"
	case 802:
		state = "scanned"
	case 803:
		normalized, e := normalizeCookie(cookie)
		if e != nil {
			return nil, library.Fail(502, "NETEASE_UNAVAILABLE", "网易云未返回有效凭据")
		}
		profile, e := s.complete(ctx, user, a, normalized)
		if e != nil {
			return nil, e
		}
		return map[string]any{"id": id, "status": "bound", "profile": profile}, nil
	default:
		return nil, platformError(errors.New("invalid qr state"))
	}
	s.mu.Lock()
	a.State = state
	s.mu.Unlock()
	return map[string]any{"id": id, "status": state, "qrUrl": "https://music.163.com/login?codekey=" + key}, nil
}
func (s *Service) Playlists(ctx context.Context, user int64, limit, offset int) (map[string]any, error) {
	b, cookie, e := s.credentials(ctx, user)
	if e != nil {
		return nil, e
	}
	var profile map[string]any
	_ = json.Unmarshal([]byte(b.PayloadJSON), &profile)
	uid, _ := profile["uid"].(string)
	out, e := s.requestBinding(ctx, b, cookie, "/user/playlist", map[string]any{"uid": uid, "limit": limit, "offset": offset, "includeVideo": false})
	if e != nil {
		return nil, e
	}
	items, ok := out["playlist"].([]any)
	if !ok {
		return nil, platformError(errors.New("invalid playlists"))
	}
	result := make([]map[string]any, 0, len(items))
	for _, v := range items {
		item, ok := v.(map[string]any)
		if !ok {
			return nil, platformError(errors.New("invalid playlist"))
		}
		creator, _ := item["creator"].(map[string]any)
		result = append(result, map[string]any{"id": item["id"], "name": item["name"], "coverImgUrl": item["coverImgUrl"], "trackCount": item["trackCount"], "playCount": item["playCount"], "creator": creator["nickname"], "creatorId": creator["userId"], "description": item["description"], "subscribed": item["subscribed"]})
	}
	return map[string]any{"playlists": result, "more": out["more"], "revision": b.Revision}, nil
}
