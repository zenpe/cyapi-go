package librarystore

import (
	"context"
	"cyapi-go/internal/library"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"time"
)

type snapshot struct {
	ID, PlaylistID, BindingVersion int64
	ExpectedCount, ItemCount       int
	Status, CreatedAt              string
	CompletedAt                    *string
}

func (snapshot) TableName() string { return "playlist_snapshots" }

type snapshotItem struct {
	SnapshotID, TrackID             int64
	SourcePosition, OccurrenceIndex int
}

func (snapshotItem) TableName() string { return "playlist_snapshot_items" }

var _ library.RemoteStore = (*Store)(nil)

func (s *Store) BeginRemote(ctx context.Context, user int64, c library.Command) (library.RemoteTicket, *library.Result, error) {
	now := s.now().UTC()
	at := now.Format(stamp)
	ticket := library.RemoteTicket{Command: c, StartedAt: at}
	var replay *library.Result
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := s.pruneOperations(tx, now); e != nil {
			return e
		}
		if e := tx.Where("user_id = ? AND operation_id = ? AND expires_at <= ?", user, c.OperationID, at).Delete(&operation{}).Error; e != nil {
			return e
		}
		op := operation{UserID: user, OperationID: c.OperationID, Scope: c.Scope(), Status: "pending", CreatedAt: at, ExpiresAt: now.Add(5 * time.Minute).Format(stamp)}
		q := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&op)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 0 {
			var old operation
			if e := tx.Where("user_id = ? AND operation_id = ?", user, c.OperationID).Take(&old).Error; e != nil {
				return e
			}
			if old.Scope != op.Scope {
				return library.Fail(409, "OPERATION_SCOPE_MISMATCH", "operationId 已用于其他操作")
			}
			if old.ResponseJSON == nil {
				return library.Fail(409, "OPERATION_PENDING", "操作仍在处理中")
			}
			replay = &library.Result{}
			return json.Unmarshal([]byte(*old.ResponseJSON), replay)
		}
		if c.Kind == "follow" {
			var count int64
			if e := tx.Model(&playlist{}).Where("user_id = ? AND type = 'synced' AND source = ? AND source_playlist_id = ?", user, c.Source, c.SourcePlaylistID).Count(&count).Error; e != nil {
				return e
			}
			if count > 0 {
				return library.Fail(409, "SOURCE_ALREADY_FOLLOWED", "该来源歌单已关注")
			}
			return nil
		}
		p, e := lock(tx, user, c.PlaylistID)
		if e != nil {
			return e
		}
		if p.Type != "synced" {
			return library.Fail(409, "LOCAL_PLAYLIST_NOT_SYNCABLE", "本地歌单无需同步")
		}
		// A previous process may have died while fetching. A newer revision fences its eventual completion.
		if p.SyncStatus == "syncing" && p.UpdatedAt > now.Add(-5*time.Minute).Format(stamp) {
			return library.Fail(409, "SYNC_IN_PROGRESS", "歌单正在同步中")
		}
		if p.Source == nil || p.SourcePlaylistID == nil {
			return library.Fail(422, "INVALID_SOURCE", "来源绑定无效")
		}
		ticket.Command.Source = *p.Source
		ticket.Command.SourcePlaylistID = *p.SourcePlaylistID
		ticket.BindingVersion = p.BindingVersion
		ticket.SyncRevision = p.SyncRevision + 1
		return tx.Model(&playlist{}).Where("id = ?", p.ID).Updates(map[string]any{"sync_revision": ticket.SyncRevision, "sync_status": "syncing", "updated_at": at}).Error
	})
	return ticket, replay, e
}
func (s *Store) FinishRemote(ctx context.Context, user int64, t library.RemoteTicket, remote library.RemoteSnapshot, failure *library.Failure) (library.Result, error) {
	var out library.Result
	now := s.now().UTC()
	at := now.Format(stamp)
	c := t.Command
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Model(&operation{}).Where("user_id = ? AND operation_id = ? AND scope = ? AND status = 'pending' AND created_at = ? AND expires_at > ?", user, c.OperationID, c.Scope(), t.StartedAt, at).UpdateColumn("status", "pending")
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 1 {
			return library.Fail(409, "OPERATION_SUPERSEDED", "操作已过期或被更新，请重新操作")
		}
		workErr := tx.Transaction(func(work *gorm.DB) error {
			var p playlist
			if c.Kind == "sync" {
				var err error
				p, err = lock(work, user, c.PlaylistID)
				if err != nil {
					return err
				}
				if p.Type != "synced" || p.BindingVersion != t.BindingVersion || p.SyncRevision != t.SyncRevision || p.SyncStatus != "syncing" || p.Source == nil || p.SourcePlaylistID == nil || *p.Source != c.Source || *p.SourcePlaylistID != c.SourcePlaylistID {
					return library.Fail(409, "SYNC_SUPERSEDED", "来源绑定或同步版本已变化")
				}
			}
			if failure != nil {
				return failure
			}
			if c.Kind == "follow" {
				name := c.Name
				if name == "" {
					name = remote.Name
				}
				p = playlist{UserID: user, Name: name, Type: "synced", Source: &c.Source, SourcePlaylistID: &c.SourcePlaylistID, BindingVersion: 1, SyncStatus: "syncing", CreatedAt: at, UpdatedAt: at}
				q := work.Clauses(clause.OnConflict{DoNothing: true}).Create(&p)
				if q.Error != nil {
					return q.Error
				}
				if q.RowsAffected != 1 {
					return library.Fail(409, "SOURCE_ALREADY_FOLLOWED", "该来源歌单已关注")
				}
			}
			if err := writeSnapshot(work, p, remote, at); err != nil {
				return err
			}
			var err error
			out, err = resultPlaylist(work, user, p.ID, map[string]any{})
			return err
		})
		state := "succeeded"
		if workErr != nil {
			var f *library.Failure
			if !errors.As(workErr, &f) {
				return workErr
			}
			out = library.Rejected(f)
			state = "failed"
			if c.Kind == "sync" {
				if e := tx.Model(&playlist{}).Where("id = ? AND user_id = ? AND binding_version = ? AND sync_revision = ? AND sync_status = 'syncing'", c.PlaylistID, user, t.BindingVersion, t.SyncRevision).Updates(map[string]any{"sync_status": "error", "updated_at": at}).Error; e != nil {
					return e
				}
			}
		}
		body := encode(out)
		return tx.Model(&operation{}).Where("user_id = ? AND operation_id = ? AND created_at = ?", user, c.OperationID, t.StartedAt).Updates(map[string]any{"status": state, "response_json": body, "expires_at": now.Add(24 * time.Hour).Format(stamp)}).Error
	})
	if e != nil {
		// On storage failure no business mutation committed. Release only this task's lease so the same ID can retry.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.db.WithContext(cleanup).Transaction(func(tx *gorm.DB) error {
			q := tx.Where("user_id = ? AND operation_id = ? AND created_at = ? AND status = 'pending'", user, c.OperationID, t.StartedAt).Delete(&operation{})
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected == 0 {
				return nil
			}
			if c.Kind == "sync" {
				return tx.Model(&playlist{}).Where("id = ? AND user_id = ? AND binding_version = ? AND sync_revision = ? AND sync_status = 'syncing'", c.PlaylistID, user, t.BindingVersion, t.SyncRevision).Updates(map[string]any{"sync_status": "error", "updated_at": at}).Error
			}
			return nil
		})
	}
	return out, e
}
func writeSnapshot(tx *gorm.DB, p playlist, remote library.RemoteSnapshot, at string) error {
	snap := snapshot{PlaylistID: p.ID, BindingVersion: p.BindingVersion, ExpectedCount: remote.ExpectedCount, ItemCount: len(remote.Tracks), Status: "complete", CreatedAt: at, CompletedAt: &at}
	if err := tx.Create(&snap).Error; err != nil {
		return err
	}
	unique := map[string]library.Track{}
	for _, t := range remote.Tracks {
		unique[t.Source+"\x00"+t.TrackID] = t.Track
	}
	keys := make([]string, 0, len(unique))
	for k := range unique {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]catalog, 0, len(keys))
	for _, key := range keys {
		t := unique[key]
		rows = append(rows, catalog{Source: t.Source, SourceTrackID: t.TrackID, Name: t.Name, Artists: t.Artists, Album: t.Album, PicURL: t.PicURL, SourceIDsJSON: t.SourceJSON(), CreatedAt: at, UpdatedAt: at})
	}
	if len(rows) > 0 {
		err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "source"}, {Name: "source_track_id"}}, DoUpdates: clause.AssignmentColumns([]string{"name", "artists", "album", "pic_url", "updated_at"})}, clause.Returning{Columns: []clause.Column{{Name: "id"}}}).CreateInBatches(&rows, 100).Error
		if err != nil {
			return err
		}
	}
	ids := map[string]int64{}
	for i, key := range keys {
		ids[key] = rows[i].ID
	}
	items := make([]snapshotItem, 0, len(remote.Tracks))
	for _, t := range remote.Tracks {
		items = append(items, snapshotItem{SnapshotID: snap.ID, TrackID: ids[t.Source+"\x00"+t.TrackID], SourcePosition: t.SourcePosition, OccurrenceIndex: t.OccurrenceIndex})
	}
	if len(items) > 0 {
		if err := tx.CreateInBatches(&items, 100).Error; err != nil {
			return err
		}
	}
	var count int64
	if err := tx.Model(&snapshotItem{}).Where("snapshot_id = ?", snap.ID).Count(&count).Error; err != nil {
		return err
	}
	if count != int64(remote.ExpectedCount) {
		return library.Fail(422, "SOURCE_SNAPSHOT_INCOMPLETE", "快照校验失败")
	}
	q := tx.Model(&playlist{}).Where("id = ? AND type = 'synced' AND binding_version = ? AND sync_revision = ? AND sync_status = 'syncing'", p.ID, p.BindingVersion, p.SyncRevision).Updates(map[string]any{"active_snapshot_id": snap.ID, "sync_status": "ready", "content_revision": gorm.Expr("content_revision + 1"), "last_synced_at": at, "updated_at": at})
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return library.Fail(409, "SYNC_SUPERSEDED", "同步版本已变化")
	}
	if err := tx.Model(&snapshot{}).Where("playlist_id = ? AND id <> ?", p.ID, snap.ID).Update("status", "retired").Error; err != nil {
		return err
	}
	// Keep the immediately preceding snapshot; readers use a repeatable transaction during cleanup.
	var old []int64
	if err := tx.Model(&snapshot{}).Where("playlist_id = ? AND status = 'retired'", p.ID).Order("id DESC").Pluck("id", &old).Error; err != nil {
		return err
	}
	if len(old) > 1 {
		return tx.Where("id IN ?", old[1:]).Delete(&snapshot{}).Error
	}
	return nil
}
func copySnapshot(tx *gorm.DB, user int64, p playlist, c library.Command, at string) (library.Result, error) {
	if p.Type != "synced" {
		return library.Result{}, library.Fail(409, "LOCAL_PLAYLIST_NOT_COPYABLE", "仅同步歌单需要复制")
	}
	if p.ActiveSnapshotID == nil {
		return library.Result{}, library.Fail(409, "SOURCE_SNAPSHOT_INCOMPLETE", "尚无完整来源快照")
	}
	name := c.Name
	if name == "" {
		name = p.Name + " - 本地副本"
		r := []rune(name)
		if len(r) > 200 {
			name = string(r[:200])
		}
	}
	local := playlist{UserID: user, Name: name, Type: "local", ContentRevision: 1, OrderRevision: 1, SyncStatus: "idle", CreatedAt: at, UpdatedAt: at}
	if err := tx.Create(&local).Error; err != nil {
		return library.Result{}, err
	}
	var rows []snapshotItem
	if err := tx.Where("snapshot_id = ?", *p.ActiveSnapshotID).Order("source_position ASC, occurrence_index ASC").Find(&rows).Error; err != nil {
		return library.Result{}, err
	}
	seen := map[int64]bool{}
	items := make([]item, 0, len(rows))
	for _, r := range rows {
		if seen[r.TrackID] {
			continue
		}
		seen[r.TrackID] = true
		items = append(items, item{PlaylistID: local.ID, TrackID: r.TrackID, Rank: int64(len(items)) * 1024, AddedAt: at})
	}
	if len(items) > library.MaxItems {
		return library.Result{}, library.Fail(409, "PLAYLIST_LIMIT", "单个歌单最多10000首歌曲")
	}
	if len(items) > 0 {
		if err := tx.CreateInBatches(&items, 100).Error; err != nil {
			return library.Result{}, err
		}
	}
	return resultPlaylist(tx, user, local.ID, map[string]any{"copiedCount": len(items)})
}
