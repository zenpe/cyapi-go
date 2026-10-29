package librarystore

import (
	"context"
	"cyapi-go/internal/library"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"sync/atomic"
	"time"
)

const stamp = "2006-01-02T15:04:05.000000000Z"

type Store struct {
	db        *gorm.DB
	now       func() time.Time
	nextPrune atomic.Int64
}

func New(db *gorm.DB) *Store { return &Store{db: db, now: time.Now} }

var _ library.Store = (*Store)(nil)

func missing() *library.Failure {
	return library.Fail(404, "PLAYLIST_NOT_FOUND", "歌单不存在或不可修改")
}
func owned(db *gorm.DB, user, id int64) (playlist, error) {
	var p playlist
	e := db.Where("id = ? AND user_id = ?", id, user).Take(&p).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return p, missing()
	}
	return p, e
}
func lock(db *gorm.DB, user, id int64) (playlist, error) {
	// A scoped no-op UPDATE obtains a row lock on PostgreSQL and a write lock on SQLite.
	// SQLite transactions must acquire their write lock before reading state.
	q := db.Model(&playlist{}).Where("id = ? AND user_id = ?", id, user).UpdateColumn("updated_at", gorm.Expr("updated_at"))
	if q.Error != nil {
		return playlist{}, q.Error
	}
	if q.RowsAffected == 0 {
		return playlist{}, missing()
	}
	return owned(db, user, id)
}

const countSelect = "playlists.*, CASE WHEN playlists.type = 'local' THEN (SELECT COUNT(*) FROM local_playlist_items li WHERE li.playlist_id = playlists.id) ELSE (SELECT COUNT(*) FROM playlist_snapshot_items si WHERE si.snapshot_id = playlists.active_snapshot_id) END AS track_count"

