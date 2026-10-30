package auth

import (
	"strings"
	"testing"
)

func TestEmailPolicy(t *testing.T) {
	for _, email := range []string{"realroc@qq.com", "person@example.com", "first.last+music@example.co.uk", "123456@qq.com"} {
		if e := ValidateNewAccount(email, "zeng", "Password!123"); e != nil {
			t.Fatalf("valid email %s: %v", email, e)
		}
	}
	for _, email := range []string{"not-an-email", "name <a@example.com>", "a@localhost", "a@example.com\nBcc: evil@example.com", " a@example.com", strings.Repeat("a", 65) + "@example.com"} {
		if ValidEmail(email) {
			t.Fatalf("invalid email accepted: %q", email)
		}
	}
	if e := ValidateNewAccount("bad", "zeng", "Password!123"); e == nil || !strings.Contains(e.Error(), "email") {
		t.Fatal("field error missing", e)
	}
	if e := ValidateNewAccount("realroc@qq.com", "a", "Password!123"); e == nil || !strings.Contains(e.Error(), "username") {
		t.Fatal("field error missing", e)
	}
}
