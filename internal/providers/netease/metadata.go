package netease

import (
	"context"
	"cyapi-go/internal/music"
	"strconv"
)

func (c *Client) SampleMetadata(ctx context.Context, id string) (music.Metadata, error) {
	n, e := strconv.ParseInt(id, 10, 64)
	if e != nil || n <= 0 || n > music.MaxID || strconv.FormatInt(n, 10) != id {
		return music.Metadata{}, music.ErrInvalidQuery
	}
	songs, e := c.details(ctx, []int64{n})
	if e != nil {
		return music.Metadata{}, e
	}
	song, ok := songs[n]
	if !ok {
		return music.Metadata{}, music.BadUpstream()
	}
	track, e := song.track()
	if e != nil {
		return music.Metadata{}, e
	}
	return music.Metadata{ID: id, Title: track.Name, Artist: track.Artists, Album: track.Album, DurationMS: song.Duration, LXMetadata: track.LXMetadata}, nil
}
