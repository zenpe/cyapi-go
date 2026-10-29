package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maxSafeID int64 = 9007199254740991

type Claims struct{ UserID, Version int64 }
type Signer struct {
	secret []byte
	ttl    time.Duration
}

func NewSigner(secret string, ttl time.Duration) (*Signer, error) {
	if len(secret) < 32 {
		return nil, errors.New("AUTH_SECRET must contain at least 32 bytes")
	}
	if ttl < time.Second || ttl > 365*24*time.Hour {
		return nil, errors.New("token lifetime must be between 1s and 365d")
	}
	return &Signer{[]byte(secret), ttl}, nil
}
func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.secret)
	_, _ = m.Write([]byte(payload))
	return m.Sum(nil)
}
func (s *Signer) Issue(id, version int64, now time.Time) (string, error) {
	if id <= 0 || id > maxSafeID || version < 0 || version > maxSafeID {
		return "", ErrUnauthorized
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	iat := now.Unix()
	exp := iat + int64(s.ttl/time.Second)
	payload := fmt.Sprintf("cy1.%d.%d.%d.%d.%x", id, iat, exp, version, nonce)
	return payload + "." + hex.EncodeToString(s.mac(payload)), nil
}
func (s *Signer) Verify(token string, now time.Time) (Claims, error) {
	var c Claims
	if len(token) > 256 {
		return c, ErrUnauthorized
	}
	parts := strings.Split(token, ".")
	if len(parts) != 7 || parts[0] != "cy1" {
		return c, ErrUnauthorized
	}
	values := make([]int64, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil || v < 0 || v > maxSafeID || strconv.FormatInt(v, 10) != parts[i+1] {
			return c, ErrUnauthorized
		}
		values[i] = v
	}
	id, iat, exp, version := values[0], values[1], values[2], values[3]
	if id <= 0 || iat > now.Unix()+60 || exp <= iat || exp-iat > int64(s.ttl/time.Second) || now.Unix() >= exp {
		return c, ErrUnauthorized
	}
	nonce, e1 := hex.DecodeString(parts[5])
	signature, e2 := hex.DecodeString(parts[6])
	if e1 != nil || e2 != nil || len(nonce) != 16 || len(signature) != 32 {
		return c, ErrUnauthorized
	}
	if !hmac.Equal(signature, s.mac(strings.Join(parts[:6], "."))) {
		return c, ErrUnauthorized
	}
	return Claims{id, version}, nil
}
