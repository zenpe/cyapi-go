package netease

import (
	"cyapi-go/internal/music"
	"strconv"
)

func (s rawSong) lxMetadata() music.LXMetadata {
	m := music.LXMetadata{Image: s.Album.PicURL}
	if s.Album.ID > 0 {
		m.AlbumID = strconv.FormatInt(s.Album.ID, 10)
	}
	if s.L != nil {
		m = m.WithQuality("128k", s.L.Size)
	}
	if s.H != nil {
		m = m.WithQuality("320k", s.H.Size)
	}
	if s.SQ != nil {
		m = m.WithQuality("flac", s.SQ.Size)
	}
	if s.HR != nil {
		m = m.WithQuality("flac24bit", s.HR.Size)
	}
	return m.Clean()
}
