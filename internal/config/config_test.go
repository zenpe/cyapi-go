package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestRelativePathsAndNoSecretNeeded(t *testing.T) {
	p := write(t, "database:\n  dsn: db/main.db\n  busy_timeout: 250ms\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.DSN != filepath.Join(filepath.Dir(p), "db/main.db") || c.Database.BusyTimeout != 250*time.Millisecond {
		t.Fatalf("unexpected config: %+v", c.Database)
	}
	if c.Sources.Directory != filepath.Join(filepath.Dir(p), "data/sources") {
		t.Fatal("source path resolution")
	}
	t.Setenv("CYAPI_AUTH_SECRET", "")
	if _, err = c.Secret(); err == nil {
		t.Fatal("serve must require a secret")
	}
}
func TestRejectsInvalidAndRedactsYAML(t *testing.T) {
	for _, body := range []string{"database:\n  driver: mysql\n", "database:\n  busy_timeout: 0s\n", "server:\n  trusted_proxies: [all]\n", "database:\n  dsn: ':memory:'\n", "server:\n  public_base_url: https://user:password@example.com\n", "server:\n  cors_allowed_origins: ['*']\n", "unknown_secret: supersecret\n", "{}\n---\n{}\n"} {
		_, err := Load(write(t, body))
		if err == nil {
			t.Fatalf("accepted %q", body)
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Fatal("secret leaked")
		}
	}
}
func TestDSNEnvAndExplicitOverride(t *testing.T) {
	p := write(t, "database:\n  driver: postgres\n  dsn_env: TEST_CYAPI_PG_DSN\n")
	t.Setenv("TEST_CYAPI_PG_DSN", "postgres://configured")
	c, err := Load(p)
	if err != nil || c.Database.DSN != "postgres://configured" {
		t.Fatal(c.Database, err)
	}
	t.Setenv("CYAPI_DATABASE_DSN", "postgres://override")
	c, err = Load(p)
	if err != nil || c.Database.DSN != "postgres://override" {
		t.Fatal(c.Database, err)
	}
}
