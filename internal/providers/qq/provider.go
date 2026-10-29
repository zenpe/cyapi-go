package qq

import (
	"context"
	"cyapi-go/internal/library"
	"cyapi-go/internal/music"
	p "cyapi-go/internal/providers/platformhttp"
	"encoding/base64"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type Client struct {
	http   *p.Client
	cookie string
}

func New(o p.Options) (*Client, error) {
	c, e := p.New(o, "u.y.qq.com", "c.y.qq.com")
	return &Client{c, o.Cookie}, e
}
func (c *Client) Close() { c.http.Close() }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
var pathID = regexp.MustCompile(`/(?:songDetail|song)/([A-Za-z0-9]+)(?:\.html)?`)

func parseID(value string) (string, error) {
	if strings.Contains(value, "://") {
		u, e := url.Parse(value)
		if e != nil || u.Host != "y.qq.com" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return "", music.ErrInvalidQuery
		}
		value = u.Query().Get("songmid")
		if value == "" {
			m := pathID.FindStringSubmatch(u.Path)
			if len(m) > 1 {
				value = m[1]
			}
		}
	}
	if !idPattern.MatchString(value) {
		return "", music.ErrInvalidQuery
	}
	return value, nil
}
func (c *Client) rpc(ctx context.Context, module, method string, param map[string]any) (map[string]any, error) {
	comm := map[string]any{"format": "json", "ct": 24, "cv": 0}
	if module == "music.search.SearchCgiService" {
		comm["ct"] = 19
		comm["cv"] = "1859"
		comm["uin"] = "0"
	}
	if module == "vkey.GetVkeyServer" {
		comm["uin"] = param["uin"]
	}
	out, e := c.http.Do(ctx, "https://u.y.qq.com/cgi-bin/musicu.fcg", map[string]any{"req_1": map[string]any{"module": module, "method": method, "param": param}, "comm": comm})
	if e != nil {
		return nil, e
	}
	req := p.Obj(out["req_1"])
	if !p.Code(out["code"], 0) || !p.Code(req["code"], 0) || req["data"] == nil {
		return nil, music.BadUpstream()
	}
	return p.Obj(req["data"]), nil
}
func mapped(item map[string]any) map[string]any {
	singers := []string{}
	switch v := item["singer"].(type) {
	case []any:
		for _, s := range v {
			singers = append(singers, p.Text(p.Obj(s)["name"]))
		}
	case string:
		singers = append(singers, v)
	}
	if len(singers) == 0 {
		singers = append(singers, p.Text(item["singerName"]))
	}
	a := p.Obj(item["album"])
	cover := p.Text(item["pic"])
	if cover == "" && p.Text(a["mid"]) != "" {
		cover = "https://y.qq.com/music/photo_new/T002R800x800M000" + p.Text(a["mid"]) + ".jpg?max_age=2592000"
	}
	if cover == "" {
		cover = "https://y.qq.com/mediastyle/global/img/album_300.png"
	}
	album := p.Text(a["name"])
	if album == "" {
		album = p.Text(item["albumName"])
	}
	return map[string]any{"name": p.Text(item["name"]), "album": album, "singer": strings.Join(singers, ", "), "pic": strings.Replace(cover, "http://", "https://", 1), "mid": p.Text(item["mid"]), "id": item["id"]}
}
func (c *Client) search(ctx context.Context, q map[string]string) (map[string]any, error) {
	page, _ := strconv.Atoi(q["page"])
	limit, _ := strconv.Atoi(q["limit"])
	data, e := c.rpc(ctx, "music.search.SearchCgiService", "DoSearchForQQMusicDesktop", map[string]any{"grp": 1, "search_type": 0, "query": q["keywords"], "page_num": page, "num_per_page": limit})
	songs, ids := []any{}, []any{}
	if e == nil {
		song := p.Obj(p.Obj(data["body"])["song"])
		raw, ok := song["list"].([]any)
		if !ok || len(raw) > limit {
			e = music.BadUpstream()
		} else {
			for _, v := range raw {
				item := p.Obj(v)
				m := mapped(item)
				id := item["mid"]
				if p.Text(id) == "" {
					id = item["id"]
				}
				ids = append(ids, id)
				if m["mid"] != "" {
					songs = append(songs, m)
				}
			}
			if len(raw) > 0 || page > 1 {
				return p.Page(songs, ids, page, limit, nil), nil
			}
		}
	}
	if page > 1 {
		return nil, e
	}
	primaryError := e
	smart, e := c.http.Do(ctx, "https://c.y.qq.com/splcloud/fcgi-bin/smartbox_new.fcg?"+url.Values{"key": {q["keywords"]}}.Encode(), nil)
	if e != nil {
		return nil, e
	}
	raw, ok := p.Obj(p.Obj(smart["data"])["song"])["itemlist"].([]any)
	if !p.Code(smart["code"], 0) || !ok {
		return nil, music.BadUpstream()
	}
	songs = []any{}
	for _, v := range raw {
		m := mapped(p.Obj(v))
		if m["mid"] != "" {
			songs = append(songs, m)
		}
		if len(songs) >= limit {
			break
		}
	}
	if len(songs) == 0 && primaryError != nil {
		return nil, primaryError
	}
	out := p.Page(songs, []any{}, page, limit, nil)
	out["resultKind"] = "suggestions"
	out["hasMore"] = false
	return out, nil
}
func (c *Client) details(ctx context.Context, id string) (map[string]any, error) {
	form := url.Values{"platform": {"yqq"}, "format": {"json"}}
	if _, e := strconv.ParseInt(id, 10, 64); e == nil {
		form.Set("songid", id)
	} else {
		form.Set("songmid", id)
	}
	out, e := c.http.Do(ctx, "https://c.y.qq.com/v8/fcg-bin/fcg_play_single_song.fcg", form)
	if e != nil {
		return nil, e
	}
	items := p.Arr(out["data"])
	if !p.Code(out["code"], 0) || len(items) != 1 {
		return nil, music.BadUpstream()
	}
	info := mapped(p.Obj(items[0]))
	if info["name"] == "" || info["mid"] == "" || p.Int(info["id"]) <= 0 {
		return nil, music.BadUpstream()
	}
	return info, nil
}
func (c *Client) lyric(ctx context.Context, id any) (map[string]any, error) {
	const key = "music.musichallSong.PlayLyricInfo.GetPlayLyricInfo"
	out, e := c.http.Do(ctx, "https://u.y.qq.com/cgi-bin/musicu.fcg", map[string]any{key: map[string]any{"module": "music.musichallSong.PlayLyricInfo", "method": "GetPlayLyricInfo", "param": map[string]any{"songID": id, "trans": 1, "roma": 1}}})
	if e != nil {
		return nil, e
	}
	req := p.Obj(out[key])
	if !p.Code(out["code"], 0) || !p.Code(req["code"], 0) || req["data"] == nil {
		return nil, music.BadUpstream()
	}
	data := p.Obj(req["data"])
	decode := func(v any) (string, error) {
		s := p.Text(v)
		if s == "" {
			return "", nil
		}
		b, e := base64.StdEncoding.DecodeString(s)
		return string(b), e
	}
	l, e := decode(data["lyric"])
	if e != nil {
		return nil, music.BadUpstream()
	}
	tl, e := decode(data["trans"])
	if e != nil {
		return nil, music.BadUpstream()
	}
	state := "ready"
	if strings.TrimSpace(l) == "" {
		state = "unavailable"
	}
	return map[string]any{"state": state, "retryable": state == "unavailable", "lyric": l, "tlyric": tl}, nil
}
func (c *Client) address(ctx context.Context, mid, quality string) (map[string]any, error) {
	prefix, ext, bitrate := "M500", ".mp3", "128kbps"
	if quality == "320" {
		prefix, bitrate = "M800", "320kbps"
	}
	if quality == "flac" {
		prefix, ext, bitrate = "F000", ".flac", "FLAC"
	}
	cookies := p.Cookie(c.cookie)
	uin := cookies["uin"]
	if uin == "" {
		uin = cookies["euin"]
	}
	if uin == "" {
		uin = "0"
	}
	out, e := c.rpc(ctx, "vkey.GetVkeyServer", "CgiGetVkey", map[string]any{"filename": []string{prefix + mid + mid + ext}, "guid": "10000", "songmid": []string{mid}, "songtype": []int{0}, "uin": uin, "loginflag": 1, "platform": "20"})
	if e != nil {
		return nil, e
	}
	urls := p.Arr(out["midurlinfo"])
	if len(urls) == 0 || p.Text(p.Obj(urls[0])["purl"]) == "" {
		return nil, nil
	}
	path := p.Text(p.Obj(urls[0])["purl"])
	for _, sip := range p.Arr(out["sip"]) {
		address, ok := p.SecureMedia(p.Text(sip)+path, "qq.com", "qqmusic.qq.com", "qqmusic.tc.qq.com")
		if ok {
			return map[string]any{"url": address, "bitrate": bitrate}, nil
		}
	}
	return nil, music.BadUpstream()
}
func (c *Client) playlist(ctx context.Context, id string) (map[string]any, error) {
	n, e := strconv.ParseInt(id, 10, 64)
	if e != nil || n <= 0 || n > music.MaxID {
		return nil, music.ErrInvalidQuery
	}
	out, e := c.rpc(ctx, "srf_diss_info.DissInfoServer", "CgiGetDiss", map[string]any{"disstid": n, "onlysonglist": 0, "song_begin": 0, "song_num": 1000})
	if e != nil {
		return nil, e
	}
	dir := p.Obj(out["dirinfo"])
	raw, ok := out["songlist"].([]any)
	if dir["title"] == nil || !ok || len(raw) > 1000 {
		return nil, music.BadUpstream()
	}
	expected := p.Int(dir["songnum"])
	if dir["songnum"] == nil || expected < 0 || expected > 10000 {
		return nil, music.BadUpstream()
	}
	tracks := []any{}
	for i, v := range raw {
		m := mapped(p.Obj(v))
		mid := p.Text(m["mid"])
		if !idPattern.MatchString(mid) || m["name"] == "" {
			return nil, music.BadUpstream()
		}
		tracks = append(tracks, map[string]any{"id": mid, "name": m["name"], "artists": m["singer"], "album": m["album"], "picUrl": m["pic"], "sourcePosition": i})
	}
	return map[string]any{"id": dir["id"], "name": dir["title"], "coverImgUrl": dir["picurl"], "creator": dir["nick"], "trackCount": len(tracks), "expectedTrackCount": expected, "resolvedTrackCount": len(tracks), "complete": int64(len(tracks)) == expected, "tracks": tracks}, nil
}
func (c *Client) Request(ctx context.Context, kind string, q map[string]string) (map[string]any, error) {
	if kind == "search" {
		return c.search(ctx, q)
	}
	if kind == "playlist" {
		p, e := c.playlist(ctx, q["id"])
		return map[string]any{"status": 200, "success": true, "data": map[string]any{"playlist": p}}, e
	}
	id := q["ids"]
	if id == "" {
		id = q["id"]
	}
	if id == "" {
		id = q["url"]
	}
	id, e := parseID(id)
	if e != nil {
		return nil, e
	}
	info, e := c.details(ctx, id)
	if e != nil {
		return nil, e
	}
	if kind == "lyric" {
		l, e := c.lyric(ctx, info["id"])
		return map[string]any{"status": 200, "data": l}, e
	}
	if kind != "song" {
		return nil, music.ErrInvalidQuery
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	urls := map[string]any{}
	var first error
	for _, quality := range []string{"flac", "320", "128"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			address, err := c.address(ctx, p.Text(info["mid"]), quality)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && first == nil {
				first = err
			}
			if address != nil {
				urls[quality] = address
			}
		}()
	}
	wg.Wait()
	if len(urls) == 0 && first != nil {
		return nil, first
	}
	lyric, e := c.lyric(ctx, info["id"])
	if e != nil {
		lyric = map[string]any{"lyric": "", "tlyric": ""}
	}
	return map[string]any{"status": 200, "song": info, "lyric": map[string]any{"lyric": lyric["lyric"], "tylyric": lyric["tlyric"]}, "music_urls": urls}, nil
}
func (c *Client) FetchSnapshot(ctx context.Context, source, id string) (library.RemoteSnapshot, error) {
	if source != "qq" {
		return library.RemoteSnapshot{}, music.ErrInvalidQuery
	}
	body, e := c.playlist(ctx, id)
	if e != nil {
		return library.RemoteSnapshot{}, e
	}
	s := library.RemoteSnapshot{Name: p.Text(body["name"]), ExpectedCount: int(body["expectedTrackCount"].(int64)), Complete: body["complete"] == true}
	for _, v := range body["tracks"].([]any) {
		t := v.(map[string]any)
		s.Tracks = append(s.Tracks, library.RemoteTrack{Track: library.Track{TrackID: p.Text(t["id"]), Name: p.Text(t["name"]), Artists: p.Text(t["artists"]), Album: p.Text(t["album"]), PicURL: p.Text(t["picUrl"]), Source: "qq"}, SourcePosition: t["sourcePosition"].(int)})
	}
	return s, nil
}

var _ music.Extension = (*Client)(nil)
var _ library.RemoteProvider = (*Client)(nil)
