package qq

import (
	"context"
	"cyapi-go/internal/music"
	p "cyapi-go/internal/providers/platformhttp"
	"net/url"
	"strings"
)

func (c *Client) Metadata(ctx context.Context, id string) (music.Metadata, error) {
	if !idPattern.MatchString(id) {
		return music.Metadata{}, music.ErrInvalidQuery
	}
	out, e := c.http.Do(ctx, "https://c.y.qq.com/v8/fcg-bin/fcg_play_single_song.fcg", url.Values{"platform": {"yqq"}, "format": {"json"}, "songmid": {id}})
	if e != nil {
		return music.Metadata{}, e
	}
	rows := p.Arr(out["data"])
	if !p.Code(out["code"], 0) || len(rows) != 1 {
		return music.Metadata{}, music.BadUpstream()
	}
	raw := p.Obj(rows[0])
	mid := p.Text(raw["mid"])
	if mid == "" {
		mid = p.Text(raw["songmid"])
	}
	if mid != id {
		return music.Metadata{}, music.BadUpstream()
	}
	title := p.Text(raw["name"])
	if title == "" {
		title = p.Text(raw["songname"])
	}
	names := []string{}
	for _, s := range p.Arr(raw["singer"]) {
		if name := p.Text(p.Obj(s)["name"]); name != "" {
			names = append(names, name)
		}
	}
	album := p.Text(p.Obj(raw["album"])["name"])
	if album == "" {
		album = p.Text(raw["albumname"])
	}
	seconds := p.Int(raw["interval"])
	if title == "" || len(names) == 0 || seconds < 0 || seconds > 86400 {
		return music.Metadata{}, music.BadUpstream()
	}
	return music.Metadata{ID: id, Title: title, Artist: strings.Join(names, "、"), Album: album, DurationMS: seconds * 1000, LXMetadata: music.ParseLXMetadata(mapped(raw)["lxMetadata"])}, nil
}
