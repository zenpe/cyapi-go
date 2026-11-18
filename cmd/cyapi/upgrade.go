package main

import (
	"context"
	"crypto/sha256"
	"cyapi-go/internal/config"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func upgradeCommand(args []string, log *slog.Logger) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	file := flags.String("file", "", "trusted new binary")
	expected := flags.String("sha256", "", "expected SHA256 from release")
	path := flags.String("config", "config.yaml", "installed configuration")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return errors.New("usage: cyapi upgrade [--file NEW_BINARY --sha256 HASH] [--config config.yaml]")
	}
	if *file == "" && *expected != "" {
		return errors.New("--sha256 requires --file; automatic upgrades verify SHA256SUMS themselves")
	}
	if targetOS() != "linux" || !systemdAvailable() || !isRoot() {
		return errors.New("managed upgrade requires Linux systemd and root; see manual upgrade/recovery documentation")
	}
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	i, e := loadUpgradeInstallation(c)
	if e != nil || i.Unit == "" {
		return fmt.Errorf("cannot load managed systemd installation: %w", e)
	}
	absolute, e := filepath.Abs(*path)
	if e != nil || absolute != i.Config {
		return errors.New("upgrade must use the installed configuration")
	}
	unlock, e := acquireLock(filepath.Join(i.Root, ".cyapi-management.lock"))
	if e != nil {
		return e
	}
	defer unlock()
	onlineTag := ""
	if *file == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		command, err := serviceCommand(ctx, i.User, i.Binary, "version")
		if err != nil {
			return err
		}
		installedBytes, err := command.Output()
		if err != nil {
			return fmt.Errorf("cannot read installed version: %w", err)
		}
		fields := strings.Fields(string(installedBytes))
		if len(fields) < 2 || fields[0] != "cyapi" {
			return errors.New("invalid installed version output")
		}
		fmt.Println("Checking latest stable release from GitHub...")
		downloaded, hash, tag, err := newReleaseUpdater(c.Env(c.Sources.GitHubTokenEnv)).downloadLatest(ctx, i.Root, fields[1], targetArch())
		if err != nil {
			return fmt.Errorf("automatic update failed before stopping service: %w; retry or use upgrade --file NEW_BINARY", err)
		}
		if downloaded == "" {
			return nil
		}
		defer os.Remove(downloaded)
		*file = downloaded
		*expected = hash
		onlineTag = tag
	}
	staged := filepath.Join(i.Root, ".cyapi-upgrade-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if e = copyExecutable(*file, staged); e != nil {
		return e
	}
	defer os.Remove(staged)
	f, e := os.Open(staged)
	if e != nil {
		return e
	}
	hash := sha256.New()
	_, e = io.Copy(hash, f)
	f.Close()
	if e != nil {
		return e
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if *expected != "" && !strings.EqualFold(*expected, digest) {
		return errors.New("candidate SHA256 does not match; service was not stopped")
	}
	fmt.Println("Candidate SHA256:", digest)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	versionCommand, e := serviceCommand(ctx, i.User, staged, "version")
	if e != nil {
		cancel()
		return e
	}
	versionBytes, e := versionCommand.Output()
	cancel()
	if e != nil || !strings.HasPrefix(string(versionBytes), "cyapi ") || !strings.HasSuffix(strings.TrimSpace(string(versionBytes)), "linux/"+targetArch()) {
		return errors.New("candidate version/architecture check failed; service was not stopped")
	}
	candidateVersion := strings.Fields(string(versionBytes))[1]
	if onlineTag != "" && candidateVersion != onlineTag {
		return errors.New("downloaded binary version differs from release tag; service was not stopped")
	}
	old := i
	err := executeUpgrade(i, c, staged, digest, log, upgradeOps{
		service: systemctl,
		waitOld: func(listen string, timeout time.Duration) error {
			return waitManagedReady(old, listen, old.Version, timeout)
		},
		prepare: func() error {
			if e := prepareManagedPaths(i, c); e != nil {
				return e
			}
			if e := writeManagedInstallation(i); e != nil {
				return e
			}
			if e := registerService(i); e != nil {
				return e
			}
			candidate := i
			candidate.Binary = staged
			return checkAsService(candidate, true)
		},
		step: func(binary string, timeout time.Duration, args ...string) error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd, e := serviceCommand(ctx, i.User, binary, args...)
			if e != nil {
				return e
			}
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		},
		wait: func(listen string, timeout time.Duration) error {
			return waitManagedReady(i, listen, candidateVersion, timeout)
		},
	})
	if err != nil {
		return err
	}
	i.Version = candidateVersion
	if err := writeManagedInstallation(i); err != nil {
		return fmt.Errorf("new service ready; cannot update installation version: %w", err)
	}
	return nil
}

