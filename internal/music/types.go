// Package music keeps client contracts independent from provider protocols.
package music

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxID int64 = 9007199254740991

var ErrTimeout = errors.New("music upstream timeout")
var ErrUnavailable = errors.New("song unavailable for current provider credentials")

type UpstreamError struct {
	Status     int
	Retryable  bool
	RetryAfter string
}

func (e *UpstreamError) Error() string { return "music upstream unavailable" }
func BadUpstream() error               { return &UpstreamError{Status: 502} }

type Track struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Artists string `json:"artists"`
	Album   string `json:"album"`
	PicURL  string `json:"picUrl"`
}
type PlaylistTrack struct {
	Track
	Duration       int64 `json:"duration"`
	SourcePosition int   `json:"sourcePosition"`
}
type SearchPage struct {
	Songs           []Track `json:"result"`
	Page            int     `json:"page"`
	ResultKind      string  `json:"resultKind"`
	HasMore         bool    `json:"hasMore"`
	PageFingerprint *string `json:"pageFingerprint"`
}
type Lyric struct {
	State     string `json:"state"`
	Retryable bool   `json:"retryable"`
	Lyric     string `json:"lyric"`
	TLyric    string `json:"tlyric"`
}
type Song struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Pic       string    `json:"pic"`
	Artist    string    `json:"ar_name"`
	Album     string    `json:"al_name"`
	Level     string    `json:"level"`
	Size      string    `json:"size"`
	URL       string    `json:"url"`
	Lyric     string    `json:"lyric"`
	TLyric    string    `json:"tlyric"`
	ExpiresAt time.Time `json:"-"`
}
type Playlist struct {
	ID                 int64           `json:"id"`
	Name               string          `json:"name"`
	Cover              string          `json:"coverImgUrl"`
	Creator            string          `json:"creator"`
	TrackCount         int             `json:"trackCount"`
	Description        string          `json:"description"`
	Tags               []string        `json:"tags"`
	PlayCount          int64           `json:"playCount"`
	CreateTime         int64           `json:"createTime"`
	UpdateTime         int64           `json:"updateTime"`
	Tracks             []PlaylistTrack `json:"tracks"`
	ExpectedTrackCount int             `json:"expectedTrackCount"`
	ResolvedTrackCount int             `json:"resolvedTrackCount"`
	Complete           bool            `json:"complete"`
}
type AlbumTrack struct {
	Track
	Duration int64 `json:"duration"`
}
type Album struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Cover       string       `json:"coverImgUrl"`
	Artist      string       `json:"artist"`
	PublishTime int64        `json:"publishTime"`
	Description string       `json:"description"`
	Company     string       `json:"company"`
	Size        int          `json:"size"`
	Songs       []AlbumTrack `json:"songs"`
}
type Provider interface {
	Search(context.Context, string, int, int) (SearchPage, error)
	Song(context.Context, string, string) (Song, error)
	Lyric(context.Context, string) (Lyric, error)
	Playlist(context.Context, string, int) (Playlist, error)
	Album(context.Context, string) (Album, error)
}

func ParseID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") {
		u, e := url.Parse(value)
		if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host != "music.163.com" || u.User != nil {
			return "", errors.New("invalid music ID")
		}
		q := u.Query()
		if q.Get("id") == "" && u.Fragment != "" {
			fragment, e := url.Parse(u.Fragment)
			if e != nil {
				return "", errors.New("invalid music ID")
			}
			q = fragment.Query()
		}
		if len(q["id"]) != 1 {
			return "", errors.New("invalid music ID")
		}
		value = q.Get("id")
	}
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", errors.New("invalid music ID")
	}
	id, e := strconv.ParseInt(value, 10, 64)
	if e != nil || id <= 0 || id > MaxID {
		return "", errors.New("invalid music ID")
	}
	return strconv.FormatInt(id, 10), nil
}
func ValidLevel(level string) bool {
	switch level {
	case "standard", "exhigh", "lossless", "hires", "sky", "jyeffect", "jymaster":
		return true
	}
	return false
}
