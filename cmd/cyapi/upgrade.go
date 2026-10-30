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
	if flags.NArg() != 0 || *file == "" {
		return errors.New("usage: cyapi upgrade --file NEW_BINARY [--sha256 HASH] [--config config.yaml]")
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
		return errors.New("no managed systemd installation; use the manual upgrade procedure")
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
	i.Version = candidateVersion
	return executeUpgrade(i, c, staged, digest, log, upgradeOps{
		service: systemctl,
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
}

type upgradeOps struct {
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
	if ops.prepare != nil {
		if e = ops.prepare(); e != nil {
			return fmt.Errorf("service stopped; permission preparation failed: %w", e)
		}
	}
	release, e := acquireLock(filepath.Join(c.RuntimeDirectory, "serve.lock"))
	if e != nil {
		return errors.New("service stopped but another API owns the instance; upgrade aborted")
	}
	release()
	if i.User != "" {
		if e = securePath(filepath.Join(c.RuntimeDirectory, "serve.lock"), i.User, 0600); e != nil {
			return e
		}
	}
	output := filepath.Join(c.Maintenance.BackupDirectory, "upgrade-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if e = ops.step(i.Binary, 30*time.Minute, "backup", "--config", i.Config, "--output", output, "--include-config"); e != nil {
		restartErr := ops.service("start", i.Unit)
		if restartErr != nil {
			log.Error("old service restart failed after backup failure")
		}
		return errors.New("upgrade backup failed; no migration or binary replacement was attempted")
	}
	fmt.Println("Recovery backup:", output)
	if e = writeJSONFile(filepath.Join(i.Root, ".cyapi-upgrade.json"), map[string]any{"stage": "backed_up", "backup": output, "candidate_sha256": digest, "time": time.Now().UTC()}); e != nil {
		return e
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
