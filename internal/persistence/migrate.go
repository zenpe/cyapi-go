package persistence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*/*.sql
var migrationFS embed.FS

type migration struct{ Name, Body, Checksum string }

func migrations(driver string) ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations/" + driver)
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + driver + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{e.Name(), string(b), fmt.Sprintf("%x", sha256.Sum256(b))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func applied(ctx context.Context, q queryer) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, checksum FROM cyapi_schema_migrations ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, s string
		if err = rows.Scan(&n, &s); err != nil {
			return nil, err
		}
		out[n] = s
	}
	return out, rows.Err()
}
func verify(ms []migration, done map[string]string, complete bool) error {
	missing := false
	for _, m := range ms {
		s, ok := done[m.Name]
		if !ok {
			missing = true
			if complete {
				return errors.New("schema is behind this binary; run migrate using the new binary")
			}
			continue
		}
		if missing {
			return errors.New("migration history contains a gap")
		}
		if s != m.Checksum {
			return errors.New("migration checksum mismatch")
		}
		delete(done, m.Name)
	}
	if len(done) > 0 {
		return errors.New("database has migrations unknown to this binary")
	}
	return nil
}
func (d *Database) CheckSchema(ctx context.Context) error {
	ms, err := migrations(d.Driver)
	if err != nil {
		return err
	}
	done, err := applied(ctx, d.SQL)
	if err != nil {
		return errors.New("schema unavailable; run migrate before serve")
	}
	return verify(ms, done, true)
}

// Migrate is explicit, serialized across processes, and transactional. Never runs from serve.
func (d *Database) Migrate(ctx context.Context) error {
	ms, err := migrations(d.Driver)
	if err != nil {
		return err
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if d.Driver == "sqlite" {
		var mode string
		if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return err
		}
		if strings.ToLower(mode) != "wal" {
			return errors.New("SQLite WAL mode could not be enabled")
		}
	}
	begin := "BEGIN"
	if d.Driver == "sqlite" {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cleanup, "ROLLBACK")
	}()
	if d.Driver == "postgres" {
		if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_xact_lock(630912341)"); err != nil {
			return err
		}
	}
	var exists bool
	if d.Driver == "sqlite" {
		err = conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='cyapi_schema_migrations')").Scan(&exists)
	} else {
		err = conn.QueryRowContext(ctx, "SELECT to_regclass('cyapi_schema_migrations') IS NOT NULL").Scan(&exists)
	}
	if err != nil {
		return err
	}
	if !exists {
		var count int
		if d.Driver == "sqlite" {
			err = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&count)
		} else {
			err = conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'").Scan(&count)
		}
		if err != nil {
			return err
		}
		if count != 0 {
			return errors.New("unmanaged non-empty database; import legacy data into a new database instead")
		}
	}
	if _, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS cyapi_schema_migrations (name TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)"); err != nil {
		return err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return err
	}
	check := map[string]string{}
	for k, v := range done {
		check[k] = v
	}
	if err = verify(ms, check, false); err != nil {
		return err
	}
	for _, m := range ms {
		if _, ok := done[m.Name]; ok {
			continue
		}
		if _, err = conn.ExecContext(ctx, m.Body); err != nil {
			return fmt.Errorf("migration %s failed: %w", m.Name, err)
		}
		stmt := "INSERT INTO cyapi_schema_migrations(name,checksum,applied_at) VALUES (?,?,?)"
		if d.Driver == "postgres" {
			stmt = "INSERT INTO cyapi_schema_migrations(name,checksum,applied_at) VALUES ($1,$2,$3)"
		}
		if _, err = conn.ExecContext(ctx, stmt, m.Name, m.Checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
