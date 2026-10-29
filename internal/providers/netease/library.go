package netease

import (
	"context"
	"cyapi-go/internal/library"
	"cyapi-go/internal/music"
	"strconv"
)

func (c *Client) FetchSnapshot(ctx context.Context, source, id string) (library.RemoteSnapshot, error) {
	if source != "netease" {
		return library.RemoteSnapshot{}, music.BadUpstream()
	}
	p, e := c.Playlist(ctx, id, 0)
	if e != nil {
		return library.RemoteSnapshot{}, e
	}
	s := library.RemoteSnapshot{Name: p.Name, ExpectedCount: p.ExpectedTrackCount, Complete: p.Complete, Tracks: make([]library.RemoteTrack, 0, len(p.Tracks))}
	for _, t := range p.Tracks {
		s.Tracks = append(s.Tracks, library.RemoteTrack{Track: library.Track{TrackID: strconv.FormatInt(t.ID, 10), Name: t.Name, Artists: t.Artists, Album: t.Album, PicURL: t.PicURL, Source: source}, SourcePosition: t.SourcePosition})
	}
	return s, nil
}
