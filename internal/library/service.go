// Package library implements the user's favorites and local playlists.
package library

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxBatch = 500
const MaxItems = 10000
const MaxID int64 = 9007199254740991

type Result struct {
	Status int            `json:"status"`
	Body   map[string]any `json:"body"`
}
type Failure struct {
	Status        int
	Code, Message string
}

func (e *Failure) Error() string                     { return e.Code }
func Fail(status int, code, message string) *Failure { return &Failure{status, code, message} }
func OK(body map[string]any) Result                  { body["status"] = 200; return Result{200, body} }
func Rejected(e *Failure) Result {
	return Result{e.Status, map[string]any{"status": e.Status, "code": e.Code, "message": e.Message}}
}

type Track struct {
	TrackID   string            `json:"trackId"`
	Name      string            `json:"name"`
	Artists   string            `json:"artists"`
	Album     string            `json:"album"`
	PicURL    string            `json:"picUrl"`
	Source    string            `json:"source"`
	SourceIDs map[string]string `json:"sourceIds,omitempty"`
}

func (t Track) SourceJSON() *string {
	if len(t.SourceIDs) == 0 {
		return nil
	}
	b, _ := json.Marshal(t.SourceIDs)
	s := string(b)
	return &s
}
func DecodeSource(s *string) map[string]string {
	var m map[string]string
	if s != nil {
		_ = json.Unmarshal([]byte(*s), &m)
	}
	return m
}

type Query struct {
	Kind            string
	PlaylistID      int64
	TrackID, Source string
}
type Command struct {
	Kind, OperationID, Name  string
	Source, SourcePlaylistID string
	PlaylistID, ItemID       int64
	Track                    Track
	Tracks                   []Track
	ItemIDs                  []int64
	ExpectedOrderRevision    int64
}

func (c Command) Scope() string {
	switch c.Kind {
	case "import":
		return "playlist:import:" + c.Source + ":" + c.SourcePlaylistID
	case "follow":
		return "playlist:follow:" + c.Source + ":" + c.SourcePlaylistID
	case "create":
		return "playlist:create"
	case "add":
		return fmt.Sprintf("playlist:%d:add-track", c.PlaylistID)
	case "batch-add":
		return fmt.Sprintf("playlist:%d:add-tracks", c.PlaylistID)
	case "remove":
		return fmt.Sprintf("playlist:%d:remove-item:%d", c.PlaylistID, c.ItemID)
	case "batch-remove":
		return fmt.Sprintf("playlist:%d:remove-items", c.PlaylistID)
	case "replace":
		return fmt.Sprintf("playlist:%d:replace-item:%d", c.PlaylistID, c.ItemID)
	default:
		return fmt.Sprintf("playlist:%d:%s", c.PlaylistID, c.Kind)
	}
}

type Store interface {
	Read(context.Context, int64, Query) (Result, error)
	Write(context.Context, int64, Command) (Result, error)
	AddFavorite(context.Context, int64, Track) (Result, error)
	DeleteFavorite(context.Context, int64, string, string) (Result, error)
}
type Service struct {
	store       Store
	remote      RemoteStore
	provider    RemoteProvider
	remoteSlots chan struct{}
}

