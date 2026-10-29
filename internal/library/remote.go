package library

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type RemoteTrack struct {
	Track
	SourcePosition, OccurrenceIndex int
}
type RemoteSnapshot struct {
	Name          string
	ExpectedCount int
	Complete      bool
	Tracks        []RemoteTrack
}
type RemoteProvider interface {
	FetchSnapshot(context.Context, string, string) (RemoteSnapshot, error)
}
type RemoteTicket struct {
	Command                      Command
	StartedAt                    string
	BindingVersion, SyncRevision int64
}
type RemoteStore interface {
	BeginRemote(context.Context, int64, Command) (RemoteTicket, *Result, error)
	FinishRemote(context.Context, int64, RemoteTicket, RemoteSnapshot, *Failure) (Result, error)
}

// NewWithRemote keeps platform I/O outside persistence transactions.
func NewWithRemote(store Store, remote RemoteStore, provider RemoteProvider) *Service {
	return &Service{store: store, remote: remote, provider: provider, remoteSlots: make(chan struct{}, 4)}
}
func validName(name string) bool {
	return name != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 200 && !strings.ContainsRune(name, 0)
}
func (s *Service) Remote(ctx context.Context, user int64, c Command) (Result, error) {
	if !validID(user) {
		return Result{}, Fail(401, "UNAUTHORIZED", "请先登录")
	}
	c.OperationID = strings.TrimSpace(c.OperationID)
	if c.OperationID == "" || len(c.OperationID) > 128 || !utf8.ValidString(c.OperationID) || strings.ContainsRune(c.OperationID, 0) {
		return Result{}, Fail(400, "OPERATION_ID_REQUIRED", "需要不超过128字节的 operationId")
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name != "" && !validName(c.Name) {
		return Result{}, Fail(400, "INVALID_NAME", "歌单名称需为1至200个字符")
	}
	if c.Kind == "copy" {
		if !validID(c.PlaylistID) {
			return Result{}, Fail(400, "INVALID_PLAYLIST_ID", "歌单 ID 无效")
		}
		return s.store.Write(ctx, user, c)
	}
	if c.Kind != "follow" && c.Kind != "sync" {
		return Result{}, Fail(400, "INVALID_REQUEST", "请求无效")
	}
	if c.Kind == "sync" && !validID(c.PlaylistID) {
		return Result{}, Fail(400, "INVALID_PLAYLIST_ID", "歌单 ID 无效")
	}
	if c.Kind == "follow" {
		c.Source = strings.ToLower(strings.TrimSpace(c.Source))
		c.SourcePlaylistID = strings.TrimSpace(c.SourcePlaylistID)
		id, e := ParseID(c.SourcePlaylistID)
		if (c.Source != "netease" && c.Source != "qq") || e != nil {
			return Result{}, Fail(400, "INVALID_SOURCE", "当前仅支持网易云或QQ的数字歌单 ID")
		}
		c.SourcePlaylistID = strconv.FormatInt(id, 10)
	}
	if s.remote == nil || s.provider == nil {
		return Result{}, Fail(503, "SOURCE_UNAVAILABLE", "来源服务尚未配置")
	}
	select {
	case s.remoteSlots <- struct{}{}:
		defer func() { <-s.remoteSlots }()
	default:
		return Result{}, Fail(429, "SOURCE_BUSY", "来源请求繁忙，请稍后重试")
	}
	ticket, replay, e := s.remote.BeginRemote(ctx, user, c)
	if e != nil {
		return Result{}, e
	}
	if replay != nil {
		return *replay, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	snapshot, fetchErr := s.provider.FetchSnapshot(fetchCtx, ticket.Command.Source, ticket.Command.SourcePlaylistID)
	cancel()
	var failure *Failure
	if fetchErr != nil || ctx.Err() != nil || !snapshot.Complete || snapshot.ExpectedCount < 0 || snapshot.ExpectedCount > MaxItems || len(snapshot.Tracks) != snapshot.ExpectedCount {
		failure = Fail(422, "SOURCE_SNAPSHOT_INCOMPLETE", "来源快照不完整或暂时不可用，已保留上次完整内容")
	} else {
		occurrences := map[string]int{}
		for i := range snapshot.Tracks {
			t := &snapshot.Tracks[i]
			normalized, err := normalizeTrack(t.Track)
			if err != nil || normalized.Source != ticket.Command.Source || t.SourcePosition != i {
				failure = Fail(422, "SOURCE_SNAPSHOT_INCOMPLETE", "来源歌曲数据无效，已保留上次完整内容")
				break
			}
			_, err = ParseID(normalized.TrackID)
			if ticket.Command.Source == "qq" && remoteQQID.MatchString(normalized.TrackID) {
				err = nil
			}
			if err != nil {
				failure = Fail(422, "SOURCE_SNAPSHOT_INCOMPLETE", "来源歌曲标识无效")
				break
			}
			t.Track = normalized
			t.OccurrenceIndex = occurrences[t.TrackID]
			occurrences[t.TrackID]++
		}
		if ticket.Command.Kind == "follow" && ticket.Command.Name == "" && !validName(snapshot.Name) {
			failure = Fail(422, "SOURCE_SNAPSHOT_INCOMPLETE", "来源歌单名称无效")
		}
	}
	// Successful snapshot writes share the request's remaining budget. Only failure
	// cleanup detaches from cancellation; a 10000-track publish must not inherit
	// the short failure-cleanup deadline (especially under race instrumentation).
	finishParent, finishBudget := ctx, 20*time.Second
	if failure != nil {
		finishParent, finishBudget = context.WithoutCancel(ctx), 5*time.Second
	}
	finishCtx, finishCancel := context.WithTimeout(finishParent, finishBudget)
	defer finishCancel()
	return s.remote.FinishRemote(finishCtx, user, ticket, snapshot, failure)
}

type RemoteProviders map[string]RemoteProvider

func (p RemoteProviders) FetchSnapshot(ctx context.Context, source, id string) (RemoteSnapshot, error) {
	provider := p[source]
	if provider == nil {
		return RemoteSnapshot{}, Fail(400, "INVALID_SOURCE", "来源不可用")
	}
	return provider.FetchSnapshot(ctx, source, id)
}

var remoteQQID = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
