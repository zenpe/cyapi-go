package persistence

import (
	"context"
	"cyapi-go/internal/config"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDB(t *testing.T, driver, dsn string) *Database {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d, err := Open(ctx, config.Database{Driver: driver, DSN: dsn, BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func fresh(t *testing.T) *Database {
	t.Helper()
	return testDB(t, "sqlite", filepath.Join(t.TempDir(), "app.db"))
}
func TestSQLiteConnectionReplacement(t *testing.T) {
	d := fresh(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	check := func() {
		conn, err := d.SQL.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		for key, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 100, "synchronous": 2} {
			var got int
			if err = conn.QueryRowContext(ctx, "PRAGMA "+key).Scan(&got); err != nil || got != want {
				t.Fatalf("%s: %d %v", key, got, err)
			}
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO local_playlist_items(playlist_id,track_id,rank) VALUES (999,999,1)"); err == nil {
			t.Fatal("orphan FK accepted")
		}
	}
	check()
	d.SQL.SetMaxIdleConns(0)
	check()
	d.SQL.SetMaxIdleConns(1)
	check()
}
func TestSchemaHistoryGuards(t *testing.T) {
	d := fresh(t)
	ctx := context.Background()
	if d.CheckSchema(ctx) == nil {
		t.Fatal("unmigrated schema accepted")
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := d.SQL.Exec("UPDATE cyapi_schema_migrations SET checksum='tampered'")
	if err != nil {
		t.Fatal(err)
	}
	if d.CheckSchema(ctx) == nil || d.Migrate(ctx) == nil {
		t.Fatal("checksum mismatch accepted")
	}
}
func TestRefusesUnmanagedDatabase(t *testing.T) {
	d := fresh(t)
	if _, err := d.SQL.Exec("CREATE TABLE legacy_data(id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if d.Migrate(context.Background()) == nil {
		t.Fatal("adopted legacy database")
	}
	var exists int
	if err := d.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='cyapi_schema_migrations'").Scan(&exists); err != nil || exists != 0 {
		t.Fatal("refusal modified schema", err)
	}
}
func TestConcurrentMigrations(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.db")
	a := testDB(t, "sqlite", p)
	b := testDB(t, "sqlite", p)
	// Establish WAL before contention; concurrent version applications still serialize.
	if _, err := a.SQL.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, d := range []*Database{a, b} {
		wg.Add(1)
		go func(d *Database) { defer wg.Done(); errs <- d.Migrate(context.Background()) }(d)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	a.SQL.QueryRow("SELECT count(*) FROM cyapi_schema_migrations").Scan(&count)
	if count != 2 {
		t.Fatal(count)
	}
}
func TestSQLiteLockWaitIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.db")
	a := testDB(t, "sqlite", p)
	b := testDB(t, "sqlite", p)
	if err := a.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := a.SQL.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = b.SQL.ExecContext(ctx, "INSERT INTO users(email,username,password_hash) VALUES ('locked','locked','x')")
	if err == nil {
		t.Fatal("write ignored lock")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("lock wait unbounded")
	}
}
func contract(t *testing.T, d *Database) {
	t.Helper()
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if d.ORM.Migrator().HasTable("qr_login_sessions") {
		t.Fatal("deferred QR login schema remains active")
	}
	// Explicit model: no soft deletes or implicit schema changes.
	type user struct {
		ID           int64
		Email        string
		Username     string
		PasswordHash string
		AvatarURL    *string
	}
	u := user{Email: "contract@example.test", Username: "contract", PasswordHash: "x"}
	if err := d.ORM.Table("users").Create(&u).Error; err != nil || u.ID <= 0 {
		t.Fatal("generated ID", err, u.ID)
	}
	if err := d.ORM.Table("users").Create(&user{Email: u.Email, Username: "duplicate", PasswordHash: "x"}).Error; err == nil {
		t.Fatal("unique constraint missing")
	}
	sentinel := errors.New("rollback")
	err := d.ORM.Transaction(func(tx *gorm.DB) error {
		if e := tx.Table("users").Where("id = ?", u.ID).Update("username", "changed").Error; e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var got user
	if err = d.ORM.Table("users").Where("id = ?", u.ID).Take(&got).Error; err != nil || got.Username != "contract" || got.AvatarURL != nil {
		t.Fatal("rollback/NULL", got, err)
	}
	if err = d.ORM.Table("users").Where("id = ?", u.ID).Updates(map[string]any{"is_verified": 0, "avatar_url": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err = d.ORM.Exec("INSERT INTO local_playlist_items(playlist_id,track_id,rank) VALUES (?,?,?)", int64(999999), int64(999999), 1).Error; err == nil {
		t.Fatal("foreign key constraint missing")
	}
	// The legacy API permits fractional listening seconds.
	if err = d.ORM.Exec("INSERT INTO listening_stats(user_id,total_listening_time) VALUES (?,?)", u.ID, 1.5).Error; err != nil {
		t.Fatal(err)
	}
	var seconds float64
	if err = d.ORM.Raw("SELECT total_listening_time FROM listening_stats WHERE user_id=?", u.ID).Scan(&seconds).Error; err != nil || seconds != 1.5 {
		t.Fatal("fractional time", seconds, err)
	}
}
func TestSQLiteContract(t *testing.T) { contract(t, fresh(t)) }
func TestPostgresContract(t *testing.T) {
	dsn := os.Getenv("CYAPI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL integration requires CYAPI_TEST_POSTGRES_DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgres" {
		t.Fatal("test DSN must be a postgres:// URL")
	}
	admin := testDB(t, "postgres", dsn)
	schema := fmt.Sprintf("cyapi_contract_%d", time.Now().UnixNano())
	if err = admin.ORM.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.ORM.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Error(err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	contract(t, testDB(t, "postgres", u.String()))
}

func TestFutureSchemaRejected(t *testing.T) {
	d := fresh(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.Exec("INSERT INTO cyapi_schema_migrations(name,checksum,applied_at) VALUES ('9999_future.sql','future','now')"); err != nil {
		t.Fatal(err)
	}
	if d.CheckSchema(ctx) == nil || d.Migrate(ctx) == nil {
		t.Fatal("older binary accepted future schema")
	}
}
func TestMigrationDDLFailureRollsBack(t *testing.T) {
	d := fresh(t)
	ctx := context.Background()
	// Simulate a damaged empty migration history with a conflicting pre-existing table.
	if _, err := d.SQL.Exec("CREATE TABLE cyapi_schema_migrations(name TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL); CREATE TABLE playlists(marker TEXT)"); err != nil {
		t.Fatal(err)
	}
	if d.Migrate(ctx) == nil {
		t.Fatal("expected baseline collision")
	}
	var count int
	if err := d.SQL.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='users'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial migration escaped rollback", count, err)
	}
	if err := d.SQL.QueryRow("SELECT count(*) FROM cyapi_schema_migrations").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed migration recorded", count, err)
	}
}

func TestMigrationLockWaitHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	a, b := testDB(t, "sqlite", path), testDB(t, "sqlite", path)
	if err := a.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := a.SQL.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err = b.Migrate(ctx); err == nil {
		t.Fatal("migration ignored another writer")
	}
	if time.Since(began) > 2*time.Second {
		t.Fatal("migration ignored caller deadline")
	}
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err = b.CheckSchema(context.Background()); err != nil {
		t.Fatal("failed lock acquisition changed schema", err)
	}
}