func New(s Store) *Service  { return &Service{store: s} }
func validID(id int64) bool { return id > 0 && id <= MaxID }
func normalizeTrack(t Track) (Track, error) {
	t.TrackID = strings.TrimSpace(t.TrackID)
	t.Name = strings.TrimSpace(t.Name)
	t.Source = strings.ToLower(strings.TrimSpace(t.Source))
	if t.TrackID == "" || t.Name == "" || t.Source == "" {
		return t, Fail(400, "INVALID_TRACK", "缺少歌曲标识、名称或来源")
	}
	for _, f := range []struct {
		s   string
		max int
	}{{t.TrackID, 512}, {t.Name, 4096}, {t.Source, 64}, {t.Artists, 8192}, {t.Album, 4096}, {t.PicURL, 8192}} {
		if !utf8.ValidString(f.s) || len(f.s) > f.max || strings.ContainsRune(f.s, 0) {
			return t, Fail(400, "INVALID_TRACK", "歌曲字段无效或过长")
		}
	}
	ids := map[string]string{}
	for _, key := range []string{"fileHash", "emixSongId", "albumAudioId"} {
		v := strings.TrimSpace(t.SourceIDs[key])
		if len(v) > 512 || !utf8.ValidString(v) {
			return t, Fail(400, "INVALID_TRACK", "来源标识无效")
		}
		if v != "" {
			ids[key] = v
		}
	}
	t.SourceIDs = ids
	return t, nil
}
func (s *Service) Read(ctx context.Context, user int64, q Query) (Result, error) {
	if !validID(user) {
		return Result{}, Fail(401, "UNAUTHORIZED", "请先登录")
	}
	switch q.Kind {
	case "favorites", "playlists":
	case "tracks":
		if !validID(q.PlaylistID) {
			return Result{}, Fail(400, "INVALID_PLAYLIST_ID", "歌单 ID 无效")
		}
	case "check":
		q.TrackID = strings.TrimSpace(q.TrackID)
		q.Source = strings.ToLower(strings.TrimSpace(q.Source))
		if q.TrackID == "" || q.Source == "" || len(q.TrackID) > 512 || len(q.Source) > 64 || strings.ContainsRune(q.TrackID, 0) || strings.ContainsRune(q.Source, 0) || !utf8.ValidString(q.TrackID) || !utf8.ValidString(q.Source) {
			return Result{}, Fail(400, "INVALID_TRACK", "缺少歌曲标识")
		}
	default:
		return Result{}, Fail(400, "INVALID_REQUEST", "请求无效")
	}
	return s.store.Read(ctx, user, q)
}
func (s *Service) Favorite(ctx context.Context, user int64, t Track) (Result, error) {
	if !validID(user) {
		return Result{}, Fail(401, "UNAUTHORIZED", "请先登录")
	}
	v, e := normalizeTrack(t)
	if e != nil {
		return Result{}, e
	}
	return s.store.AddFavorite(ctx, user, v)
}
func (s *Service) Unfavorite(ctx context.Context, user int64, track, source string) (Result, error) {
	if !validID(user) {
		return Result{}, Fail(401, "UNAUTHORIZED", "请先登录")
	}
	if track == "" || source == "" || len(track) > 512 || len(source) > 64 || strings.ContainsRune(track, 0) || strings.ContainsRune(source, 0) || !utf8.ValidString(track) || !utf8.ValidString(source) {
		return Result{}, Fail(400, "INVALID_TRACK", "歌曲标识无效")
	}
	return s.store.DeleteFavorite(ctx, user, track, strings.ToLower(source))
}
func (s *Service) Write(ctx context.Context, user int64, c Command) (Result, error) {
	if !validID(user) {
		return Result{}, Fail(401, "UNAUTHORIZED", "请先登录")
	}
	if c.Kind != "create" && !validID(c.PlaylistID) {
		return Result{}, Fail(400, "INVALID_PLAYLIST_ID", "歌单 ID 无效")
	}
	if c.Kind != "rename" && c.Kind != "delete" {
		c.OperationID = strings.TrimSpace(c.OperationID)
		if c.OperationID == "" || len(c.OperationID) > 128 || strings.ContainsRune(c.OperationID, 0) {
			return Result{}, Fail(400, "OPERATION_ID_REQUIRED", "需要不超过128字节的 operationId")
		}
	}
	switch c.Kind {
	case "create", "rename":
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" || utf8.RuneCountInString(c.Name) > 200 || !utf8.ValidString(c.Name) || strings.ContainsRune(c.Name, 0) {
			return Result{}, Fail(400, "INVALID_NAME", "歌单名称需为1至200个字符")
		}
	case "delete":
	case "add", "replace":
		var e error
		c.Track, e = normalizeTrack(c.Track)
		if e != nil {
			return Result{}, e
		}
		if c.Kind == "replace" && !validID(c.ItemID) {
			return Result{}, Fail(400, "INVALID_ITEM_ID", "条目 ID 无效")
		}
	case "batch-add":
		if len(c.Tracks) == 0 || len(c.Tracks) > MaxBatch {
			return Result{}, Fail(400, "INVALID_TRACKS", "每批需包含1至500首歌曲")
		}
		unique := map[string]int{}
		out := []Track{}
		for _, t := range c.Tracks {
			v, e := normalizeTrack(t)
			if e != nil {
				return Result{}, e
			}
			key := v.Source + "\x00" + v.TrackID
			if i, ok := unique[key]; ok {
				out[i] = v
			} else {
				unique[key] = len(out)
				out = append(out, v)
			}
		}
		c.Tracks = out
	case "remove":
		if !validID(c.ItemID) {
			return Result{}, Fail(400, "INVALID_ITEM_ID", "条目 ID 无效")
		}
	case "batch-remove", "reorder":
		limit := MaxBatch
		if c.Kind == "reorder" {
			limit = MaxItems
		}
		if len(c.ItemIDs) > limit || (c.Kind == "batch-remove" && len(c.ItemIDs) == 0) {
			return Result{}, Fail(400, "INVALID_ITEMS", "条目数量无效")
		}
		seen := map[int64]bool{}
		ids := []int64{}
		for _, id := range c.ItemIDs {
			if !validID(id) {
				return Result{}, Fail(400, "INVALID_ITEM_ID", "条目 ID 无效")
			}
			if seen[id] {
				if c.Kind == "reorder" {
					return Result{}, Fail(400, "INVALID_ORDER", "排序条目不能重复")
				}
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		c.ItemIDs = ids
		if c.Kind == "reorder" && (c.ExpectedOrderRevision < 0 || c.ExpectedOrderRevision > MaxID) {
			return Result{}, Fail(400, "INVALID_ORDER", "排序版本无效")
		}
	default:
		return Result{}, Fail(400, "INVALID_REQUEST", "请求无效")
	}
	return s.store.Write(ctx, user, c)
}

// ParseID accepts canonical positive integers safe for existing JavaScript clients.
func ParseID(v string) (int64, error) {
	id, e := strconv.ParseInt(v, 10, 64)
	if e != nil || !validID(id) || strconv.FormatInt(id, 10) != v {
		return 0, Fail(400, "INVALID_ID", "ID 无效")
	}
	return id, nil
}
