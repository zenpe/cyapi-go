// Package backup publishes checksummed database and immutable-source backups.
package backup

import (
	"context"
	"crypto/sha256"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/sources"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Manifest struct {
	Format                             int `json:"format"`
	CreatedAt, Driver, Version, Commit string
	Database                           string
	DatabaseSHA256                     string
	DatabaseBytes                      int64
	SourcesGeneration                  string
	SourceFiles                        int
	RecoveryFiles                      map[string]string `json:",omitempty"`
	ConfigIncluded                     bool
}
type Options struct {
	Output, DSN, Version, Commit, SourcesDirectory string
	IncludeConfig                                  bool
	ConfigPath, SecretsPath                        string
	CredentialKeyPath                              string
	Manager                                        *sources.Manager
}

func Run(ctx context.Context, db *persistence.Database, o Options) (Manifest, error) {
	var manifest Manifest
	if o.Output == "" {
		return manifest, errors.New("backup requires --output directory")
	}
	target, e := filepath.Abs(o.Output)
	if e != nil {
		return manifest, e
	}
	if _, e = os.Lstat(target); !errors.Is(e, os.ErrNotExist) {
		return manifest, errors.New("backup output already exists or cannot be inspected")
	}
	parent := filepath.Dir(target)
	if e = os.MkdirAll(parent, 0700); e != nil {
		return manifest, e
	}
	stage, e := os.MkdirTemp(parent, ".cyapi-backup-")
	if e != nil {
		return manifest, e
	}
	if filepath.Dir(stage) != parent {
		return manifest, errors.New("invalid backup staging path")
	}
	defer os.RemoveAll(stage)
	manifest = Manifest{Format: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Driver: db.Driver, Version: o.Version, Commit: o.Commit, Database: "database.sqlite"}
	if db.Driver == "postgres" {
		manifest.Database = "database.dump"
	}
	file := filepath.Join(stage, manifest.Database)
	if db.Driver == "sqlite" {
		if _, e = db.SQL.ExecContext(ctx, "VACUUM INTO ?", file); e != nil {
			return Manifest{}, errors.New("SQLite consistent backup failed")
		}
		check, err := sql.Open("sqlite", filepath.ToSlash(file)+"?mode=ro")
		if err != nil {
			return Manifest{}, err
		}
		var integrity string
		err = check.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity)
		check.Close()
		if err != nil || integrity != "ok" {
			return Manifest{}, errors.New("backup integrity check failed")
		}
	} else if e = dumpPostgres(ctx, o.DSN, file); e != nil {
		return Manifest{}, e
	}
	if e = os.Chmod(file, 0600); e != nil {
		return Manifest{}, e
	}
	digest, size, e := hashFile(ctx, file)
	if e != nil {
		return Manifest{}, e
	}
	manifest.DatabaseSHA256 = digest
	manifest.DatabaseBytes = size
	if o.Manager != nil {
		_, statErr := os.Stat(filepath.Join(o.SourcesDirectory, "current.json"))
		if statErr == nil {
			snapshot, err := o.Manager.Current()
			if err != nil {
				return Manifest{}, errors.New("source snapshot unavailable; backup not published")
			}
			defer snapshot.Close()
			dest := filepath.Join(stage, "sources", "versions", snapshot.Manifest.Generation)
			if err = os.MkdirAll(filepath.Join(dest, "files"), 0700); err != nil {
				return Manifest{}, err
			}
			for _, entry := range snapshot.Manifest.Files {
				if err = ctx.Err(); err != nil {
					return Manifest{}, err
				}
				source, err := snapshot.Open(entry)
				if err != nil {
					return Manifest{}, err
				}
				target := filepath.Join(dest, "files", filepath.FromSlash(entry.Path))
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					source.Close()
					return Manifest{}, err
				}
				err = copySource(ctx, source, target, entry.Size, entry.SHA256)
				source.Close()
				if err != nil {
					return Manifest{}, err
				}
			}
			if err = writeJSON(filepath.Join(dest, "manifest.json"), snapshot.Manifest); err != nil {
				return Manifest{}, err
			}
			if err = os.WriteFile(filepath.Join(dest, ".read.lock"), nil, 0600); err != nil {
				return Manifest{}, err
			}
			if err = writeJSON(filepath.Join(stage, "sources", "current.json"), map[string]any{"format": sources.Format, "generation": snapshot.Manifest.Generation}); err != nil {
				return Manifest{}, err
			}
			manifest.SourcesGeneration = snapshot.Manifest.Generation
			manifest.SourceFiles = len(snapshot.Manifest.Files)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Manifest{}, errors.New("source directory cannot be inspected")
		}
	}

	// The credential key is recovery material even for database-only backups.
	keyPath := o.CredentialKeyPath
	if keyPath == "" && o.ConfigPath != "" {
		keyPath = filepath.Join(filepath.Dir(o.ConfigPath), "data", "netease.key")
	}
	var credentialCount int64
	if db.ORM.Migrator().HasColumn("account_bindings", "credentials") {
		if err := db.ORM.Table("account_bindings").Where("credentials <> ?", "").Count(&credentialCount).Error; err != nil {
			return Manifest{}, errors.New("cannot inspect account credentials for backup")
		}
	}
	if credentialCount > 0 && keyPath == "" {
		return Manifest{}, errors.New("credential key path required to back up account bindings")
	}
	if keyPath != "" {
		if info, err := os.Lstat(keyPath); err == nil {
			if !info.Mode().IsRegular() || info.Size() != 32 {
				return Manifest{}, errors.New("invalid netease key; backup not published")
			}
			digest, size, err := hashFile(ctx, keyPath)
			if err != nil {
				return Manifest{}, err
			}
			f, err := os.Open(keyPath)
			if err != nil {
				return Manifest{}, err
			}
			err = copySource(ctx, f, filepath.Join(stage, "netease.key"), size, digest)
			f.Close()
			if err != nil {
				return Manifest{}, err
			}
			manifest.RecoveryFiles = map[string]string{"netease.key": digest}
		} else if !errors.Is(err, os.ErrNotExist) || credentialCount > 0 {
			return Manifest{}, errors.New("account credential key missing; backup not published")
		}
	}
	if o.IncludeConfig {
		if manifest.RecoveryFiles == nil {
			manifest.RecoveryFiles = map[string]string{}
		}
		for name, source := range map[string]string{"config.yaml": o.ConfigPath, "secrets.env": o.SecretsPath} {
			if source == "" {
				continue
			}
			info, err := os.Lstat(source)
			if errors.Is(err, os.ErrNotExist) && name == "secrets.env" {
				continue
			}
			if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				return Manifest{}, errors.New("recovery file missing, linked or oversized")
			}
			digest, size, err := hashFile(ctx, source)
			if err != nil {
				return Manifest{}, err
			}
			f, err := os.Open(source)
			if err != nil {
				return Manifest{}, err
			}
			err = copySource(ctx, f, filepath.Join(stage, name), size, digest)
			f.Close()
			if err != nil {
				return Manifest{}, err
			}
			manifest.RecoveryFiles[name] = digest
		}
		manifest.ConfigIncluded = true
	}
	if e = writeJSON(filepath.Join(stage, "manifest.json"), manifest); e != nil {
		return Manifest{}, e
	}
	if e = ctx.Err(); e != nil {
		return Manifest{}, e
	}
	if _, e = os.Lstat(target); !errors.Is(e, os.ErrNotExist) {
		return Manifest{}, errors.New("backup output already exists")
	}
	if e = os.Rename(stage, target); e != nil {
		return Manifest{}, e
	}
	return manifest, nil
}

