package main

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/backup"
	"cyapi-go/internal/config"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/sources"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func TestHelpWithoutConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"--version"}, {"help", "install"}, {"install", "--help"}, {"backup", "--help"}, {"sources", "rollback", "--help"}} {
		if e := run(args, quietLog()); e != nil {
			t.Fatalf("%v: %v", args, e)
		}
	}
	if e := run([]string{"no-such-command"}, quietLog()); e == nil {
		t.Fatal("unknown command accepted")
	}
}
func TestInstallIdempotentAndFirstLogin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CYAPI_INSTALL_PASSWORD", "InstallTest!123")
	args := []string{"--dir", root, "--service", "none", "--non-interactive", "--username", "first_user", "--email", "100000001@qq.com", "--listen", "127.0.0.1:0"}
	if e := installCommand(args, quietLog()); e != nil {
		t.Fatal(e)
	}
	configPath := filepath.Join(root, "config.yaml")
	originalConfig, _ := os.ReadFile(configPath)
	originalSecret, _ := os.ReadFile(filepath.Join(root, "secrets.env"))
	if e := installCommand(args, quietLog()); e != nil {
		t.Fatal(e)
	}
	afterConfig, _ := os.ReadFile(configPath)
	afterSecret, _ := os.ReadFile(filepath.Join(root, "secrets.env"))
	if string(originalConfig) != string(afterConfig) || string(originalSecret) != string(afterSecret) {
		t.Fatal("repeat install changed configuration or secret")
	}
	c, e := config.Load(configPath)
	if e != nil {
		t.Fatal(e)
	}
	db, e := persistence.Open(context.Background(), c.Database)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	secret, _ := c.Secret()
	accounts, e := auth.New(authstore.New(db.ORM), nil, auth.Options{Secret: secret, TokenMaxAge: time.Hour, BcryptCost: 4, Logger: quietLog()})
	if e != nil {
		t.Fatal(e)
	}
	user, token, e := accounts.Login(context.Background(), "first_user", "InstallTest!123", "127.0.0.1")
	if e != nil || token == "" || user.Username != "first_user" {
		t.Fatalf("bootstrap login failed: %v", e)
	}
	var count int64
	db.ORM.Table("users").Count(&count)
	if count != 1 {
		t.Fatal("duplicate bootstrap users")
	}
	output := filepath.Join(root, "backups", "manual-test")
	_, e = backup.Run(context.Background(), db, backup.Options{Output: output, DSN: c.Database.DSN, IncludeConfig: true, ConfigPath: configPath, SecretsPath: c.SecretsFile})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(output, "manifest.json"))
	var m backup.Manifest
	if json.Unmarshal(b, &m) != nil || !m.ConfigIncluded || len(m.RecoveryFiles) != 2 {
		t.Fatal("recovery metadata absent")
	}
	secretCopy, _ := os.ReadFile(filepath.Join(output, "secrets.env"))
	if string(secretCopy) != string(originalSecret) {
		t.Fatal("recovery secret changed")
	}
	// Existing credentials cannot silently be replaced after a missing key.
	db.Close()
	if e = os.Remove(filepath.Join(root, "secrets.env")); e != nil {
		t.Fatal(e)
	}
	if e = installCommand(args, quietLog()); e == nil || !strings.Contains(e.Error(), "existing database") {
		t.Fatalf("missing-key protection: %v", e)
	}
	if _, e = os.Stat(filepath.Join(root, "secrets.env")); !os.IsNotExist(e) {
		t.Fatal("regenerated key for existing database")
	}
}
func TestDoctorDoesNotCreateDatabase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if e := os.WriteFile(path, []byte("{}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := inspectCommand([]string{"--config", path}, true); e == nil {
		t.Fatal("missing configuration passed doctor")
	}
	if _, e := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(e) {
		t.Fatal("doctor created state directory/database")
	}
}
func TestInstanceLockReleased(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serve.lock")
	release, e := acquireLock(p)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := acquireLock(p); e == nil {
		r()
		t.Fatal("second instance acquired lock")
	}
	release()
	release()
	next, e := acquireLock(p)
	if e != nil {
		t.Fatal(e)
	}
	next()
}
func TestBackupRetentionPreservesManualAndIncomplete(t *testing.T) {
	root := t.TempDir()
	names := []string{"auto-20261001T000000.000000000Z", "auto-20261002T000000.000000000Z", "auto-20261003T000000.000000000Z", "manual-20261001", "auto-20261004T000000.000000000Z"}
	for i, name := range names {
		dir := filepath.Join(root, name)
		os.Mkdir(dir, 0700)
		if i != 4 {
			b, _ := json.Marshal(backup.Manifest{Format: 1, Database: "database.sqlite", DatabaseSHA256: "example"})
			os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600)
		}
	}
	if e := pruneBackups(root, 2); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(root, names[0])); !os.IsNotExist(e) {
		t.Fatal("old managed backup retained")
	}
	for _, name := range names[1:] {
		if _, e := os.Stat(filepath.Join(root, name)); e != nil {
			t.Fatal("removed retained/manual/incomplete backup:", name)
		}
	}
}
func TestTaskSchedulePersistsSuccessAndRetries(t *testing.T) {
	now := time.Now()
	if !taskDue(taskResult{}, time.Hour, now) {
		t.Fatal("first task not due")
	}
	if taskDue(taskResult{LastAttempt: now, LastSuccess: now}, time.Hour, now.Add(10*time.Minute)) {
		t.Fatal("successful task repeated too early")
	}
	if taskDue(taskResult{LastAttempt: now}, time.Hour, now.Add(time.Minute)) {
		t.Fatal("failed task retried too early")
	}
	if !taskDue(taskResult{LastAttempt: now}, time.Hour, now.Add(6*time.Minute)) {
		t.Fatal("failed task not retried")
	}
}
func TestRotatingLogBounded(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "server.log")
	os.WriteFile(path, make([]byte, 10<<20), 0600)
	logger, e := newRotatingLog(path)
	if e != nil {
		t.Fatal(e)
	}
	defer logger.Close()
	if _, e = logger.Write([]byte("new log\n")); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(path); string(b) != "new log\n" {
		t.Fatal("active log was not rotated")
	}
	if i, e := os.Stat(path + "1"); e != nil || i.Size() != 10<<20 {
		t.Fatal("previous log missing")
	}
}
func TestSystemdQuoting(t *testing.T) {
	unit := serviceUnit(installation{Root: "/vol1/a b%", Binary: "/vol1/a b%/cyapi", Config: "/vol1/a b%/config.yaml", User: "cyapi"})
	if !strings.Contains(unit, `"/vol1/a b%%/cyapi"`) || !strings.Contains(unit, "User=cyapi") {
		t.Fatal(unit)
	}
}

