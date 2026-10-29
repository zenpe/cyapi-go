package netease

import (
	"context"
	"crypto/md5"
	"cyapi-go/internal/music"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type rawSong struct {
	ID      int64                   `json:"id"`
	Name    string                  `json:"name"`
	Artists []struct{ Name string } `json:"ar"`
	Album   struct {
		Name   string
		PicURL string `json:"picUrl"`
	} `json:"al"`
	Duration int64 `json:"dt"`
}

func (s rawSong) track() (music.Track, error) {
	if s.ID <= 0 || s.ID > music.MaxID || strings.TrimSpace(s.Name) == "" || s.Duration < 0 {
		return music.Track{}, music.BadUpstream()
	}
	names := []string{}
	for _, a := range s.Artists {
		names = append(names, a.Name)
	}
	return music.Track{ID: s.ID, Name: s.Name, Artists: strings.Join(names, "/"), Album: s.Album.Name, PicURL: strings.Replace(s.Album.PicURL, "http://", "https://", 1)}, nil
}
func (c *Client) Search(ctx context.Context, keywords string, page, limit int) (music.SearchPage, error) {
	var out struct {
		Code   int
		Result *struct {
			Songs     []rawSong
			SongCount *int64
		}
	}
	e := c.request(ctx, "https://music.163.com/api/cloudsearch/pc", url.Values{"s": {keywords}, "type": {"1"}, "limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa((page - 1) * limit)}}, &out)
	if e != nil {
		return music.SearchPage{}, e
	}
	if out.Code != 200 || out.Result == nil || out.Result.Songs == nil && (out.Result.SongCount == nil || *out.Result.SongCount != 0) {
		return music.SearchPage{}, music.BadUpstream()
	}
	if len(out.Result.Songs) > limit {
		return music.SearchPage{}, music.BadUpstream()
	}
	result := music.SearchPage{Songs: []music.Track{}, Page: page, ResultKind: "full"}
	ids := []int64{}
	for _, song := range out.Result.Songs {
		track, e := song.track()
		if e != nil {
			return result, e
		}
		result.Songs = append(result.Songs, track)
		ids = append(ids, song.ID)
	}
	if len(ids) > 0 {
		b, _ := json.Marshal(ids)
		hash := md5.Sum(b)
		fingerprint := hex.EncodeToString(hash[:])
		result.PageFingerprint = &fingerprint
		result.HasMore = len(ids) >= limit
		if total := out.Result.SongCount; total != nil && *total >= 0 && *total <= music.MaxID {
			result.HasMore = int64(page*limit) < *total
		}
	}
	return result, nil
}
func (c *Client) details(ctx context.Context, ids []int64) (map[int64]rawSong, error) {
	type wanted struct {
		ID int64 `json:"id"`
		V  int   `json:"v"`
	}
	payload := make([]wanted, 0, len(ids))
	for _, id := range ids {
		payload = append(payload, wanted{ID: id})
	}
	b, _ := json.Marshal(payload)
	var out struct {
		Code  int
		Songs []rawSong
	}
	e := c.request(ctx, "https://interface3.music.163.com/api/v3/song/detail", url.Values{"c": {string(b)}}, &out)
	if e != nil {
		return nil, e
	}
	if out.Code != 200 || out.Songs == nil {
		return nil, music.BadUpstream()
	}
	result := map[int64]rawSong{}
	for _, song := range out.Songs {
		if _, e = song.track(); e != nil {
			return nil, e
		}
		result[song.ID] = song
	}
	return result, nil
}
func (c *Client) Lyric(ctx context.Context, id string) (music.Lyric, error) {
	var out struct {
		Code    int
		NoLyric bool                   `json:"nolyric"`
		LRC     struct{ Lyric string } `json:"lrc"`
		TLyric  struct{ Lyric string } `json:"tlyric"`
	}
	e := c.request(ctx, "https://interface3.music.163.com/api/song/lyric", url.Values{"id": {id}, "lv": {"-1"}, "tv": {"-1"}}, &out)
	if e != nil {
		return music.Lyric{}, e
	}
	if out.Code != 200 {
		return music.Lyric{}, music.BadUpstream()
	}
	result := music.Lyric{State: "unavailable", Retryable: true, Lyric: out.LRC.Lyric, TLyric: out.TLyric.Lyric}
	if strings.TrimSpace(result.Lyric) != "" {
		result.State = "ready"
		result.Retryable = false
	} else if out.NoLyric {
		result.State = "empty"
		result.Retryable = false
	}
	return result, nil
}
func (c *Client) Playlist(ctx context.Context, id string, limit int) (music.Playlist, error) {
	var out struct {
		Code     int
		Playlist *struct {
			ID                                int64
			Name                              string
			Cover                             string `json:"coverImgUrl"`
			Creator                           struct{ Nickname string }
			TrackCount                        *int
			Description                       string
			Tags                              []string
			PlayCount, CreateTime, UpdateTime int64
			TrackIDs                          []struct{ ID int64 } `json:"trackIds"`
		}
	}
	e := c.request(ctx, "https://music.163.com/api/v6/playlist/detail", url.Values{"id": {id}}, &out)
	if e != nil {
		return music.Playlist{}, e
	}
	if out.Code != 200 || out.Playlist == nil || out.Playlist.ID != mustNumber(id) || out.Playlist.ID <= 0 || out.Playlist.ID > music.MaxID || out.Playlist.Name == "" || out.Playlist.TrackCount == nil || *out.Playlist.TrackCount < 0 {
		return music.Playlist{}, music.BadUpstream()
	}
	p := out.Playlist
	if p.TrackIDs == nil && *p.TrackCount > 0 {
		return music.Playlist{}, music.BadUpstream()
	}
	if len(p.TrackIDs) > 10000 && limit == 0 {
		return music.Playlist{}, music.BadUpstream()
	}
	ids := []int64{}
	count := len(p.TrackIDs)
	if limit > 0 && count > limit {
		count = limit
	}
	for _, v := range p.TrackIDs[:count] {
		if v.ID <= 0 || v.ID > music.MaxID {
			return music.Playlist{}, music.BadUpstream()
		}
		ids = append(ids, v.ID)
	}
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	result := music.Playlist{ID: p.ID, Name: p.Name, Cover: p.Cover, Creator: p.Creator.Nickname, TrackCount: *p.TrackCount, Description: p.Description, Tags: tags, PlayCount: p.PlayCount, CreateTime: p.CreateTime, UpdateTime: p.UpdateTime, Tracks: []music.PlaylistTrack{}, ExpectedTrackCount: len(ids)}
	unique := []int64{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	songs := map[int64]rawSong{}
	for start := 0; start < len(unique); start += 100 {
		end := start + 100
		if end > len(unique) {
			end = len(unique)
		}
		batch, e := c.details(ctx, unique[start:end])
		if e != nil {
			if ctx.Err() != nil {
				return result, music.ErrTimeout
			}
			continue
		}
		for id, song := range batch {
			songs[id] = song
		}
	}
	for position, id := range ids {
		song, ok := songs[id]
		if !ok {
			continue
		}
		track, e := song.track()
		if e != nil {
			continue
		}
		result.Tracks = append(result.Tracks, music.PlaylistTrack{Track: track, Duration: song.Duration, SourcePosition: position})
	}
	result.ResolvedTrackCount = len(result.Tracks)
	result.Complete = result.ResolvedTrackCount == result.ExpectedTrackCount && (limit > 0 || len(p.TrackIDs) == *p.TrackCount)
	return result, nil
}
func (c *Client) Album(ctx context.Context, id string) (music.Album, error) {
	var out struct {
		Code  int
		Album *struct {
			ID                       int64
			Name, PicURL, BlurPicURL string
			Artist                   struct{ Name string }
			Artists                  []struct{ Name string }
			PublishTime              int64
			Description, Company     string
			Size                     int
		}
		Songs []rawSong
	}
	e := c.request(ctx, "https://music.163.com/api/v1/album/"+id, nil, &out)
	if e != nil {
		return music.Album{}, e
	}
	if out.Code != 200 || out.Album == nil || out.Album.ID != mustNumber(id) || out.Album.ID <= 0 || out.Album.ID > music.MaxID || out.Album.Name == "" || out.Songs == nil {
		return music.Album{}, music.BadUpstream()
	}
	a := out.Album
	result := music.Album{ID: a.ID, Name: a.Name, Cover: a.PicURL, Artist: a.Artist.Name, PublishTime: a.PublishTime, Description: a.Description, Company: a.Company, Size: a.Size, Songs: []music.AlbumTrack{}}
	if result.Cover == "" {
		result.Cover = a.BlurPicURL
	}
	if result.Artist == "" && len(a.Artists) > 0 {
		result.Artist = a.Artists[0].Name
	}
	for _, song := range out.Songs {
		track, e := song.track()
		if e != nil {
			return result, e
		}
		if track.Album == "" {
			track.Album = a.Name
		}
		if track.PicURL == "" {
			track.PicURL = strings.Replace(a.PicURL, "http://", "https://", 1)
		}
		result.Songs = append(result.Songs, music.AlbumTrack{Track: track, Duration: song.Duration})
	}
	return result, nil
}
func (c *Client) Song(ctx context.Context, id, level string) (music.Song, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	number, _ := strconv.ParseInt(id, 10, 64)
	type detailResult struct {
		songs map[int64]rawSong
		err   error
	}
	type lyricResult struct {
		lyric music.Lyric
		err   error
	}
	type urlResult struct {
		url, level string
		size       int64
		expiry     time.Time
		err        error
	}
	detailsCh := make(chan detailResult, 1)
	lyricsCh := make(chan lyricResult, 1)
	urlCh := make(chan urlResult, 1)
	go func() { songs, e := c.details(ctx, []int64{number}); detailsCh <- detailResult{songs, e} }()
	go func() { lyric, e := c.Lyric(ctx, id); lyricsCh <- lyricResult{lyric, e} }()
	go func() {
		value, quality, size, expiry, e := c.player(ctx, id, level)
		urlCh <- urlResult{value, quality, size, expiry, e}
	}()
	address := <-urlCh
	if address.err != nil {
		return music.Song{}, address.err
	}
	details := <-detailsCh
	if details.err != nil {
		return music.Song{}, details.err
	}
	song, ok := details.songs[number]
	if !ok {
		return music.Song{}, music.BadUpstream()
	}
	track, e := song.track()
	if e != nil {
		return music.Song{}, e
	}
	lyrics := <-lyricsCh
	result := music.Song{ID: track.ID, Name: track.Name, Pic: song.Album.PicURL, Artist: track.Artists, Album: track.Album, Level: levelName(address.level), Size: formatSize(address.size), URL: address.url, ExpiresAt: address.expiry}
	if lyrics.err == nil {
		result.Lyric = lyrics.lyric.Lyric
		result.TLyric = lyrics.lyric.TLyric
	}
	return result, nil
}