func withCount(db *gorm.DB, user, id int64) (playlist, error) {
	var p playlist
	e := db.Model(&playlist{}).Select(countSelect).Where("playlists.id = ? AND playlists.user_id = ?", id, user).Take(&p).Error
	return p, e
}
func (s *Store) Read(ctx context.Context, user int64, q library.Query) (library.Result, error) {
	db := s.db.WithContext(ctx)
	switch q.Kind {
	case "favorites":
		var rows []favorite
		if e := db.Where("user_id = ?", user).Order("added_at DESC, id DESC").Find(&rows).Error; e != nil {
			return library.Result{}, e
		}
		out := []map[string]any{}
		for _, r := range rows {
			out = append(out, r.public())
		}
		return library.OK(map[string]any{"favorites": out}), nil
	case "playlists":
		p := playlist{UserID: user, Name: "我的收藏", Type: "local", IsDefault: 1, SyncStatus: "idle", CreatedAt: s.now().UTC().Format(stamp), UpdatedAt: s.now().UTC().Format(stamp)}
		if e := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error; e != nil {
			return library.Result{}, e
		}
		var rows []playlist
		if e := db.Model(&playlist{}).Select(countSelect).Where("user_id = ?", user).Order("is_default DESC, created_at ASC, id ASC").Find(&rows).Error; e != nil {
			return library.Result{}, e
		}
		out := []map[string]any{}
		for _, r := range rows {
			out = append(out, r.public())
		}
		return library.OK(map[string]any{"playlists": out}), nil
	case "check":
		var rows []struct {
			PlaylistID   int64  `json:"playlistId"`
			PlaylistName string `json:"playlistName"`
			ItemID       int64  `json:"itemId"`
		}
		e := db.Table("playlists p").Select("p.id AS playlist_id, p.name AS playlist_name, li.id AS item_id").Joins("JOIN local_playlist_items li ON li.playlist_id = p.id").Joins("JOIN playlist_tracks_catalog t ON t.id = li.track_id").Where("p.user_id = ? AND p.type = 'local' AND t.source = ? AND t.source_track_id = ?", user, q.Source, q.TrackID).Order("p.id").Scan(&rows).Error
		if rows == nil {
			rows = make([]struct {
				PlaylistID   int64  `json:"playlistId"`
				PlaylistName string `json:"playlistName"`
				ItemID       int64  `json:"itemId"`
			}, 0)
		}
		return library.OK(map[string]any{"inPlaylist": len(rows) > 0, "matches": rows}), e
	case "tracks":
		var out library.Result
		opts := &sql.TxOptions{ReadOnly: true}
		if db.Dialector.Name() == "postgres" {
			opts.Isolation = sql.LevelRepeatableRead
		}
		e := db.Transaction(func(tx *gorm.DB) error {
			p, e := owned(tx, user, q.PlaylistID)
			if e != nil {
				return e
			}
			type row struct {
				Track   catalog `gorm:"embedded"`
				ItemID  *int64
				AddedAt string
			}
			var rows []row
			if p.Type == "local" {
				e = tx.Table("local_playlist_items li").Select("t.*, li.id AS item_id, li.added_at").Joins("JOIN playlist_tracks_catalog t ON t.id = li.track_id").Where("li.playlist_id = ?", p.ID).Order("li.rank ASC, li.id ASC").Scan(&rows).Error
			} else {
				e = tx.Table("playlist_snapshot_items si").Select("t.*, NULL AS item_id, s.completed_at AS added_at").Joins("JOIN playlist_snapshots s ON s.id = si.snapshot_id").Joins("JOIN playlist_tracks_catalog t ON t.id = si.track_id").Where("si.snapshot_id = ?", p.ActiveSnapshotID).Order("si.source_position ASC, si.occurrence_index ASC").Scan(&rows).Error
			}
			if e != nil {
				return e
			}
			tracks := []map[string]any{}
			for _, r := range rows {
				t := r.Track.track()
				v := map[string]any{"itemId": r.ItemID, "trackId": t.TrackID, "name": t.Name, "artists": t.Artists, "album": t.Album, "picUrl": t.PicURL, "source": t.Source, "addedAt": r.AddedAt}
				if len(t.SourceIDs) > 0 {
					v["sourceIds"] = t.SourceIDs
				}
				tracks = append(tracks, v)
			}
			out = library.OK(map[string]any{"type": p.Type, "contentRevision": p.ContentRevision, "orderRevision": p.OrderRevision, "tracks": tracks})
			return nil
		}, opts)
		return out, e
	}
	return library.Result{}, library.Fail(400, "INVALID_REQUEST", "请求无效")
}
func (s *Store) AddFavorite(ctx context.Context, user int64, t library.Track) (library.Result, error) {
	f := favorite{UserID: user, TrackID: t.TrackID, Name: t.Name, Artists: t.Artists, Album: t.Album, PicURL: t.PicURL, Source: t.Source, SourceIDsJSON: t.SourceJSON(), AddedAt: s.now().UTC().Format(stamp)}
	q := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&f)
	if q.Error != nil {
		return library.Result{}, q.Error
	}
	if q.RowsAffected == 0 {
		return library.Result{}, library.Fail(400, "ALREADY_FAVORITED", "已经收藏过该歌曲")
	}
	return library.OK(map[string]any{"message": "收藏成功"}), nil
}
func (s *Store) DeleteFavorite(ctx context.Context, user int64, track, source string) (library.Result, error) {
	q := s.db.WithContext(ctx).Where("user_id = ? AND track_id = ? AND source = ?", user, track, source).Delete(&favorite{})
	if q.Error != nil {
		return library.Result{}, q.Error
	}
	if q.RowsAffected == 0 {
		return library.Result{}, library.Fail(404, "FAVORITE_NOT_FOUND", "未找到该收藏")
	}
	return library.OK(map[string]any{"message": "取消收藏成功"}), nil
}
func (s *Store) Write(ctx context.Context, user int64, c library.Command) (library.Result, error) {
	now := s.now().UTC()
	at := now.Format(stamp)
	var out library.Result
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if c.Kind == "rename" || c.Kind == "delete" {
			var e error
			out, e = s.mutate(tx, user, c, at)
			return e
		}
		// Operation result and every business write commit atomically. No visible pending state.
		if e := tx.Where("user_id = ? AND operation_id = ? AND expires_at <= ?", user, c.OperationID, at).Delete(&operation{}).Error; e != nil {
			return e
		}
		op := operation{UserID: user, OperationID: c.OperationID, Scope: c.Scope(), Status: "pending", CreatedAt: at, ExpiresAt: now.Add(24 * time.Hour).Format(stamp)}
		q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&op)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			var saved operation
			if e := tx.Where("user_id = ? AND operation_id = ?", user, c.OperationID).Take(&saved).Error; e != nil {
				return e
			}
			if saved.Scope != op.Scope {
				return library.Fail(409, "OPERATION_SCOPE_MISMATCH", "operationId 已用于其他操作")
			}
			if saved.ResponseJSON == nil {
				return library.Fail(409, "OPERATION_PENDING", "操作仍在处理中")
			}
			return json.Unmarshal([]byte(*saved.ResponseJSON), &out)
		}
		// Roll back partial work on business failure while retaining a replayable error result.
		workErr := tx.Transaction(func(work *gorm.DB) error { var e error; out, e = s.mutate(work, user, c, at); return e })
		status := "succeeded"
		if workErr != nil {
			var f *library.Failure
			if !errors.As(workErr, &f) {
				return workErr
			}
			out = library.Rejected(f)
			status = "failed"
		}
		result := encode(out)
		if e := tx.Model(&operation{}).Where("user_id = ? AND operation_id = ?", user, c.OperationID).Updates(map[string]any{"status": status, "response_json": result}).Error; e != nil {
			return e
		}
		next := s.nextPrune.Load()
		if now.Unix() >= next && s.nextPrune.CompareAndSwap(next, now.Unix()+60) {
			expired := tx.Model(&operation{}).Select("user_id, operation_id").Where("expires_at <= ?", at).Order("expires_at").Limit(100)
			if e := tx.Where("(user_id, operation_id) IN (?)", expired).Delete(&operation{}).Error; e != nil {
				return e
			}
		}
		return nil
	})
	return out, e
}
func resultPlaylist(tx *gorm.DB, user, id int64, fields map[string]any) (library.Result, error) {
	p, e := withCount(tx, user, id)
	if e != nil {
		return library.Result{}, e
	}
	fields["playlist"] = p.public()
	return library.OK(fields), nil
}
func bump(tx *gorm.DB, id int64, content, order bool, at string) error {
	values := map[string]any{"updated_at": at}
	if content {
		values["content_revision"] = gorm.Expr("content_revision + 1")
	}
	if order {
		values["order_revision"] = gorm.Expr("order_revision + 1")
	}
	return tx.Model(&playlist{}).Where("id = ?", id).Updates(values).Error
}
func upsert(tx *gorm.DB, t library.Track, at string) (int64, error) {
	c := catalog{Source: t.Source, SourceTrackID: t.TrackID, Name: t.Name, Artists: t.Artists, Album: t.Album, PicURL: t.PicURL, SourceIDsJSON: t.SourceJSON(), CreatedAt: at, UpdatedAt: at}
	e := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "source"}, {Name: "source_track_id"}}, DoUpdates: clause.Assignments(map[string]any{"name": t.Name, "artists": t.Artists, "album": t.Album, "pic_url": t.PicURL, "source_ids_json": gorm.Expr("COALESCE(excluded.source_ids_json, playlist_tracks_catalog.source_ids_json)"), "updated_at": at})}, clause.Returning{Columns: []clause.Column{{Name: "id"}}}).Create(&c).Error
	return c.ID, e
}
func (s *Store) mutate(tx *gorm.DB, user int64, c library.Command, at string) (library.Result, error) {
	if c.Kind == "create" {
		p := playlist{UserID: user, Name: c.Name, Type: "local", SyncStatus: "idle", CreatedAt: at, UpdatedAt: at}
		if e := tx.Create(&p).Error; e != nil {
			return library.Result{}, e
		}
		return resultPlaylist(tx, user, p.ID, map[string]any{})
	}
	p, e := lock(tx, user, c.PlaylistID)
	if e != nil {
		return library.Result{}, e
	}
	switch c.Kind {
	case "rename":
		if p.IsDefault == 1 {
			return library.Result{}, missing()
		}
		if e = tx.Model(&playlist{}).Where("id = ?", p.ID).Updates(map[string]any{"name": c.Name, "updated_at": at}).Error; e != nil {
			return library.Result{}, e
		}
		return resultPlaylist(tx, user, p.ID, map[string]any{"message": "更新成功"})
	case "delete":
		if p.IsDefault == 1 {
			return library.Result{}, missing()
		}
		if e = tx.Delete(&playlist{}, p.ID).Error; e != nil {
			return library.Result{}, e
		}
		return library.OK(map[string]any{"message": "删除成功"}), nil
	}
	if p.Type != "local" {
		return library.Result{}, library.Fail(409, "SYNCED_PLAYLIST_READ_ONLY", "同步歌单为只读")
	}
	switch c.Kind {
	case "add", "batch-add":
		tracks := c.Tracks
		if c.Kind == "add" {
			tracks = []library.Track{c.Track}
		}
		var count int64
		if e = tx.Model(&item{}).Where("playlist_id = ?", p.ID).Count(&count).Error; e != nil {
			return library.Result{}, e
		}
		// Catalog locks are always acquired in canonical order across playlists.
		ordered := append([]library.Track(nil), tracks...)
		sort.Slice(ordered, func(i, j int) bool {
			return ordered[i].Source+"\x00"+ordered[i].TrackID < ordered[j].Source+"\x00"+ordered[j].TrackID
		})
		ids := map[string]int64{}
		for _, t := range ordered {
			id, e := upsert(tx, t, at)
			if e != nil {
				return library.Result{}, e
			}
			ids[t.Source+"\x00"+t.TrackID] = id
		}
		var min int64
		if e = tx.Model(&item{}).Select("COALESCE(MIN(rank), 0)").Where("playlist_id = ?", p.ID).Scan(&min).Error; e != nil {
			return library.Result{}, e
		}
		if min < -library.MaxID+int64(len(tracks))*1024 {
			return library.Result{}, library.Fail(409, "RANK_LIMIT", "请先重新排序歌单")
		}
		added := int64(0)
		var itemID int64
		for index, t := range tracks {
			row := item{PlaylistID: p.ID, TrackID: ids[t.Source+"\x00"+t.TrackID], Rank: min - int64(len(tracks)-index)*1024, AddedAt: at}
			q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
			if q.Error != nil {
				return library.Result{}, q.Error
			}
			if q.RowsAffected == 1 {
				added++
				itemID = row.ID
			} else {
				if e = tx.Where("playlist_id = ? AND track_id = ?", p.ID, row.TrackID).Take(&row).Error; e != nil {
					return library.Result{}, e
				}
				itemID = row.ID
			}
		}
		if count+added > library.MaxItems {
			return library.Result{}, library.Fail(409, "PLAYLIST_LIMIT", "单个歌单最多10000首歌曲")
		}
		if added > 0 {
			if e = bump(tx, p.ID, true, true, at); e != nil {
				return library.Result{}, e
			}
		}
		fields := map[string]any{"addedCount": added}
		if c.Kind == "add" {
			fields = map[string]any{"added": added == 1, "itemId": itemID}
		}
		return resultPlaylist(tx, user, p.ID, fields)
	case "remove", "batch-remove":
		ids := c.ItemIDs
		if c.Kind == "remove" {
			ids = []int64{c.ItemID}
		}
		q := tx.Where("playlist_id = ? AND id IN ?", p.ID, ids).Delete(&item{})
		if q.Error != nil {
			return library.Result{}, q.Error
		}
		if q.RowsAffected == 0 && c.Kind == "remove" {
			return library.Result{}, library.Fail(404, "ITEM_NOT_FOUND", "歌单条目不存在")
		}
		if q.RowsAffected > 0 {
			if e = bump(tx, p.ID, true, true, at); e != nil {
				return library.Result{}, e
			}
		}
		fields := map[string]any{}
		if c.Kind == "batch-remove" {
			fields["removedCount"] = q.RowsAffected
		}
		return resultPlaylist(tx, user, p.ID, fields)
	case "replace":
		var row item
		e = tx.Where("id = ? AND playlist_id = ?", c.ItemID, p.ID).Take(&row).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return library.Result{}, library.Fail(404, "ITEM_NOT_FOUND", "歌单条目不存在")
		}
		if e != nil {
			return library.Result{}, e
		}
		id, e := upsert(tx, c.Track, at)
		if e != nil {
			return library.Result{}, e
		}
		if id != row.TrackID {
			var count int64
			if e = tx.Model(&item{}).Where("playlist_id = ? AND track_id = ? AND id <> ?", p.ID, id, row.ID).Count(&count).Error; e != nil {
				return library.Result{}, e
			}
			if count > 0 {
				return library.Result{}, library.Fail(409, "DUPLICATE_TRACK", "目标歌曲已在歌单中")
			}
			if e = tx.Model(&item{}).Where("id = ?", row.ID).Update("track_id", id).Error; e != nil {
				return library.Result{}, e
			}
			if e = bump(tx, p.ID, true, false, at); e != nil {
				return library.Result{}, e
			}
		}
		return resultPlaylist(tx, user, p.ID, map[string]any{"itemId": row.ID})
	case "reorder":
		if p.OrderRevision != c.ExpectedOrderRevision {
			return library.Result{}, library.Fail(409, "REVISION_CONFLICT", "歌单顺序已更新，请刷新后重试")
		}
		var ids []int64
		if e = tx.Model(&item{}).Where("playlist_id = ?", p.ID).Pluck("id", &ids).Error; e != nil {
			return library.Result{}, e
		}
		set := map[int64]bool{}
		for _, id := range ids {
			set[id] = true
		}
		if len(ids) != len(c.ItemIDs) {
			return library.Result{}, library.Fail(409, "ORDER_SET_MISMATCH", "歌单内容已更新")
		}
		for _, id := range c.ItemIDs {
			if !set[id] {
				return library.Result{}, library.Fail(409, "ORDER_SET_MISMATCH", "歌单内容已更新")
			}
		}
		// Bounded CASE batches avoid SQLite parameter limits and keep one atomic transaction.
		for start := 0; start < len(c.ItemIDs); start += 200 {
			end := start + 200
			if end > len(c.ItemIDs) {
				end = len(c.ItemIDs)
			}
			expr := "CASE id"
			args := []any{}
			for i := start; i < end; i++ {
				expr += " WHEN ? THEN ?"
				args = append(args, c.ItemIDs[i], int64(i)*1024)
			}
			expr += " ELSE rank END"
			if e = tx.Model(&item{}).Where("playlist_id = ? AND id IN ?", p.ID, c.ItemIDs[start:end]).Update("rank", gorm.Expr(expr, args...)).Error; e != nil {
				return library.Result{}, e
			}
		}
		if e = bump(tx, p.ID, false, true, at); e != nil {
			return library.Result{}, e
		}
		return resultPlaylist(tx, user, p.ID, map[string]any{})
	}
	return library.Result{}, fmt.Errorf("unsupported library command %q", c.Kind)
}
