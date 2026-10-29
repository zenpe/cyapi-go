package main

import (
	"context"
	"cyapi-go/internal/backup"
	"cyapi-go/internal/config"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/sources"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

type taskResult struct {
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success"`
	Error       string    `json:"error,omitempty"`
	Output      string    `json:"output,omitempty"`
}
type maintenanceState struct {
	Format int        `json:"format"`
	Sync   taskResult `json:"sync"`
	Backup taskResult `json:"backup"`
}

func startMaintenance(ctx context.Context, c config.Config, configPath string, db *persistence.Database, manager *sources.Manager, log *slog.Logger) func() {
	if !c.Maintenance.Enabled {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		statePath := filepath.Join(c.RuntimeDirectory, "maintenance.json")
		state := maintenanceState{Format: 1}
		if b, e := os.ReadFile(statePath); e == nil {
			if json.Unmarshal(b, &state) != nil || state.Format != 1 {
				log.Warn("invalid maintenance state; tasks will be retried")
				state = maintenanceState{Format: 1}
			}
		}
		save := func() {
			if e := writeJSONFile(statePath, state); e != nil {
				log.Warn("cannot persist maintenance state")
			}
		}
		runSync := func() {
			state.Sync.LastAttempt = time.Now().UTC()
			save()
			work, cancel := context.WithTimeout(ctx, c.Sources.Timeout)
			defer cancel()
			report, e := manager.Sync(work, false, false)
			if e != nil {
				state.Sync.Error = "source synchronization failed; inspect network, disk and sources status"
				log.Warn("scheduled source synchronization failed", "error", e.Error())
			} else {
				state.Sync.Error = ""
				state.Sync.LastSuccess = time.Now().UTC()
				state.Sync.Output = report.Generation
				log.Info("scheduled source synchronization complete", "generation", report.Generation, "files", report.Files)
			}
			save()
		}
		runBackup := func() {
			state.Backup.LastAttempt = time.Now().UTC()
			save()
			work, cancel := context.WithTimeout(ctx, 30*time.Minute)
			defer cancel()
			output := filepath.Join(c.Maintenance.BackupDirectory, "auto-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
			_, e := backup.Run(work, db, backup.Options{Output: output, DSN: c.Database.DSN, Version: version, Commit: commit, SourcesDirectory: c.Sources.Directory, Manager: manager, IncludeConfig: true, ConfigPath: configPath, SecretsPath: c.SecretsFile})
			if e != nil {
				state.Backup.Error = "backup failed; inspect database, sources and available disk space"
				log.Warn("scheduled backup failed", "error", e.Error())
			} else {
				state.Backup.Error = ""
				state.Backup.LastSuccess = time.Now().UTC()
				state.Backup.Output = output
				log.Info("scheduled backup complete", "output", output)
				if e = pruneBackups(c.Maintenance.BackupDirectory, c.Maintenance.BackupKeep); e != nil {
					log.Warn("backup retention skipped", "error", e.Error())
				}
			}
			save()
		}
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if taskDue(state.Sync, c.Maintenance.SyncInterval, time.Now()) {
				runSync()
			}
			if ctx.Err() != nil {
				return
			}
			if taskDue(state.Backup, c.Maintenance.BackupInterval, time.Now()) {
				runBackup()
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() { <-done }
}
func taskDue(s taskResult, interval time.Duration, now time.Time) bool {
	if s.LastAttempt.After(s.LastSuccess) && now.Sub(s.LastAttempt) < 5*time.Minute {
		return false
	}
	return s.LastSuccess.IsZero() || now.Sub(s.LastSuccess) >= interval
}

var automaticBackupName = regexp.MustCompile(`^auto-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z$`)

// Only completed, program-named backups are eligible. Manual and upgrade backups are retained.
func pruneBackups(directory string, keep int) error {
	if keep < 1 {
		return errors.New("invalid retention")
	}
	root, e := os.OpenRoot(directory)
	if e != nil {
		return e
	}
	defer root.Close()
	f, e := root.Open(".")
	if e != nil {
		return e
	}
	entries, e := f.ReadDir(-1)
	f.Close()
	if e != nil {
		return e
	}
	var names []string
	for _, entry := range entries {
		if !automaticBackupName.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		b, e := root.ReadFile(entry.Name() + "/manifest.json")
		if e != nil {
			continue
		}
		var m backup.Manifest
		if json.Unmarshal(b, &m) != nil || m.Format != 1 || m.DatabaseSHA256 == "" || m.Database == "" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, name := range names {
		if i < keep {
			continue
		}
		tree, e := root.OpenRoot(name)
		if e != nil {
			return e
		}
		e = fs.WalkDir(tree.FS(), ".", func(_ string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("backup cleanup refuses symbolic links")
			}
			return nil
		})
		tree.Close()
		if e != nil {
			return e
		}
		if e = root.RemoveAll(name); e != nil {
			return e
		}
	}
	return nil
}
