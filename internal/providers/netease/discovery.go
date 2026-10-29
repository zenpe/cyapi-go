package netease

import (
	"context"
	"cyapi-go/internal/music"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func (c *Client) tags(ctx context.Context) ([]map[string]any, error) {
	var out struct {
		Code int
		Tags []map[string]any
	}
	e := c.request(ctx, "https://music.163.com/api/playlist/highquality/tags", nil, &out)
	if e != nil {
		return nil, e
	}
	if out.Code != 200 || out.Tags == nil {
		return nil, music.BadUpstream()
	}
	return out.Tags, nil
}
func (c *Client) list(ctx context.Context, q map[string]string) (map[string]any, error) {
	cat := q["cat"]
	if cat == "" {
		cat = q["category"]
	}
	if cat == "" || cat == "全部歌单" {
		cat = "全部"
	}
	if utf8.RuneCountInString(cat) > 30 || strings.IndexFunc(cat, func(r rune) bool { return r < 32 }) >= 0 {
		return nil, music.ErrInvalidQuery
	}
	limit, offset := 30, 0
	for key, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw, exists := q[key]; exists {
			v, e := strconv.Atoi(raw)
			if e != nil {
				return nil, music.ErrInvalidQuery
			}
			*target = v
		}
	}
	if q["__discover"] == "1" {
		limit = 30
		if cursor := q["cursor"]; cursor != "" {
			data, e := base64.RawURLEncoding.DecodeString(cursor)
			if e != nil {
				return nil, music.ErrInvalidQuery
			}
			var parsed struct {
				Offset   int    `json:"offset"`
				Revision string `json:"revision,omitempty"`
			}
			if json.Unmarshal(data, &parsed) != nil {
				return nil, music.ErrInvalidQuery
			}
			offset = parsed.Offset
		}
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, music.ErrInvalidQuery
	}
	order := q["order"]
	if order == "" {
		order = "hot"
	}
	if order != "hot" && order != "new" {
		return nil, music.ErrInvalidQuery
	}
	query := url.Values{"cat": {cat}, "limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}, "order": {order}}
	var out struct {
		Code      int
		Playlists []map[string]any
		Total     int64
		More      bool
	}
	e := c.request(ctx, "https://music.163.com/api/playlist/list?"+query.Encode(), nil, &out)
	if e != nil {
		return nil, e
	}
	if out.Code != 200 || out.Playlists == nil || out.Total < 0 || len(out.Playlists) > limit {
		return nil, music.BadUpstream()
	}
	for _, p := range out.Playlists {
		if cover, ok := p["coverImgUrl"].(string); ok {
			p["coverImgUrl"] = strings.Replace(cover, "http://", "https://", 1)
		}
	}
	if q["__discover"] == "1" {
		var next *string
		if out.More {
			b, _ := json.Marshal(map[string]int{"offset": offset + limit})
			v := base64.RawURLEncoding.EncodeToString(b)
			next = &v
		}
		return map[string]any{"status": 200, "items": out.Playlists, "total": out.Total, "hasMore": out.More, "nextCursor": next}, nil
	}
	return map[string]any{"status": 200, "playlists": out.Playlists, "total": out.Total, "more": out.More}, nil
}
func (c *Client) charts(ctx context.Context, limit int) ([]any, error) {
	entries := []struct{ ID, Name string }{{"19723756", "soaring"}, {"3779629", "new"}, {"2884035", "original"}, {"3778678", "hot"}}
	results := make([]any, 4)
	var wg sync.WaitGroup
	for i, v := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := c.Playlist(ctx, v.ID, limit)
			if e != nil {
				return
			}
			b, _ := json.Marshal(p)
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			body["name_en"] = v.Name
			results[i] = body
		}()
	}
	wg.Wait()
	out := []any{}
	for _, v := range results {
		if v != nil {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, music.BadUpstream()
	}
	return out, nil
}
func (c *Client) Request(ctx context.Context, kind string, q map[string]string) (map[string]any, error) {
	switch kind {
	case "tags", "discover-tags":
		tags, e := c.tags(ctx)
		key := "tags"
		if kind == "discover-tags" {
			key = "items"
		}
		return map[string]any{"status": 200, key: tags}, e
	case "playlists", "discover":
		if kind == "discover" {
			q["__discover"] = "1"
		}
		return c.list(ctx, q)
	case "toplists", "home":
		limit := 20
		if kind == "home" {
			limit = 12
		}
		charts, e := c.charts(ctx, limit)
		if kind == "home" {
			return map[string]any{"status": 200, "generatedAt": time.Now().UnixMilli(), "partial": len(charts) != 4, "sections": charts}, e
		}
		return map[string]any{"status": 200, "toplists": charts}, e
	case "artist":
		var out struct {
			Code   int
			Result *struct {
				Artists []struct {
					ID                      int64
					Name, PicURL, Img1v1URL string
					Alias                   []string
					AlbumSize, MusicSize    int
				}
				ArtistCount int
			}
		}
		e := c.request(ctx, "https://music.163.com/api/cloudsearch/pc", url.Values{"s": {q["keywords"]}, "type": {"100"}, "limit": {q["limit"]}}, &out)
		if e != nil {
			return nil, e
		}
		if out.Code != 200 || out.Result == nil || out.Result.Artists == nil && out.Result.ArtistCount != 0 {
			return nil, music.BadUpstream()
		}
		artists := []any{}
		for _, a := range out.Result.Artists {
			if a.ID <= 0 || a.ID > music.MaxID || strings.TrimSpace(a.Name) == "" {
				return nil, music.BadUpstream()
			}
			cover := a.PicURL
			if cover == "" {
				cover = a.Img1v1URL
			}
			alias := a.Alias
			if alias == nil {
				alias = []string{}
			}
			artists = append(artists, map[string]any{"id": a.ID, "name": a.Name, "picUrl": strings.Replace(cover, "http://", "https://", 1), "alias": alias, "albumSize": a.AlbumSize, "musicSize": a.MusicSize})
		}
		return map[string]any{"status": 200, "result": artists}, nil
	}
	return nil, music.ErrInvalidQuery
}
