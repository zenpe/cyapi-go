package auth

import (
	"strings"
	"testing"
	"time"
)

func TestTokenBoundaries(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s, err := NewSigner(strings.Repeat("s", 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Issue(42, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(token, now)
	if err != nil || c.UserID != 42 || c.Version != 3 {
		t.Fatal(c, err)
	}
	other, _ := s.Issue(42, 3, now)
	if other == token {
		t.Fatal("token nonce reused")
	}
	for name, tc := range map[string]struct {
		token string
		now   time.Time
	}{
		"expired": {token, now.Add(time.Hour)}, "future": {token, now.Add(-2 * time.Minute)},
		"tampered": {strings.Replace(token, "cy1.42.", "cy1.43.", 1), now},
		"legacy":   {"42.1800000000.abcdef", now}, "oversize": {strings.Repeat("x", 300), now},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Verify(tc.token, tc.now); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	if _, err := NewSigner("short", time.Hour); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := NewSigner(strings.Repeat("s", 32), 0); err == nil {
		t.Fatal("zero TTL accepted")
	}
	if _, err := s.Issue(0, 0, now); err == nil {
		t.Fatal("zero ID accepted")
	}
}