func TestUpgradeFailureBoundaries(t *testing.T) {
	for _, failure := range []string{"backup", "migrate", "ready", "none"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			os.Mkdir(filepath.Join(root, "data"), 0700)
			os.Mkdir(filepath.Join(root, "backups"), 0700)
			old := filepath.Join(root, "cyapi")
			staged := filepath.Join(root, "candidate")
			os.WriteFile(old, []byte("old"), 0755)
			os.WriteFile(staged, []byte("new"), 0755)
			c := config.Defaults()
			c.RuntimeDirectory = filepath.Join(root, "data")
			c.Maintenance.BackupDirectory = filepath.Join(root, "backups")
			i := installation{Root: root, Binary: old, Config: filepath.Join(root, "config.yaml"), Unit: "test.service"}
			var calls []string
			ops := upgradeOps{
				service: func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil },
				step: func(binary string, timeout time.Duration, args ...string) error {
					calls = append(calls, args[0])
					if args[0] == failure {
						return errors.New("injected failure")
					}
					return nil
				},
				wait: func(string, time.Duration) error {
					calls = append(calls, "ready")
					if failure == "ready" {
						return errors.New("not ready")
					}
					return nil
				},
			}
			e := executeUpgrade(i, c, staged, "digest", quietLog(), ops)
			callString := strings.Join(calls, ",")
			switch failure {
			case "backup":
				if e == nil || strings.Contains(callString, "migrate") || !strings.Contains(callString, "start test.service") {
					t.Fatalf("unsafe backup failure: %v %s", e, callString)
				}
			case "migrate":
				if e == nil || strings.Contains(callString, "start test.service") {
					t.Fatalf("restarted after migration failure: %v %s", e, callString)
				}
			case "ready":
				if e == nil || !strings.HasSuffix(callString, "stop test.service") {
					t.Fatalf("failed candidate not stopped: %v %s", e, callString)
				}
			case "none":
				if e != nil || !strings.HasSuffix(callString, "ready") {
					t.Fatalf("upgrade failed: %v %s", e, callString)
				}
			}
			current, _ := os.ReadFile(old)
			if (failure == "backup" || failure == "migrate") && string(current) != "old" {
				t.Fatal("replaced program before backup/migration succeeded")
			}
			if (failure == "ready" || failure == "none") && string(current) != "new" {
				t.Fatal("candidate not installed")
			}
		})
	}
}
func TestBackupRetentionRefusesLinkedContent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600)
	for _, name := range []string{"auto-20261001T000000.000000000Z", "auto-20261002T000000.000000000Z"} {
		dir := filepath.Join(root, name)
		os.Mkdir(dir, 0700)
		b, _ := json.Marshal(backup.Manifest{Format: 1, Database: "database.sqlite", DatabaseSHA256: "example"})
		os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600)
	}
	if e := os.Symlink(outside, filepath.Join(root, "auto-20261001T000000.000000000Z", "linked")); e != nil {
		t.Skip("symlink privilege unavailable")
	}
	if e := pruneBackups(root, 1); e == nil {
		t.Fatal("deleted a linked backup")
	}
	if _, e := os.Stat(filepath.Join(outside, "keep")); e != nil {
		t.Fatal("cleanup escaped backup root")
	}
}