type upgradeOps struct {
	waitOld func(string, time.Duration) error
	prepare func() error
	service func(...string) error
	step    func(string, time.Duration, ...string) error
	wait    func(string, time.Duration) error
}

func executeUpgrade(i installation, c config.Config, staged, digest string, log *slog.Logger, ops upgradeOps) error {
	var e error
	if e = ops.service("stop", i.Unit); e != nil {
		return e
	}
	lockPath := filepath.Join(c.RuntimeDirectory, "serve.lock")
	restore := func(cause error) error {
		release, err := acquireLock(lockPath)
		if err != nil {
			return fmt.Errorf("%w; cannot confirm instance ownership, old service restart skipped: %v", cause, err)
		}
		release()
		if i.User != "" {
			if err = securePath(lockPath, i.User, 0600); err != nil {
				return fmt.Errorf("%w; service stopped, cannot restore lock ownership: %v", cause, err)
			}
		}
		if fi, err := os.Stat(i.Binary); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%w; old binary unavailable, service stopped", cause)
		}
		if err := ops.service("start", i.Unit); err != nil {
			return fmt.Errorf("%w; old service restart failed: %v", cause, err)
		}
		wait := ops.waitOld
		if wait == nil {
			wait = ops.wait
		}
		if err := wait(c.Server.Listen, 20*time.Second); err != nil {
			return fmt.Errorf("%w; old service started but readiness unconfirmed: %v", cause, err)
		}
		return fmt.Errorf("%w; old service restored and ready", cause)
	}
	if ops.prepare != nil {
		if e = ops.prepare(); e != nil {
			return restore(fmt.Errorf("upgrade preparation failed: %w", e))
		}
	}
	release, e := acquireLock(filepath.Join(c.RuntimeDirectory, "serve.lock"))
	if e != nil {
		return errors.New("service stopped but another API owns the instance; upgrade aborted")
	}
	release()
	if i.User != "" {
		if e = securePath(lockPath, i.User, 0600); e != nil {
			return restore(fmt.Errorf("cannot prepare instance lock: %w", e))
		}
	}
	output := filepath.Join(c.Maintenance.BackupDirectory, "upgrade-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if e = ops.step(i.Binary, 30*time.Minute, "backup", "--config", i.Config, "--output", output, "--include-config"); e != nil {
		return restore(fmt.Errorf("upgrade backup failed before migration: %w", e))
	}
	fmt.Println("Recovery backup:", output)
	if e = writeJSONFile(filepath.Join(i.Root, ".cyapi-upgrade.json"), map[string]any{"stage": "backed_up", "backup": output, "candidate_sha256": digest, "time": time.Now().UTC()}); e != nil {
		return restore(fmt.Errorf("recording upgrade state failed; recovery backup %s: %w", output, e))
	}
	// Once migration starts, never automatically restart an older program against the resulting schema.
	if e = ops.step(staged, 2*time.Minute, "migrate", "--config", i.Config); e != nil {
		return fmt.Errorf("migration failed; service left stopped, old binary retained; inspect recovery backup %s", output)
	}
	previous := i.Binary + ".previous-" + time.Now().UTC().Format("20060102T150405.000000000")
	if _, e = os.Lstat(previous); e == nil {
		return fmt.Errorf("migration completed but previous binary already exists at %s; service remains stopped; preserve/remove it explicitly before switching", previous)
	}
	if e = copyExecutable(i.Binary, previous); e != nil {
		return fmt.Errorf("cannot preserve previous binary; service left stopped; recovery backup: %s", output)
	}
	if e = os.Rename(staged, i.Binary); e != nil {
		return fmt.Errorf("binary replacement failed; service left stopped; recovery backup: %s", output)
	}
	if e = writeJSONFile(filepath.Join(i.Root, ".cyapi-upgrade.json"), map[string]any{"stage": "switched", "backup": output, "candidate_sha256": digest, "previous": previous, "time": time.Now().UTC()}); e != nil {
		return e
	}
	if e = ops.service("start", i.Unit); e == nil {
		e = ops.wait(c.Server.Listen, 20*time.Second)
	}
	if e != nil {
		if stopErr := ops.service("stop", i.Unit); stopErr != nil {
			return fmt.Errorf("new service failed readiness AND could not be stopped; stop it manually before recovery from %s", output)
		}
		return fmt.Errorf("new service failed readiness and was stopped; do not restart the old binary against a migrated schema; restore recovery backup %s", output)
	}
	if e = writeJSONFile(filepath.Join(i.Root, ".cyapi-upgrade.json"), map[string]any{"stage": "ready", "backup": output, "candidate_sha256": digest, "previous": previous, "time": time.Now().UTC()}); e != nil {
		log.Warn("upgrade succeeded but cannot persist final upgrade status")
	}
	fmt.Println("Upgrade complete. API ready. Previous binary:", previous)
	log.Info("upgrade completed", "backup", output, "sha256", digest)
	return nil
}
