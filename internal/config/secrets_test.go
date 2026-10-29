package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralSecretsAndEnvironmentPrecedence(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(configPath, []byte("{}"), 0600)
	os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("CYAPI_AUTH_SECRET=012345678901234567890123456789012345\nCYAPI_TEST_LITERAL=$(this-is-not-executed)\n"), 0600)
	c, e := Load(configPath)
	if e != nil {
		t.Fatal(e)
	}
	if c.Env("CYAPI_TEST_LITERAL") != "$(this-is-not-executed)" {
		t.Fatal("literal value changed")
	}
	t.Setenv("CYAPI_AUTH_SECRET", "environment-wins")
	s, _ := c.Secret()
	if s != "environment-wins" {
		t.Fatal("process env did not win")
	}
	os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("SECRET=must-not-leak\nSECRET=other\n"), 0600)
	_, e = Load(configPath)
	if e == nil || strings.Contains(e.Error(), "must-not-leak") {
		t.Fatal("duplicate key accepted or exposed")
	}
}