type reader struct {
	ctx context.Context
	r   io.Reader
}

func (r reader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
func hashFile(ctx context.Context, path string) (string, int64, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, reader{ctx, f})
	return hex.EncodeToString(h.Sum(nil)), n, e
}
func copySource(ctx context.Context, source io.Reader, target string, size int64, expected string) error {
	f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(reader{ctx, source}, size+1))
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("source checksum mismatch; backup not published")
	}
	return nil
}
func writeJSON(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(append(b, '\n'))
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func dumpPostgres(ctx context.Context, dsn, file string) error {
	u, e := url.Parse(dsn)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
		return errors.New("PostgreSQL backup requires a postgres:// DSN")
	}
	tool, e := exec.LookPath("pg_dump")
	if e != nil {
		return errors.New("PostgreSQL backup requires matching pg_dump in PATH")
	}
	values := map[string]string{"PGHOST": u.Hostname(), "PGPORT": u.Port(), "PGDATABASE": strings.TrimPrefix(u.Path, "/"), "PGCONNECT_TIMEOUT": "10"}
	if values["PGPORT"] == "" {
		values["PGPORT"] = "5432"
	}
	if u.User != nil {
		values["PGUSER"] = u.User.Username()
		if password, ok := u.User.Password(); ok {
			values["PGPASSWORD"] = password
		}
	}
	for key, value := range u.Query() {
		switch key {
		case "sslmode":
			values["PGSSLMODE"] = value[0]
		case "sslrootcert":
			values["PGSSLROOTCERT"] = value[0]
		case "sslcert":
			values["PGSSLCERT"] = value[0]
		case "sslkey":
			values["PGSSLKEY"] = value[0]
		case "search_path":
			values["PGOPTIONS"] = "-c search_path=" + value[0]
		default:
			return errors.New("unsupported PostgreSQL backup DSN option")
		}
	}
	cmd := exec.CommandContext(ctx, tool, "--format=custom", "--no-owner", "--no-acl", "--file", file)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "PG") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if cmd.Run() != nil {
		return errors.New("PostgreSQL backup failed; check server and pg_dump compatibility")
	}
	return nil
}
