package auth

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalid         = errors.New("invalid input")
	ErrUnauthorized    = errors.New("invalid credentials or session")
	ErrConflict        = errors.New("email or username already exists")
	ErrCode            = errors.New("invalid or expired verification code")
	ErrMailUnavailable = errors.New("mail unavailable")
	ErrMailDelivery    = errors.New("mail delivery failed")
)

type RateError struct{ RetryAfter int }

func (e *RateError) Error() string { return "too many requests" }

type User struct {
	ID                                       int64
	Email, Username, PasswordHash, CreatedAt string
	TokenVersion                             int64
	IsVerified, IsSponsor                    bool
	LastLogin, AvatarURL, SponsorSince       *string
}

func (u User) Public() map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "username": u.Username, "isVerified": u.IsVerified, "lastLogin": u.LastLogin, "avatarUrl": u.AvatarURL, "isSponsor": u.IsSponsor, "sponsorSince": u.SponsorSince}
}

type Code struct {
	ID                                           int64
	Email, Digest, Purpose, ExpiresAt, CreatedAt string
}
type Reservation struct {
	Key       string
	ExpiresAt int64
}
type Message struct {
	To, Username, Code, Purpose string
	Expires                     time.Duration
}
type Mailer interface {
	Ready() bool
	Send(context.Context, Message) error
}

// Each mutation defines a business-level atomic boundary. No ORM types escape.
type Store interface {
	FindUser(context.Context, string) (*User, error)
	FindUserID(context.Context, int64) (*User, error)
	Available(context.Context, string, string) error
	PrepareCode(context.Context, Code) (int64, error)
	ActivateCode(context.Context, Code) error
	Register(context.Context, User, string, string) (*User, error)
	ResetPassword(context.Context, string, string, string, string) error
	RecordLogin(context.Context, int64, int64, string) error
	Reserve(context.Context, string, int64, int64, int) (bool, int64, error)
	Refund(context.Context, Reservation) error
	Prune(context.Context, int64, string) error
}
