package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cyapi-go/internal/config"
	"gopkg.in/yaml.v3"
)

func unitName(root string) string {
	hash := sha256.Sum256([]byte(root))
	return "cyapi-" + hex.EncodeToString(hash[:6]) + ".service"
}
func installCommand(args []string, log *slog.Logger) error {
	f := flag.NewFlagSet("install", flag.ContinueOnError)
	dir := f.String("dir", ".", "installation directory")
	listen := f.String("listen", "127.0.0.1:8080", "listen address")
	service := f.String("service", "auto", "auto, systemd or none")
	account := f.String("user", "cyapi", "service user")
	_ = f.Bool("non-interactive", false, "unattended account setup")
	_ = f.String("username", "", "first username")
	_ = f.String("email", "", "first email")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected install arguments")
	}
	if *service != "none" && *service != "auto" && *service != "systemd" {
		return errors.New("invalid service mode")
	}
	managed := *service == "systemd" || (*service == "auto" && isRoot() && systemdAvailable())
	if !managed {
		root, e := filepath.Abs(*dir)
		if e != nil {
			return e
		}
		if _, e = os.Stat(filepath.Join(root, ".cyapi-install.json")); e == nil {
			return errors.New("managed installation exists; use sudo cyapi install or upgrade")
		}
		// Check binding before creating configuration, database or accounts.
		address := *listen
		if c, e := config.Load(filepath.Join(root, "config.yaml")); e == nil {
			address = c.Server.Listen
		}
		if e := checkPort(address); e != nil {
			return e
		}
		return installLocal(args, log, false)
	}
	if !isRoot() || !systemdAvailable() {
		return errors.New("systemd installation requires root and a running systemd manager")
	}
	if e := rejectManagedOverrides(); e != nil {
		return e
	}
	root, e := filepath.Abs(*dir)
	if e != nil {
		return e
	}
	if strings.ContainsAny(root, "\n\r\x00") {
		return errors.New("invalid installation path")
	}
	if e = os.MkdirAll(root, 0755); e != nil {
		return e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	i := installation{Format: 2, Root: root, Binary: filepath.Join(root, "cyapi"), Config: filepath.Join(root, "config.yaml"), Unit: unitName(root), User: *account, Version: version}
	if _, e = os.Stat(filepath.Join(root, ".cyapi-install.json")); e == nil {
		c, err := config.Load(i.Config)
		if err != nil {
			return err
		}
		old, err := loadManagedInstallation(c)
		if err != nil {
			return err
		}
		i.User = old.User
	}
	unlock, e := acquireLock(filepath.Join(root, ".cyapi-management.lock"))
	if e != nil {
		return e
	}
	defer unlock()
	c := config.Defaults()
	c.Server.Listen = *listen
	c.Maintenance.Enabled = true
	if _, err := os.Lstat(i.Config); err == nil {
		c, e = config.Load(i.Config)
		if e != nil {
			return e
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if e = c.Validate(); e != nil {
		return e
	}
	if serviceActive(i.Unit) {
		if e = verifyServiceDefinition(i); e != nil {
			return e
		}
		if e = waitManagedReady(i, c.Server.Listen, "", 3*time.Second); e != nil {
			return e
		}
		fmt.Println("Existing service is running; settings and accounts preserved. Use config edit to change settings.")
		return nil
	}
	if e = checkPort(c.Server.Listen); e != nil {
		return e
	}
	if e = ensureServiceUser(i.User); e != nil {
		return e
	}
	self, e := os.Executable()
	if e != nil {
		return e
	}
	self, e = filepath.EvalSymlinks(self)
	if e != nil {
		return e
	}
	if self != i.Binary {
		if _, err := os.Lstat(i.Binary); err == nil {
			same, err := identicalFiles(self, i.Binary)
			if err != nil || !same {
				return errors.New("different installed binary exists; run the new binary with upgrade --file NEW_BINARY")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if e = copyExecutable(self, i.Binary); e != nil {
			return e
		}
	}
	if e = securePath(root, "root", 0755); e != nil {
		return e
	}
	if e = securePath(i.Binary, "root", 0755); e != nil {
		return e
	}
	if _, err := os.Lstat(i.Config); errors.Is(err, os.ErrNotExist) {
		b, err := yaml.Marshal(c)
		if err != nil {
			return err
		}
		if e = writeNew(i.Config, b, 0600); e != nil {
			return e
		}
	}
	c, e = config.Load(i.Config)
	if e != nil {
		return e
	}
	if e = validateManagedPaths(i, c); e != nil {
		return e
	}
	// Provision the key once; all subsequent data work runs under the service UID.
	if _, err := c.Secret(); err != nil {
		if info, err := os.Stat(c.Database.DSN); c.Database.Driver != "sqlite" || (err == nil && info.Size() > 0) {
			return errors.New("existing database has no authentication secret; restore the original secrets.env")
		}
		if _, err := os.Lstat(c.SecretsFile); !errors.Is(err, os.ErrNotExist) {
			return errors.New("existing secrets file requires repair; it will not be overwritten")
		}
		b := make([]byte, 48)
		if _, e = rand.Read(b); e != nil {
			return e
		}
		if e = writeNew(c.SecretsFile, []byte(c.Auth.SecretEnv+"="+hex.EncodeToString(b)+"\n"), 0600); e != nil {
			return e
		}
	}
	c, e = config.Load(i.Config)
	if e != nil {
		return e
	}
	if e = prepareManagedPaths(i, c); e != nil {
		return e
	}
	if e = checkAsService(i, true); e != nil {
		return e
	}
	childArgs := append([]string{"_install-local"}, args...)
	childArgs = append(childArgs, "--dir", root, "--service", "none")
	// No wall-clock timeout while a person is typing an account/password.
	cmd, e := serviceCommand(context.Background(), i.User, i.Binary, childArgs...)
	if e != nil {
		return e
	}
	cmd.Env = append(cmd.Env, "CYAPI_INSTALL_PASSWORD="+os.Getenv("CYAPI_INSTALL_PASSWORD"))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		return fmt.Errorf("account/database initialization failed: %w; rerun install to resume", e)
	}
	if e = writeManagedInstallation(i); e != nil {
		return e
	}
	if e = writeAtomic(filepath.Join(root, "start.sh"), []byte("#!/bin/sh\nexec "+shellQuote(i.Binary)+" serve --config "+shellQuote(i.Config)+" --log-file "+shellQuote(filepath.Join(root, "logs/server.log"))+"\n"), 0755); e != nil {
		return e
	}
	if e = checkAsService(i, true); e != nil {
		return e
	}
	if e = registerService(i); e != nil {
		return e
	}
	_ = systemctl("reset-failed", i.Unit)
	if e = systemctl("start", i.Unit); e == nil {
		e = waitManagedReady(i, c.Server.Listen, version, 20*time.Second)
	}
	if e != nil {
		detail := serviceFailure(i.Unit)
		_ = systemctl("stop", i.Unit)
		return fmt.Errorf("installation preserved, service stopped after startup failure: %v\n%s", e, detail)
	}
	if e = systemctl("enable", i.Unit); e != nil {
		return e
	}
	fmt.Println("Installed and running; boot startup enabled:", i.Unit)
	fmt.Println("Listening:", c.Server.Listen, "— use your NAS IP/domain in the client")
	return nil
}

func checkPort(address string) error {
	l, e := net.Listen("tcp", address)
	if e != nil {
		return fmt.Errorf("cannot listen on %s: %w; choose another --listen port (or edit the existing config)", address, e)
	}
	return l.Close()
}
func ensureServiceUser(name string) error {
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`).MatchString(name) || name == "root" {
		return errors.New("service user must be an unprivileged Linux account")
	}
	if u, e := user.Lookup(name); e == nil {
		if u.Uid == "0" {
			return errors.New("service UID cannot be 0")
		}
		return nil
	}
	if name != "cyapi" {
		return errors.New("requested service user does not exist")
	}
	return exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name).Run()
}
func rejectManagedOverrides() error {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CYAPI_") && key != "CYAPI_INSTALL_PASSWORD" {
			return fmt.Errorf("%s is a shell-only override; put managed settings in config.yaml/secrets.env", key)
		}
	}
	return nil
}
func serviceActive(unit string) bool { return systemctl("is-active", "--quiet", unit) == nil }
func serviceFailure(unit string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", "8", "--no-pager", "-o", "cat").CombinedOutput()
	if len(b) == 0 {
		return "Inspect journalctl -u " + unit
	}
	return strings.TrimSpace(string(b))
}
func writeManagedInstallation(i installation) error {
	i.Format = 2
	path := filepath.Join(i.Root, ".cyapi-install.json")
	if e := writeJSONFile(path, i); e != nil {
		return e
	}
	return securePath(path, "root", 0644)
}
func loadManagedInstallation(c config.Config) (installation, error) {
	root := filepath.Dir(c.FilePath)
	path := filepath.Join(root, ".cyapi-install.json")
	if e := checkRootOwned(root, true); e != nil {
		return installation{}, e
	}
	if e := checkRootOwned(path, false); e != nil {
		return installation{}, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return installation{}, e
	}
	var i installation
	if e = json.Unmarshal(b, &i); e != nil {
		return i, e
	}
	if i.Format != 2 || i.Root != root || i.Binary != filepath.Join(root, "cyapi") || i.Config != c.FilePath || i.Unit != unitName(root) || i.User == "" || i.User == "root" {
		return i, errors.New("invalid protected installation metadata")
	}
	if e = checkRootOwned(i.Binary, false); e != nil {
		return i, e
	}
	return i, validateManagedPaths(i, c)
}
func validateManagedPaths(i installation, c config.Config) error {
	for _, p := range []string{c.RuntimeDirectory, c.Sources.Directory, c.Maintenance.BackupDirectory, c.SecretsFile} {
		if p == "" || p == i.Root || !inside(i.Root, p) {
			return errors.New("managed config, secrets and data must remain inside the installation directory")
		}
		if e := rejectSymlinkPath(i.Root, p); e != nil {
			return e
		}
	}
	if c.Database.Driver == "sqlite" {
		if !inside(i.Root, c.Database.DSN) || c.Database.DSN == i.Root {
			return errors.New("managed SQLite must remain inside the installation directory")
		}
		if e := rejectSymlinkPath(i.Root, c.Database.DSN); e != nil {
			return e
		}
	}
	return rejectSymlinkPath(i.Root, i.Config)
}
func rejectSymlinkPath(root, path string) error {
	for p := path; p != root; p = filepath.Dir(p) {
		if !inside(root, p) {
			return errors.New("path escapes installation directory")
		}
		info, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed path is a symbolic link: %s", p)
		}
	}
	return nil
}
func prepareManagedPaths(i installation, c config.Config) error {
	if e := validateManagedPaths(i, c); e != nil {
		return e
	}
	dirs := []string{filepath.Join(i.Root, "data"), filepath.Join(i.Root, "logs"), filepath.Join(i.Root, "backups"), c.RuntimeDirectory, c.Sources.Directory, c.Maintenance.BackupDirectory}
	if c.Database.Driver == "sqlite" {
		dirs = append(dirs, filepath.Dir(c.Database.DSN))
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == i.Root {
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			mode := os.FileMode(0600)
			if d.IsDir() {
				mode = 0700
			} else if !d.Type().IsRegular() {
				return fmt.Errorf("managed path is not a regular file/directory: %s", p)
			}
			return securePath(p, i.User, mode)
		}); e != nil {
			return e
		}
	}
	for _, p := range []string{i.Config, c.SecretsFile} {
		if e := securePath(p, i.User, 0600); e != nil {
			return e
		}
	}
	return nil
}
func checkAsService(i installation, write bool) error {
	args := []string{"_check-access", "--config", i.Config}
	if write {
		args = append(args, "--write")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, e := serviceCommand(ctx, i.User, i.Binary, args...)
	if e != nil {
		return e
	}
	b, e := cmd.CombinedOutput()
	if e != nil {
		return fmt.Errorf("service user %s access check failed: %s (%w); inspect parent-directory permissions/ACL", i.User, strings.TrimSpace(string(b)), e)
	}
	return nil
}
func accessCommand(args []string) error {
	f := flag.NewFlagSet("access", flag.ContinueOnError)
	path := f.String("config", "config.yaml", "")
	write := f.Bool("write", false, "")
	if e := f.Parse(args); e != nil {
		return e
	}
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	if _, e = c.Secret(); e != nil {
		return e
	}
	dirs := []string{c.RuntimeDirectory, c.Maintenance.BackupDirectory, c.Sources.Directory, filepath.Join(filepath.Dir(c.FilePath), "logs")}
	if c.Database.Driver == "sqlite" {
		dirs = append(dirs, filepath.Dir(c.Database.DSN))
		if _, e = os.Stat(c.Database.DSN); e == nil {
			f, e := os.OpenFile(c.Database.DSN, os.O_RDWR, 0)
			if e != nil {
				return e
			}
			f.Close()
		}
	}
	for _, dir := range dirs {
		if e = checkWritableDirectory(dir); e != nil {
			return fmt.Errorf("directory %s: %w", dir, e)
		}
		if *write {
			if e = probeDirectory(dir); e != nil {
				return e
			}
		}
	}
	logPath := filepath.Join(filepath.Dir(c.FilePath), "logs/server.log")
	if _, e = os.Stat(logPath); e == nil {
		f, e := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
		if e != nil {
			return e
		}
		f.Close()
	}
	return nil
}
func probeDirectory(dir string) error {
	f, e := os.CreateTemp(dir, ".cyapi-access-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	defer os.Remove(name + ".renamed")
	_, e = io.Copy(f, bytes.NewBufferString("access check"))
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(name, name+".renamed")
}

// Legacy adoption derives privileged paths from the explicit config and root-owned
// systemd unit, never from the service-writable data/install.json.
func loadUpgradeInstallation(c config.Config) (installation, error) {
	if e := rejectManagedOverrides(); e != nil {
		return installation{}, e
	}
	root := filepath.Dir(c.FilePath)
	if _, e := os.Lstat(filepath.Join(root, ".cyapi-install.json")); !errors.Is(e, os.ErrNotExist) {
		return loadManagedInstallation(c)
	}
	i := installation{Format: 2, Root: root, Binary: filepath.Join(root, "cyapi"), Config: c.FilePath, Unit: unitName(root), Version: version}
	if e := checkRootOwned(filepath.Join("/etc/systemd/system", i.Unit), false); e != nil {
		return i, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, "systemctl", "show", i.Unit, "-p", "User", "--value").Output()
	if e != nil {
		return i, e
	}
	i.User = strings.TrimSpace(string(b))
	if e = ensureServiceUser(i.User); e != nil {
		return i, e
	}
	if e = verifyServiceDefinition(i); e != nil {
		return i, e
	}
	if e = validateManagedPaths(i, c); e != nil {
		return i, e
	}
	if e = securePath(root, "root", 0755); e != nil {
		return i, e
	}
	if e = securePath(i.Binary, "root", 0755); e != nil {
		return i, e
	}
	fmt.Println("Adopting legacy installation; account, secret, configured port and data will be preserved.")
	return i, nil
}
