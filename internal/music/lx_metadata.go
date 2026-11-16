package music

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type LXQuality struct {
	Size  string `json:"size,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}
type LXMetadata struct {
	SongID    string               `json:"songId,omitempty"`
	MediaMID  string               `json:"strMediaMid,omitempty"`
	AlbumID   string               `json:"albumId,omitempty"`
	AlbumMID  string               `json:"albumMid,omitempty"`
	Image     string               `json:"img,omitempty"`
	Qualities map[string]LXQuality `json:"qualities,omitempty"`
}

var LXQualityOrder = []string{"128k", "192k", "320k", "flac", "flac24bit"}

func (m LXMetadata) Empty() bool {
	return m.SongID == "" && m.MediaMID == "" && m.AlbumID == "" && m.AlbumMID == "" && m.Image == "" && len(m.Qualities) == 0
}
func ParseLXMetadata(v any) LXMetadata {
	b, e := json.Marshal(v)
	if e != nil || len(b) > 16384 {
		return LXMetadata{}
	}
	var m LXMetadata
	if json.Unmarshal(b, &m) != nil {
		return LXMetadata{}
	}
	return m.Clean()
}
func (m LXMetadata) Clean() LXMetadata {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 512 || strings.ContainsRune(s, 0) {
			return ""
		}
		return s
	}
	m.SongID = clean(m.SongID)
	m.MediaMID = clean(m.MediaMID)
	m.AlbumID = clean(m.AlbumID)
	m.AlbumMID = clean(m.AlbumMID)
	if len(m.Image) > 8192 || strings.ContainsRune(m.Image, 0) {
		m.Image = ""
	}
	q := map[string]LXQuality{}
	for _, k := range LXQualityOrder {
		if v, ok := m.Qualities[k]; ok {
			if len(v.Size) > 64 {
				v.Size = ""
			}
			if v.Bytes < 0 || v.Bytes > MaxID {
				v.Bytes = 0
			}
			q[k] = v
		}
	}
	m.Qualities = q
	return m
}
func (m LXMetadata) WithQuality(kind string, bytes int64) LXMetadata {
	if bytes <= 0 || bytes > MaxID {
		return m
	}
	if m.Qualities == nil {
		m.Qualities = map[string]LXQuality{}
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	unit := 0
	number := float64(bytes)
	for number >= 1024 && unit < len(units)-1 {
		number /= 1024
		unit++
	}
	size := fmt.Sprintf("%.2f %s", number, units[unit])
	m.Qualities[kind] = LXQuality{Size: size, Bytes: bytes}
	return m
}
func (m LXMetadata) ScriptFields(source string) map[string]any {
	m = m.Clean()
	types := []any{}
	details := map[string]any{}
	for _, k := range LXQualityOrder {
		if v, ok := m.Qualities[k]; ok {
			b, _ := json.Marshal(v)
			var info map[string]any
			_ = json.Unmarshal(b, &info)
			delete(info, "bytes")
			details[k] = info
			item := map[string]any{"type": k}
			for key, value := range info {
				if key == "size" {
					item[key] = value
				}
			}
			types = append(types, item)
		}
	}
	out := map[string]any{"img": m.Image, "types": types, "_types": details, "typeUrl": map[string]any{}}
	if m.AlbumID != "" {
		var id any = m.AlbumID
		if source == "wy" {
			if n, e := strconv.ParseInt(m.AlbumID, 10, 64); e == nil {
				id = n
			}
		}
		out["albumId"] = id
	}
	if source == "tx" {
		if n, e := strconv.ParseInt(m.SongID, 10, 64); e == nil && n > 0 && n <= MaxID {
			out["songId"] = n
		}
		if m.MediaMID != "" {
			out["strMediaMid"] = m.MediaMID
		}
		if m.AlbumMID != "" {
			out["albumMid"] = m.AlbumMID
		}
	}
	return out
}
