package authstore

import (
	"context"
	"errors"

	"cyapi-go/internal/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db} }

var _ auth.Store = (*Store)(nil)

type userRecord struct {
	ID                                             int64
	Email, Username, PasswordHash                  string
	CreatedAt                                      string `gorm:"autoCreateTime:false"`
	UpdatedAt                                      string `gorm:"autoUpdateTime:false"`
	TokenVersion                                   int64
	IsVerified, IsSponsor                          int
	VerifiedAt, LastLogin, AvatarURL, SponsorSince *string
}

func (userRecord) TableName() string { return "users" }
func (u userRecord) domain() *auth.User {
	return &auth.User{ID: u.ID, Email: u.Email, Username: u.Username, PasswordHash: u.PasswordHash, CreatedAt: u.CreatedAt, TokenVersion: u.TokenVersion, IsVerified: u.IsVerified == 1, IsSponsor: u.IsSponsor == 1, LastLogin: u.LastLogin, AvatarURL: u.AvatarURL, SponsorSince: u.SponsorSince}
}
func (s *Store) find(ctx context.Context, where string, value any) (*auth.User, error) {
	var u userRecord
	err := s.db.WithContext(ctx).Where(where, value).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u.domain(), nil
}
func (s *Store) FindUser(ctx context.Context, account string) (*auth.User, error) {
	var u userRecord
	err := s.db.WithContext(ctx).Where("email = ? OR username = ?", account, account).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u.domain(), nil
}
func (s *Store) FindUserID(ctx context.Context, id int64) (*auth.User, error) {
	return s.find(ctx, "id = ?", id)
}
func (s *Store) Available(ctx context.Context, email, username string) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&userRecord{}).Where("email = ? OR username = ?", email, username).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return auth.ErrConflict
	}
	return nil
}

type codeRecord struct {
	ID                           int64
	Email, Code, Type, ExpiresAt string
	Used                         int
	CreatedAt                    string `gorm:"autoCreateTime:false"`
}

func (codeRecord) TableName() string { return "verification_codes" }
func (s *Store) PrepareCode(ctx context.Context, c auth.Code) (int64, error) {
	row := codeRecord{Email: c.Email, Code: c.Digest, Type: c.Purpose, ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt, Used: 1}
	err := s.db.WithContext(ctx).Create(&row).Error
	return row.ID, err
}
func (s *Store) ActivateCode(ctx context.Context, c auth.Code) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&codeRecord{}).Where("email = ? AND type = ? AND id <> ? AND used = 0", c.Email, c.Purpose, c.ID).Update("used", 1).Error; err != nil {
			return err
		}
		result := tx.Model(&codeRecord{}).Where("id = ? AND email = ? AND type = ? AND used = 1", c.ID, c.Email, c.Purpose).Update("used", 0)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return auth.ErrCode
		}
		return nil
	})
}
func consume(tx *gorm.DB, email, purpose, digest, now string) error {
	candidate := tx.Model(&codeRecord{}).Select("id").Where("email = ? AND type = ? AND code = ? AND used = 0 AND expires_at > ?", email, purpose, digest, now).Order("id DESC").Limit(1)
	result := tx.Model(&codeRecord{}).Where("id = (?) AND used = 0", candidate).Update("used", 1)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return auth.ErrCode
	}
	return nil
}
func (s *Store) Register(ctx context.Context, u auth.User, digest, now string) (*auth.User, error) {
	row := userRecord{Email: u.Email, Username: u.Username, PasswordHash: u.PasswordHash, CreatedAt: now, UpdatedAt: now, IsVerified: 1, VerifiedAt: &now, AvatarURL: u.AvatarURL}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := consume(tx, u.Email, "register", digest, now); err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return auth.ErrConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return row.domain(), nil
}
func (s *Store) ResetPassword(ctx context.Context, email, hash, digest, now string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := consume(tx, email, "reset_password", digest, now); err != nil {
			return err
		}
		result := tx.Model(&userRecord{}).Where("email = ?", email).Updates(map[string]any{"password_hash": hash, "token_version": gorm.Expr("token_version + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return auth.ErrCode
		}
		return nil
	})
}
func (s *Store) RecordLogin(ctx context.Context, id, version int64, now string) error {
	result := s.db.WithContext(ctx).Model(&userRecord{}).Where("id = ? AND token_version = ?", id, version).Updates(map[string]any{"last_login": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return auth.ErrUnauthorized
	}
	return nil
}

type rateRecord struct {
	Key       string `gorm:"primaryKey"`
	Attempts  int
	ExpiresAt int64
}

func (rateRecord) TableName() string { return "auth_rate_limits" }
func (s *Store) Reserve(ctx context.Context, key string, expiry, now int64, limit int) (bool, int64, error) {
	row := rateRecord{Key: key, Attempts: 1, ExpiresAt: expiry}
	result := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"attempts":   gorm.Expr("CASE WHEN auth_rate_limits.expires_at <= ? THEN 1 ELSE auth_rate_limits.attempts + 1 END", now),
			"expires_at": gorm.Expr("CASE WHEN auth_rate_limits.expires_at <= ? THEN excluded.expires_at ELSE auth_rate_limits.expires_at END", now),
		}),
		Where: clause.Where{Exprs: []clause.Expression{gorm.Expr("auth_rate_limits.expires_at <= ? OR auth_rate_limits.attempts < ?", now, limit)}},
	}, clause.Returning{}).Create(&row)
	if result.Error != nil {
		return false, 0, result.Error
	}
	if result.RowsAffected == 1 {
		return true, row.ExpiresAt, nil
	}
	if err := s.db.WithContext(ctx).Where("key = ?", key).Take(&row).Error; err != nil {
		return false, 0, err
	}
	return false, row.ExpiresAt, nil
}
func (s *Store) Refund(ctx context.Context, r auth.Reservation) error {
	return s.db.WithContext(ctx).Model(&rateRecord{}).Where("key = ? AND expires_at = ? AND attempts > 0", r.Key, r.ExpiresAt).Update("attempts", gorm.Expr("attempts - 1")).Error
}
func (s *Store) Prune(ctx context.Context, now int64, stamp string) error {
	db := s.db.WithContext(ctx)
	oldRates := db.Model(&rateRecord{}).Select("key").Where("expires_at <= ?", now).Order("expires_at").Limit(200)
	if err := db.Where("key IN (?)", oldRates).Delete(&rateRecord{}).Error; err != nil {
		return err
	}
	oldCodes := db.Model(&codeRecord{}).Select("id").Where("expires_at <= ?", stamp).Order("expires_at").Limit(100)
	return db.Where("id IN (?)", oldCodes).Delete(&codeRecord{}).Error
}
