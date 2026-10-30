package main

import (
	"cyapi-go/internal/config"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func configEditCommand(args []string) error {
	if len(args) == 0 || args[0] != "edit" {
		return errors.New("usage: cyapi config edit [--config config.yaml]")
	}
	f := flag.NewFlagSet("config edit", flag.ContinueOnError)
	path := f.String("config", "config.yaml", "configuration file")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected config edit arguments")
	}
	if e := rejectManagedOverrides(); e != nil {
		return e
	}
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	var i installation
	managed := false
	if _, e = os.Lstat(filepath.Join(filepath.Dir(c.FilePath), ".cyapi-install.json")); e == nil {
		if !isRoot() {
			return errors.New("managed configuration editing requires sudo")
		}
		i, e = loadManagedInstallation(c)
		if e != nil {
			return e
		}
		managed = true
	}
	if _, e = os.Stat(filepath.Join(c.RuntimeDirectory, "install.json")); e == nil && !managed {
		if old, e := loadInstallation(c); e == nil && old.Unit != "" {
			return errors.New("upgrade the legacy managed installation before using config edit")
		}
	}
	lockPath := filepath.Join(c.RuntimeDirectory, "management.lock")
	if managed {
		lockPath = filepath.Join(i.Root, ".cyapi-management.lock")
	}
	unlock, e := acquireLock(lockPath)
	if e != nil {
		return e
	}
	defer unlock()
	original, e := os.ReadFile(c.FilePath)
	if e != nil {
		return e
	}
	temp, e := os.CreateTemp(filepath.Dir(c.FilePath), ".cyapi-config-*.yaml")
	if e != nil {
		return e
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, e = temp.Write(original); e != nil {
		temp.Close()
		return e
	}
	if e = temp.Close(); e != nil {
		return e
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	// Shell expansion is deliberately not used; EDITOR is an executable plus simple arguments.
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return errors.New("EDITOR is empty")
	}
	cmd := exec.Command(parts[0], append(parts[1:], tempPath)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		return fmt.Errorf("editor failed; original config retained: %w", e)
	}
	next, e := config.Load(tempPath)
	if e != nil {
		return fmt.Errorf("invalid edited config; original retained: %w", e)
	}
	next.FilePath = c.FilePath
	if managed {
		if e = validateManagedPaths(i, next); e != nil {
			return e
		}
		// Moving existing data requires an explicit migration, not just editing its path.
		if next.RuntimeDirectory != c.RuntimeDirectory || next.Database.DSN != c.Database.DSN || next.Database.Driver != c.Database.Driver || next.Sources.Directory != c.Sources.Directory || next.SecretsFile != c.SecretsFile || next.Maintenance.BackupDirectory != c.Maintenance.BackupDirectory {
			return errors.New("data/secret path changes require explicit migration; original config retained")
		}
	}
	if next.Server.Listen != c.Server.Listen {
		if e = checkPort(next.Server.Listen); e != nil {
			return e
		}
	}
	edited, e := os.ReadFile(tempPath)
	if e != nil {
		return e
	}
	if e = writeAtomic(c.FilePath, edited, 0600); e != nil {
		return e
	}
	if managed {
		if e = securePath(c.FilePath, i.User, 0600); e != nil {
			return e
		}
		if e = checkAsService(i, false); e != nil {
			return e
		}
		fmt.Println("Configuration saved with service-user permissions. Apply with: sudo systemctl restart", i.Unit)
	} else {
		fmt.Println("Configuration saved. Restart the API to apply changes.")
	}
	return nil
}