type disabledNetwork struct{}

func (disabledNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network deliberately disabled")
}
func TestMaintenanceOfflineStillBacksUpAndStops(t *testing.T) {
	root := t.TempDir()
	c := config.Defaults()
	c.RuntimeDirectory = filepath.Join(root, "data")
	c.Database.DSN = filepath.Join(c.RuntimeDirectory, "cyapi.db")
	c.Sources.Directory = filepath.Join(c.RuntimeDirectory, "sources")
	c.Maintenance.BackupDirectory = filepath.Join(root, "backups")
	c.Maintenance.Enabled = true
	c.SecretsFile = filepath.Join(root, "secrets.env")
	os.Mkdir(c.RuntimeDirectory, 0700)
	os.WriteFile(c.SecretsFile, []byte("CYAPI_AUTH_SECRET=0123456789012345678901234567890123456789\n"), 0600)
	configPath := filepath.Join(root, "config.yaml")
	b, _ := yaml.Marshal(c)
	os.WriteFile(configPath, b, 0600)
	db, e := persistence.Open(context.Background(), c.Database)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	options := sources.DefaultOptions(c.Sources.Directory)
	options.Client = &http.Client{Transport: disabledNetwork{}}
	manager, e := sources.New(options)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := startMaintenance(ctx, c, configPath, db, manager, quietLog())
	defer wait()
	defer cancel()
	deadline := time.Now().Add(5 * time.Second)
	var state maintenanceState
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(filepath.Join(c.RuntimeDirectory, "maintenance.json"))
		if e == nil && json.Unmarshal(b, &state) == nil && !state.Backup.LastSuccess.IsZero() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state.Sync.Error == "" || state.Backup.LastSuccess.IsZero() || state.Backup.Error != "" {
		t.Fatalf("offline maintenance result: %+v", state)
	}
	if _, e := os.Stat(filepath.Join(state.Backup.Output, "secrets.env")); e != nil {
		t.Fatal("automatic recovery backup lacks secrets", e)
	}
	if taskDue(state.Backup, c.Maintenance.BackupInterval, time.Now()) {
		t.Fatal("restart would duplicate a successful backup")
	}
	cancel()
}
