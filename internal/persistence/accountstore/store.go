package accountstore

import (
	"context"
	"cyapi-go/internal/library"
	"encoding/json"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Binding struct {
	UserID      int64
	Provider    string
	PayloadJSON string
	UpdatedAt   string
	Revision    int64
	Status      string
	Credentials string
}

func (Binding) TableName() string { return "account_bindings" }

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db} }
func (s *Store) Get(ctx context.Context, user int64) (Binding, error) {
	b := Binding{UserID: user, Provider: "netease", Status: "unbound"}
	e := s.db.WithContext(ctx).Where("user_id = ? AND provider = ?", user, "netease").Take(&b).Error
	if e == gorm.ErrRecordNotFound {
		return Binding{UserID: user, Provider: "netease", Status: "unbound"}, nil
	}
	return b, e
}
func (s *Store) Change(ctx context.Context, user, expected int64, status, secret string, profile map[string]any) (Binding, error) {
	var out Binding
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		initial := Binding{UserID: user, Provider: "netease", Status: "unbound"}
		if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; e != nil {
			return e
		}
		payload, _ := json.Marshal(profile)
		result := tx.Model(&Binding{}).Where("user_id = ? AND provider = ? AND revision = ?", user, "netease", expected).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "status": status, "credentials": secret, "payload_json": string(payload), "updated_at": gorm.Expr("CURRENT_TIMESTAMP")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return library.Fail(409, "BINDING_CHANGED", "账号绑定已变化，请重试")
		}
		return tx.Where("user_id = ? AND provider = ?", user, "netease").Take(&out).Error
	})
	return out, e
}
