package librarystore

import (
	"cyapi-go/internal/library"
	"encoding/json"
)

type playlist struct {
	ID, UserID                     int64
	Name, Type                     string
	IsDefault                      int
	Source, SourcePlaylistID       *string
	ContentRevision, OrderRevision int64
	ActiveSnapshotID               *int64
	SyncStatus                     string
	LastSyncedAt                   *string
	CreatedAt                      string `gorm:"autoCreateTime:false"`
	UpdatedAt                      string `gorm:"autoUpdateTime:false"`
	TrackCount                     int64  `gorm:"->"`
}

func (playlist) TableName() string { return "playlists" }
func (p playlist) public() map[string]any {
	return map[string]any{"id": p.ID, "name": p.Name, "type": p.Type, "isDefault": p.IsDefault == 1, "trackCount": p.TrackCount, "contentRevision": p.ContentRevision, "orderRevision": p.OrderRevision, "source": p.Source, "sourcePlaylistId": p.SourcePlaylistID, "syncStatus": p.SyncStatus, "lastSyncedAt": p.LastSyncedAt, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt}
}

type catalog struct {
	ID                                                  int64
	Source, SourceTrackID, Name, Artists, Album, PicURL string
	SourceIDsJSON                                       *string `gorm:"column:source_ids_json"`
	CreatedAt                                           string  `gorm:"autoCreateTime:false"`
	UpdatedAt                                           string  `gorm:"autoUpdateTime:false"`
}

func (catalog) TableName() string { return "playlist_tracks_catalog" }
func (c catalog) track() library.Track {
	return library.Track{TrackID: c.SourceTrackID, Name: c.Name, Artists: c.Artists, Album: c.Album, PicURL: c.PicURL, Source: c.Source, SourceIDs: library.DecodeSource(c.SourceIDsJSON)}
}

type item struct {
	ID, PlaylistID, TrackID, Rank int64
	AddedAt                       string
}

func (item) TableName() string { return "local_playlist_items" }

type favorite struct {
	ID, UserID                                             int64
	TrackID, Name, Artists, Album, PicURL, Source, AddedAt string
	SourceIDsJSON                                          *string `gorm:"column:source_ids_json"`
}

func (favorite) TableName() string { return "favorites" }
func (f favorite) public() map[string]any {
	return map[string]any{"trackId": f.TrackID, "name": f.Name, "artists": f.Artists, "album": f.Album, "picUrl": f.PicURL, "source": f.Source, "sourceIds": library.DecodeSource(f.SourceIDsJSON), "addedAt": f.AddedAt}
}

type operation struct {
	UserID        int64  `gorm:"primaryKey"`
	OperationID   string `gorm:"primaryKey"`
	Scope, Status string
	ResponseJSON  *string `gorm:"column:response_json"`
	CreatedAt     string  `gorm:"autoCreateTime:false"`
	ExpiresAt     string
}

func (operation) TableName() string  { return "playlist_operations" }
func encode(r library.Result) string { b, _ := json.Marshal(r); return string(b) }
