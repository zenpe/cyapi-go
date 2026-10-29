package userstatestore

import (
	"context"
	"cyapi-go/internal/library"
	"cyapi-go/internal/requestbody"
	"cyapi-go/internal/userstate"
	"database/sql"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db} }

var _ userstate.Store = (*Store)(nil)

type playback struct {
	UserID                                             int64 `gorm:"primaryKey"`
	TrackID, TrackName, Artists, Album, PicURL, Source string
	Position                                           int64
	IsFromPlaylist                                     int
	Platform, UpdatedAt                                string
}

func (playback) TableName() string { return "playback_state" }

type listening struct {
	UserID             int64 `gorm:"primaryKey"`
	TotalListeningTime float64
	UpdatedAt          string
}

func (listening) TableName() string { return "listening_stats" }

type play struct {
	UserID        int64  `json:"-"`
	TrackID       string `json:"track_id"`
	Source        string `json:"source"`
	TrackName     string `json:"track_name"`
	Artists       string `json:"artists"`
	Album         string `json:"album"`
	PicURL        string `json:"pic_url"`
	PlayCount     int64  `json:"play_count"`
	FirstPlayedAt string `json:"-"`
	LastPlayedAt  string `json:"last_played_at"`
}

func (play) TableName() string                 { return "play_counts" }
func text(b map[string]any, key string) string { return requestbody.Text(b[key]) }
func okData(data any) library.Result           { return library.OK(map[string]any{"data": data}) }
func success() library.Result                  { return library.OK(map[string]any{"message": "ok"}) }
func (s *Store) Execute(ctx context.Context, user int64, kind string, b map[string]any) (library.Result, error) {
	db := s.db.WithContext(ctx)
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	switch kind {
	case "save":
		p := playback{UserID: user, TrackID: text(b, "trackId"), TrackName: text(b, "trackName"), Artists: text(b, "artists"), Album: text(b, "album"), PicURL: text(b, "picUrl"), Source: text(b, "source"), Position: b["position"].(int64), IsFromPlaylist: b["isFromPlaylist"].(int), Platform: text(b, "platform"), UpdatedAt: at}
		// The database predicate throttles redundant writes across processes, includes source/metadata, and is cleared by DELETE.
		q := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"track_id", "track_name", "artists", "album", "pic_url", "source", "position", "is_from_playlist", "platform", "updated_at"}), Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "playback_state.updated_at <= ? OR playback_state.track_id <> excluded.track_id OR playback_state.source <> excluded.source OR ABS(playback_state.position - excluded.position) >= 3 OR playback_state.track_name <> excluded.track_name OR playback_state.artists <> excluded.artists OR playback_state.album <> excluded.album OR playback_state.pic_url <> excluded.pic_url OR playback_state.platform <> excluded.platform OR playback_state.is_from_playlist <> excluded.is_from_playlist", Vars: []any{time.Now().Add(-4 * time.Second).UTC().Format("2006-01-02T15:04:05.000000000Z")}}}}}).Create(&p)
		return success(), q.Error
	case "last":
		var p playback
		e := db.Where("user_id = ?", user).Take(&p).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return okData(nil), nil
		}
		if e != nil {
			return library.Result{}, e
		}
		t, e := time.Parse(time.RFC3339Nano, p.UpdatedAt)
		if e != nil {
			return library.Result{}, e
		}
		return okData(map[string]any{"trackId": p.TrackID, "trackName": p.TrackName, "artists": p.Artists, "album": p.Album, "picUrl": p.PicURL, "source": p.Source, "position": p.Position, "isFromPlaylist": p.IsFromPlaylist == 1, "platform": p.Platform, "updatedAt": t.UnixMilli()}), nil
	case "clear":
		return success(), db.Where("user_id = ?", user).Delete(&playback{}).Error
	case "listening-time":
		v := listening{UserID: user, TotalListeningTime: b["seconds"].(float64), UpdatedAt: at}
		e := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"total_listening_time": gorm.Expr("listening_stats.total_listening_time + excluded.total_listening_time"), "updated_at": at})}).Create(&v).Error
		return success(), e
	case "play-count":
		p := play{UserID: user, TrackID: text(b, "trackId"), Source: text(b, "source"), TrackName: text(b, "trackName"), Artists: text(b, "artists"), Album: text(b, "album"), PicURL: text(b, "picUrl"), PlayCount: 1, FirstPlayedAt: at, LastPlayedAt: at}
		e := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "track_id"}, {Name: "source"}}, DoUpdates: clause.Assignments(map[string]any{"play_count": gorm.Expr("play_counts.play_count + 1"), "track_name": p.TrackName, "artists": p.Artists, "album": p.Album, "pic_url": p.PicURL, "last_played_at": at})}).Create(&p).Error
		return success(), e
	case "stats":
		var out library.Result
		opts := &sql.TxOptions{ReadOnly: true}
		if db.Dialector.Name() == "postgres" {
			opts.Isolation = sql.LevelRepeatableRead
		}
		e := db.Transaction(func(tx *gorm.DB) error {
			var total float64
			if e := tx.Model(&listening{}).Select("COALESCE(SUM(total_listening_time),0)").Where("user_id = ?", user).Scan(&total).Error; e != nil {
				return e
			}
			var plays int64
			if e := tx.Model(&play{}).Select("COALESCE(SUM(play_count),0)").Where("user_id = ?", user).Scan(&plays).Error; e != nil {
				return e
			}
			rows := []play{}
			if e := ranking(tx, user).Limit(10).Find(&rows).Error; e != nil {
				return e
			}
			out = okData(map[string]any{"totalListeningTime": total, "totalPlayCount": plays, "playCounts": rows})
			return nil
		}, opts)
		return out, e
	case "records":
		page, limit := b["page"].(int), b["limit"].(int)
		rows := []play{}
		e := ranking(db, user).Limit(limit + 1).Offset((page - 1) * limit).Find(&rows).Error
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		return okData(map[string]any{"records": rows, "page": page, "hasMore": more}), e
	case "song-memory":
		var p play
		e := db.Where("user_id = ? AND track_id = ? AND source = ?", user, text(b, "trackId"), text(b, "source")).Take(&p).Error
		var first, last *string
		if e == nil {
			first = &p.FirstPlayedAt
			last = &p.LastPlayedAt
		} else if errors.Is(e, gorm.ErrRecordNotFound) {
			e = nil
		}
		return library.Result{Status: 200, Body: map[string]any{"code": 200, "data": map[string]any{"firstPlayedAt": first, "lastPlayedAt": last, "playCount": p.PlayCount}}}, e
	case "username":
		e := db.Table("users").Where("id = ?", user).Updates(map[string]any{"username": text(b, "newUsername"), "updated_at": at}).Error
		var sqliteCode interface{ Code() int }
		if errors.Is(e, gorm.ErrDuplicatedKey) || errors.As(e, &sqliteCode) && (sqliteCode.Code() == 2067 || sqliteCode.Code() == 1555) {
			return library.Result{}, library.Fail(400, "USERNAME_TAKEN", "该用户名已被使用")
		}
		return library.Result{Status: 200, Body: map[string]any{"code": 200, "message": "用户名更新成功"}}, e
	case "location":
		e := db.Table("users").Where("id = ?", user).Updates(map[string]any{"last_ip": text(b, "ip"), "last_ip_location": text(b, "location"), "last_ip_updated_at": at, "updated_at": at}).Error
		return library.Result{Status: 200, Body: map[string]any{"code": 200, "message": "IP归属地更新成功", "data": map[string]any{"userId": user, "ip": text(b, "ip"), "location": text(b, "location"), "updatedAt": at}}}, e
	case "bindings": // No binding creation exists while platform QR login is deferred. Never return persisted credentials.
		return library.Result{Status: 200, Body: map[string]any{"code": 200, "data": map[string]any{"netease": map[string]bool{"bound": false}, "kugou": map[string]bool{"bound": false}}}}, nil
	case "unbind-netease", "unbind-kugou":
		e := db.Table("account_bindings").Where("user_id = ? AND provider = ?", user, strings.TrimPrefix(kind, "unbind-")).Delete(map[string]any{}).Error
		return library.Result{Status: 200, Body: map[string]any{"code": 200, "message": "解绑成功"}}, e
	}
	return library.Result{}, library.Fail(400, "INVALID_OPERATION", "操作无效")
}
func ranking(db *gorm.DB, user int64) *gorm.DB {
	return db.Where("user_id = ?", user).Order("play_count DESC,last_played_at DESC,track_id ASC,source ASC")
}
