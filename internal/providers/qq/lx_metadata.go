package qq

import (
	"cyapi-go/internal/music"
	p "cyapi-go/internal/providers/platformhttp"
	"strconv"
)

func lxMetadata(item map[string]any) music.LXMetadata {
	album := p.Obj(item["album"])
	file := p.Obj(item["file"])
	m := music.LXMetadata{AlbumID: p.Text(album["mid"]), AlbumMID: p.Text(album["mid"]), MediaMID: p.Text(file["media_mid"])}
	if m.AlbumID == "" {
		m.AlbumID = p.Text(item["albummid"])
		m.AlbumMID = m.AlbumID
	}
	if m.MediaMID == "" {
		m.MediaMID = p.Text(item["strMediaMid"])
	}
	id := p.Int(item["id"])
	if id == 0 {
		id = p.Int(item["songid"])
	}
	if id > 0 && id <= music.MaxID {
		m.SongID = strconv.FormatInt(id, 10)
	}
	for k, v := range map[string]string{"128k": "size_128mp3", "320k": "size_320mp3", "flac": "size_flac", "flac24bit": "size_hires"} {
		m = m.WithQuality(k, p.Int(file[v]))
	}
	return m.Clean()
}
