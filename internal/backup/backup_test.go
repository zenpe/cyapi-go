package backup

import (
	"context"
	"crypto/sha256"
	"cyapi-go/internal/config"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/sources"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteBackupRestoresCommittedData(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, e := persistence.Open(ctx, config.Database{Driver: "sqlite", DSN: filepath.Join(root, "live.db"), BusyTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = db.ORM.Exec("INSERT INTO users(email,username,password_hash) VALUES ('123456@qq.com','backup','unused')").Error; e != nil {
		t.Fatal(e)
	}
	sourceRoot := filepath.Join(root, "sources")
	generation := "g-00000000000000000001-0123456789abcdef"
	versionRoot := filepath.Join(sourceRoot, "versions", generation)
	if e = os.MkdirAll(filepath.Join(versionRoot, "files"), 0700); e != nil {
		t.Fatal(e)
	}
	script := []byte("// source fixture\n")
	digest := sha256.Sum256(script)
	hash := hex.EncodeToString(digest[:])
	os.WriteFile(filepath.Join(versionRoot, "files", "fixture.js"), script, 0600)
	os.WriteFile(filepath.Join(versionRoot, ".read.lock"), nil, 0600)
	sourceManifest := sources.Manifest{Format: sources.Format, Generation: generation, Repo: "guoyue2010/lxmusic-", ConfigSHA256: hash, ArchiveSHA256: hash, Files: []sources.File{{Path: "fixture.js", Size: int64(len(script)), SHA256: hash}}}
	if e = writeJSON(filepath.Join(versionRoot, "manifest.json"), sourceManifest); e != nil {
		t.Fatal(e)
	}
	if e = writeJSON(filepath.Join(sourceRoot, "current.json"), map[string]any{"format": sources.Format, "generation": generation}); e != nil {
		t.Fatal(e)
	}
	manager, e := sources.New(sources.DefaultOptions(sourceRoot))
	if e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(root, "backup")
	m, e := Run(ctx, db, Options{Output: out, Version: "test", Commit: "test", Manager: manager, SourcesDirectory: sourceRoot})
	if e == nil {
		restoredManager, err := sources.New(sources.DefaultOptions(filepath.Join(out, "sources")))
		if err != nil {
			t.Fatal(err)
		}
		snap, err := restoredManager.Current()
		if err != nil {
			t.Fatal(err)
		}
		err = snap.Verify(ctx)
		snap.Close()
		if err != nil {
			t.Fatal(err)
		}
		if m.SourceFiles != 1 || m.SourcesGeneration != generation {
			t.Fatal(m)
		}
	}
	if e != nil {
		t.Fatal(e)
	}
	if m.DatabaseBytes <= 0 || len(m.DatabaseSHA256) != 64 || m.Driver != "sqlite" {
		t.Fatal(m)
	}
	if _, e = Run(ctx, db, Options{Output: out}); e == nil {
		t.Fatal("overwrote backup")
	}
	restored, e := persistence.Open(ctx, config.Database{Driver: "sqlite", DSN: filepath.Join(out, m.Database), BusyTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if e = restored.CheckSchema(ctx); e != nil {
		t.Fatal(e)
	}
	var n int64
	if e = restored.ORM.Table("users").Count(&n).Error; e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = os.Stat(filepath.Join(out, "manifest.json")); e != nil {
		t.Fatal(e)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	target := filepath.Join(root, "cancelled")
	if _, e = Run(cancelled, db, Options{Output: target}); e == nil {
		t.Fatal("cancelled backup published")
	}
	if _, e = os.Stat(target); !os.IsNotExist(e) {
		t.Fatal("failed output visible", e)
	}
}
