package sourcecheck

import (
	"context"
	"cyapi-go/internal/music"
	"strconv"
	"time"
)

type Metadata struct {
	LXMetadata                    music.LXMetadata
	TrackID, Title, Artist, Album string
	DurationMS                    int64
}
type MetadataLoader func(context.Context, string, string) (Metadata, error)

func SampleFromMetadata(platform string, m Metadata) (Sample, error) {
	if platform != "qq" && platform != "netease" || !idPattern.MatchString(m.TrackID) || m.Title == "" || m.Artist == "" {
		return Sample{}, ErrInvalid
	}
	source := "tx"
	if platform == "netease" {
		source = "wy"
	}
	info := map[string]any{"source": source, "songmid": m.TrackID, "name": m.Title, "songName": m.Title, "singer": m.Artist, "artist": m.Artist, "albumName": m.Album}
	for k, v := range m.LXMetadata.ScriptFields(source) {
		info[k] = v
	}
	if platform == "netease" {
		n, e := strconv.ParseInt(m.TrackID, 10, 64)
		if e != nil || n <= 0 {
			return Sample{}, ErrInvalid
		}
		info["id"] = n
		info["songmid"] = n
	}
	s := Sample{ID: platform + "-" + m.TrackID, Platform: platform, TrackID: m.TrackID, Enabled: true, Title: m.Title, MusicInfo: info}
	if m.DurationMS > 0 {
		s.ExpectedDurationMS = &m.DurationMS
		s.DurationEvidence = time.Now().UTC().Format(time.RFC3339) + " official song-detail metadata"
	}
	if e := s.validate(); e != nil {
		return Sample{}, ErrInvalid
	}
	return s, nil
}

// lxMusicInfo does not mutate persisted samples. ExpectedDurationMS is the
// single duration source; user-supplied duration aliases cannot override it.
func (s Sample) lxMusicInfo() map[string]any {
	info := make(map[string]any, len(s.MusicInfo)+1)
	for k, v := range s.MusicInfo {
		if k != "interval" && k != "duration" && k != "DURATION" {
			info[k] = v
		}
	}
	if s.ExpectedDurationMS != nil {
		if interval := music.LXInterval(*s.ExpectedDurationMS); interval != "" {
			info["interval"] = interval
		}
	}
	return info
}
