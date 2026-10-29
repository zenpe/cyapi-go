package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const timestamp = "2006-01-02T15:04:05.000000000Z"
const CodeLifetime = 10 * time.Minute

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{4,20}$`)
var emailPattern = regexp.MustCompile(`^[0-9]{5,11}@qq\.com$`)
var codePattern = regexp.MustCompile(`^[0-9]{6}$`)

type Options struct {
	Secret      string
	TokenMaxAge time.Duration
	BcryptCost  int
	Now         func() time.Time
	Logger      *slog.Logger
}
type Service struct {
	store     Store
	mail      Mailer
	signer    *Signer
	cost      int
	now       func() time.Time
	log       *slog.Logger
	slots     chan struct{}
	dummy     []byte
	nextPrune atomic.Int64
}

func New(store Store, mail Mailer, opts Options) (*Service, error) {
	signer, err := NewSigner(opts.Secret, opts.TokenMaxAge)
	if err != nil {
		return nil, err
	}
	if opts.BcryptCost == 0 {
		opts.BcryptCost = 10
	}
	if opts.BcryptCost < bcrypt.MinCost || opts.BcryptCost > 14 {
		return nil, errors.New("invalid bcrypt cost")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("cyapi-no-such-account"), opts.BcryptCost)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, mail: mail, signer: signer, cost: opts.BcryptCost, now: opts.Now, log: opts.Logger, slots: make(chan struct{}, 2), dummy: dummy}, nil
}
func normalizeEmail(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func validPassword(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 8 && len(value) <= 72
}
func (s *Service) withSlot(ctx context.Context, work func() error) error {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}
func (s *Service) digest(email, purpose, code string) string {
	return hex.EncodeToString(s.signer.mac("otp:" + email + ":" + purpose + ":" + code))
}
func (s *Service) RegistrationEnabled() bool { return s.mail != nil && s.mail.Ready() }
func (s *Service) rate(ctx context.Context, action, account, ip string, sending bool) (*Reservation, error) {
	now := s.now()
	window := 15 * time.Minute
	ipLimit, accountLimit := 100, 10
	if sending {
		window = time.Minute
		ipLimit, accountLimit = 10, 1
	}
	// At most one bounded cleanup each minute per process, not on every request.
	next := s.nextPrune.Load()
	if now.Unix() >= next && s.nextPrune.CompareAndSwap(next, now.Unix()+60) {
		if err := s.store.Prune(ctx, now.UnixMilli(), now.UTC().Format(timestamp)); err != nil {
			return nil, err
		}
	}
	expires := now.Add(window).UnixMilli()
	scopes := []struct {
		value string
		limit int
	}{{"ip:" + ip, ipLimit}, {"account:" + strings.ToLower(strings.TrimSpace(account)), accountLimit}}
	var reservation *Reservation
	for index, scope := range scopes {
		hash := sha256.Sum256([]byte(action + ":" + scope.value))
		key := hex.EncodeToString(hash[:])
		allowed, expiry, err := s.store.Reserve(ctx, key, expires, now.UnixMilli(), scope.limit)
		if err != nil {
			return nil, err
		}
		if !allowed {
			delay := int((expiry - now.UnixMilli() + 999) / 1000)
			if delay < 1 {
				delay = 1
			}
			return nil, &RateError{delay}
		}
		if index == 1 {
			reservation = &Reservation{key, expiry}
		}
	}
	return reservation, nil
}
func (s *Service) SendCode(ctx context.Context, email, username, purpose, ip string) error {
	if !s.RegistrationEnabled() {
		return ErrMailUnavailable
	}
	email = normalizeEmail(email)
	username = strings.TrimSpace(username)
	if !emailPattern.MatchString(email) || (purpose != "register" && purpose != "reset_password") {
		return ErrInvalid
	}
	if purpose == "register" && !usernamePattern.MatchString(username) {
		return ErrInvalid
	}
	action := "send-register"
	if purpose == "reset_password" {
		action = "send-reset"
	}
	if _, err := s.rate(ctx, action, email, ip, true); err != nil {
		return err
	}
	if purpose == "register" {
		if err := s.store.Available(ctx, email, username); err != nil {
			return err
		}
	} else {
		user, err := s.store.FindUser(ctx, email)
		if err != nil {
			return err
		}
		if user == nil {
			return nil
		}
		username = user.Username
	}
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return err
	}
	plain := fmt.Sprintf("%06d", n.Int64()+100000)
	now := s.now().UTC()
	c := Code{Email: email, Digest: s.digest(email, purpose, plain), Purpose: purpose, ExpiresAt: now.Add(CodeLifetime).Format(timestamp), CreatedAt: now.Format(timestamp)}
	c.ID, err = s.store.PrepareCode(ctx, c)
	if err != nil {
		return err
	}
	// Pending codes are not usable until delivery succeeds. No transaction spans mail IO.
	if err = s.mail.Send(ctx, Message{email, username, plain, purpose, CodeLifetime}); err != nil {
		s.log.Warn("verification email delivery failed")
		return ErrMailDelivery
	}
	return s.store.ActivateCode(ctx, c)
}
func (s *Service) Register(ctx context.Context, email, username, password, code, ip string) (*User, error) {
	email = normalizeEmail(email)
	username = strings.TrimSpace(username)
	if !emailPattern.MatchString(email) || !usernamePattern.MatchString(username) || !validPassword(password) || !codePattern.MatchString(code) {
		return nil, ErrInvalid
	}
	if _, err := s.rate(ctx, "verify-register", email, ip, false); err != nil {
		return nil, err
	}
	var hash []byte
	if err := s.withSlot(ctx, func() error { var e error; hash, e = bcrypt.GenerateFromPassword([]byte(password), s.cost); return e }); err != nil {
		return nil, err
	}
	now := s.now().UTC().Format(timestamp)
	avatar := "https://q1.qlogo.cn/g?b=qq&nk=" + strings.TrimSuffix(email, "@qq.com") + "&s=100"
	user := User{Email: email, Username: username, PasswordHash: string(hash), CreatedAt: now, IsVerified: true, AvatarURL: &avatar}
	return s.store.Register(ctx, user, s.digest(email, "register", code), now)
}
func (s *Service) Login(ctx context.Context, account, password, ip string) (user *User, token string, err error) {
	account = strings.TrimSpace(account)
	if account == "" || len(account) > 254 || password == "" || len(password) > 72 || !utf8.ValidString(password) {
		return nil, "", ErrInvalid
	}
	reservation, err := s.rate(ctx, "login", account, ip, false)
	if err != nil {
		return nil, "", err
	}
	credentialsRejected := false
	defer func() {
		if reservation != nil && !credentialsRejected {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if e := s.store.Refund(cleanup, *reservation); e != nil {
				s.log.Warn("login rate reservation refund failed")
			}
		}
	}()
	if strings.Contains(account, "@") {
		account = normalizeEmail(account)
	}
	user, err = s.store.FindUser(ctx, account)
	if err != nil {
		return nil, "", err
	}
	hash := s.dummy
	if user != nil {
		hash = []byte(user.PasswordHash)
	}
	err = s.withSlot(ctx, func() error { return bcrypt.CompareHashAndPassword(hash, []byte(password)) })
	if err != nil && !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return nil, "", err
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) || user == nil {
		credentialsRejected = true
		return nil, "", ErrUnauthorized
	}
	now := s.now()
	stamp := now.UTC().Format(timestamp)
	if err = s.store.RecordLogin(ctx, user.ID, user.TokenVersion, stamp); err != nil {
		return nil, "", err
	}
	user.LastLogin = &stamp
	token, err = s.signer.Issue(user.ID, user.TokenVersion, now)
	return
}
func (s *Service) ResetPassword(ctx context.Context, email, password, code, ip string) error {
	email = normalizeEmail(email)
	if !emailPattern.MatchString(email) || !validPassword(password) || !codePattern.MatchString(code) {
		return ErrInvalid
	}
	if _, err := s.rate(ctx, "verify-reset", email, ip, false); err != nil {
		return err
	}
	var hash []byte
	if err := s.withSlot(ctx, func() error { var e error; hash, e = bcrypt.GenerateFromPassword([]byte(password), s.cost); return e }); err != nil {
		return err
	}
	return s.store.ResetPassword(ctx, email, string(hash), s.digest(email, "reset_password", code), s.now().UTC().Format(timestamp))
}
func (s *Service) Authenticate(ctx context.Context, token string) (*User, error) {
	claims, err := s.signer.Verify(token, s.now())
	if err != nil {
		return nil, ErrUnauthorized
	}
	user, err := s.store.FindUserID(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	if user == nil || user.TokenVersion != claims.Version {
		return nil, ErrUnauthorized
	}
	return user, nil
}
