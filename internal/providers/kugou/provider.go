package kugou

import (
	"context"
	"cyapi-go/internal/music"
	p "cyapi-go/internal/providers/platformhttp"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	http    *p.Client
	cookies map[string]string
}

func New(o p.Options) (*Client, error) {
	c, e := p.New(o, "complexsearch.kugou.com", "wwwapi.kugou.com")
	return &Client{c, p.Cookie(o.Cookie)}, e
}
func (c *Client) Close() { c.http.Close() }

var encodedID = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,512}$`)

func (c *Client) base() map[string]string {
	value := func(keys ...string) string {
		for _, k := range keys {
			if c.cookies[k] != "" {
				return c.cookies[k]
			}
		}
		return "undefined"
	}
	mid := value("mid", "kg_mid")
	return map[string]string{"appid": "1014", "clienttime": strconv.FormatInt(time.Now().UnixMilli(), 10), "dfid": value("kg_dfid", "dfid"), "mid": mid, "uuid": mid, "userid": value("KugooID"), "token": value("token", "t"), "srcappid": "2919"}
}
func signed(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	const salt = "NVPh5oo715z5DIWAeQlhMDsWXXQV4hwt"
	raw := salt
	form := url.Values{}
	for _, k := range keys {
		raw += k + "=" + params[k]
		form.Set(k, params[k])
	}
	raw += salt
	form.Set("signature", p.Hash([]byte(raw)))
	return form.Encode()
}
func (c *Client) search(ctx context.Context, q map[string]string) (map[string]any, error) {
	params := c.base()
	for k, v := range map[string]string{"clientver": "1000", "iscorrection": "1", "isfuzzy": "0", "keyword": q["keywords"], "page": q["page"], "pagesize": q["limit"], "platform": "WebFilter", "privilege_filter": "0"} {
		params[k] = v
	}
	out, e := c.http.Do(ctx, "https://complexsearch.kugou.com/v2/search/song?"+signed(params), nil)
	if e != nil {
		return nil, e
	}
	data := p.Obj(out["data"])
	raw, ok := data["lists"].([]any)
	if !p.Code(out["status"], 1) || !ok {
		return nil, music.BadUpstream()
	}
	limit, _ := strconv.Atoi(q["limit"])
	page, _ := strconv.Atoi(q["page"])
	if len(raw) > limit {
		return nil, music.BadUpstream()
	}
	songs, ids := []any{}, []any{}
	for _, v := range raw {
		s := p.Obj(v)
		if p.Text(s["SongName"]) == "" || p.Text(s["FileHash"]) == "" {
			return nil, music.BadUpstream()
		}
		songs = append(songs, map[string]any{"name": s["SongName"], "hash": s["FileHash"], "album": s["AlbumName"], "singer": s["SingerName"], "duration": s["Duration"], "album_id": s["AlbumID"], "album_audio_id": s["AlbumAudioID"], "emixsongid": s["EMixSongID"], "pic": strings.Replace(strings.Replace(p.Text(s["Image"]), "{size}", "400", -1), "http://", "https://", 1)})
		id := s["EMixSongID"]
		if p.Text(id) == "" || p.Text(id) == "0" {
			id = s["FileHash"]
		}
		ids = append(ids, id)
	}
	return p.Page(songs, ids, page, limit, data["total"]), nil
}
func (c *Client) song(ctx context.Context, id string) (map[string]any, error) {
	if !encodedID.MatchString(id) {
		return nil, music.ErrInvalidQuery
	}
	params := c.base()
	for _, k := range []string{"mid", "dfid", "userid", "token"} {
		if params[k] == "undefined" {
			return nil, &music.UpstreamError{Status: 503}
		}
	}
	params["clientver"] = "20000"
	params["platid"] = "4"
	params["encode_album_audio_id"] = id
	out, e := c.http.Do(ctx, "https://wwwapi.kugou.com/play/songinfo?"+signed(params), nil)
	if e != nil {
		return nil, e
	}
	data := p.Obj(out["data"])
	if !p.Code(out["status"], 1) || data["song_name"] == nil {
		return nil, music.BadUpstream()
	}
	address := p.Text(data["play_url"])
	if address != "" {
		safe, ok := p.SecureMedia(address, "kugou.com", "kugou.net", "kugou.cloud.cn")
		if !ok {
			return nil, music.BadUpstream()
		}
		address = safe
	}
	return map[string]any{"name": data["song_name"], "singer": data["author_name"], "album": data["album_name"], "pic": strings.Replace(p.Text(data["img"]), "http://", "https://", 1), "lyric": p.Text(data["lyrics"]), "url": address, "bitrate": data["bitrate"], "duration": p.Int(data["timelength"]) / 1000}, nil
}
func (c *Client) Request(ctx context.Context, kind string, q map[string]string) (map[string]any, error) {
	if kind == "search" {
		return c.search(ctx, q)
	}
	song, e := c.song(ctx, q["emixsongid"])
	if e != nil {
		return nil, e
	}
	if kind == "song" {
		return map[string]any{"status": 200, "song": song}, nil
	}
	if kind != "lyric" {
		return nil, music.ErrInvalidQuery
	}
	state := "ready"
	if strings.TrimSpace(p.Text(song["lyric"])) == "" {
		state = "unavailable"
	}
	return map[string]any{"status": 200, "data": map[string]any{"state": state, "retryable": state == "unavailable", "lyric": song["lyric"], "tlyric": ""}}, nil
}
