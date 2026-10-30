package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/persistence"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

type installation struct {
	Format  int    `json:"format"`
	Root    string `json:"root"`
	Binary  string `json:"binary"`
	Config  string `json:"config"`
	Unit    string `json:"unit,omitempty"`
	User    string `json:"user,omitempty"`
	Version string `json:"version"`
}

func installLocal(args []string, log *slog.Logger, bootstrap bool) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	dir := flags.String("dir", ".", "installation directory")
	listen := flags.String("listen", "127.0.0.1:8080", "listen address for a new installation")
	service := flags.String("service", "auto", "auto, systemd or none")
	_ = flags.String("user", "", "service user")
	nonInteractive := flags.Bool("non-interactive", false, "read first-account password from CYAPI_INSTALL_PASSWORD")
	username := flags.String("username", "", "first-account username")
	email := flags.String("email", "", "first-account email")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments; run cyapi install --help")
	}
	if *service != "auto" && *service != "systemd" && *service != "none" {
		return errors.New("--service must be auto, systemd or none")
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
	for _, name := range []string{"data", "backups", "logs"} {
		if e = regularDirectory(filepath.Join(root, name)); e != nil {
			return e
		}
	}
	unlock, e := acquireLock(filepath.Join(root, "data", "management.lock"))
	if e != nil {
		return e
	}
	defer unlock()
	configPath := filepath.Join(root, "config.yaml")
	if _, e = os.Lstat(configPath); errors.Is(e, os.ErrNotExist) {
		c := config.Defaults()
		c.Maintenance.Enabled = true
		c.Server.Listen = *listen
		if e = c.Validate(); e != nil {
			return e
		}
		b, e := yaml.Marshal(c)
		if e != nil {
			return e
		}
		if e = writeNew(configPath, b, 0600); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	c, e := config.Load(configPath)
	if e != nil {
		return e
	}
	// Never silently initialize a different live deployment through process overrides.
	if _, e = os.Stat(c.Database.DSN); c.Database.Driver == "sqlite" && e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	existingDB := false
	if c.Database.Driver == "sqlite" {
		info, e := os.Stat(c.Database.DSN)
		existingDB = e == nil && info.Size() > 0
	} else {
		existingDB = true
	}
	if _, e = c.Secret(); e != nil {
		if existingDB {
			return errors.New("existing database has no authentication secret; restore secrets.env or provide the original environment secret")
		}
		if c.SecretsFile == "" {
			return errors.New("set secrets_file or provide the authentication secret")
		}
		if _, e = os.Lstat(c.SecretsFile); !errors.Is(e, os.ErrNotExist) {
			return errors.New("existing secrets file has no usable authentication secret; repair it without overwriting")
		}
		b := make([]byte, 48)
		if _, e = rand.Read(b); e != nil {
			return e
		}
		if e = writeNew(c.SecretsFile, []byte(c.Auth.SecretEnv+"="+hex.EncodeToString(b)+"\n"), 0600); e != nil {
			return e
		}
		c, e = config.Load(configPath)
		if e != nil {
			return e
		}
	}
	secret, e := c.Secret()
	if e != nil || len(secret) < 32 {
		return errors.New("authentication secret must contain at least 32 bytes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, e := persistence.Open(ctx, c.Database)
	if e != nil {
		return e
	}
	defer db.Close()
	instanceRelease, lockErr := acquireLock(filepath.Join(c.RuntimeDirectory, "serve.lock"))
	if lockErr == nil {
		defer instanceRelease()
		e = db.Migrate(ctx)
		if e != nil {
			return e
		}
	} else if e = db.CheckSchema(ctx); e != nil {
		return errors.New("stop the existing API before installing schema changes")
	}
	var count int64
	if e = db.ORM.WithContext(ctx).Table("users").Count(&count).Error; e != nil {
		return e
	}
	if count == 0 {
		if lockErr != nil {
			return errors.New("stop the API before creating the first account")
		}
		accountName, accountEmail, password, e := installationAccount(*username, *email, *nonInteractive)
		if e != nil {
			return e
		}
		accountCtx, accountCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer accountCancel()
		if e = createFirstAccount(accountCtx, db, accountEmail, accountName, password); e != nil {
			return e
		}
		fmt.Println("First account created:", accountName, "(no verification email sent)")
	}
	if instanceRelease != nil {
		instanceRelease()
	}
	if bootstrap {
		return nil
	}
	binary := filepath.Join(root, "cyapi")
	if targetOS() == "windows" {
		binary += ".exe"
	}
	self, e := os.Executable()
	if e != nil {
		return e
	}
	self, e = filepath.EvalSymlinks(self)
	if e != nil {
		return e
	}
	if filepath.Clean(self) != filepath.Clean(binary) {
		if _, e = os.Lstat(binary); e == nil {
			same, err := identicalFiles(self, binary)
			if err != nil || !same {
				return errors.New("a different binary already exists in the target directory; run install from it or use upgrade")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		} else if e = copyExecutable(self, binary); e != nil {
			return e
		}
	}
	script := "#!/bin/sh\nset -eu\nexec " + shellQuote(binary) + " serve --config " + shellQuote(configPath) + " --log-file " + shellQuote(filepath.Join(root, "logs", "server.log")) + "\n"
	if e = writeAtomic(filepath.Join(root, "start.sh"), []byte(script), 0700); e != nil {
		return e
	}
	installed := installation{Format: 1, Root: root, Binary: binary, Config: configPath, Version: version}
	if e = writeJSONFile(filepath.Join(root, "data", "install.json"), installed); e != nil {
		return e
	}
	fmt.Println("Files installed. Boot startup is NOT configured. Run ./start.sh or configure a NAS startup task.")
	address := healthOrigin(c.Server.Listen)
	fmt.Println("Client API address:", address)
	if c.Env(c.Mail.APIKeyEnv) == "" || c.Mail.SenderEmail == "" {
		fmt.Println("Mail is not configured: use the local first account; public registration/password recovery are disabled.")
	}
	log.Info("installation completed", "directory", root)
	return nil
}
func installationAccount(name, email string, nonInteractive bool) (string, string, string, error) {
	reader := bufio.NewReader(os.Stdin)
	line := func() (string, error) {
		s, e := reader.ReadString('\n')
		if errors.Is(e, io.EOF) && s != "" {
			e = nil
		}
		return strings.TrimSpace(s), e
	}
	email = strings.ToLower(strings.TrimSpace(email))
	password := os.Getenv("CYAPI_INSTALL_PASSWORD")
	if nonInteractive {
		if e := auth.ValidateNewAccount(email, name, password); e != nil {
			return "", "", "", e
		}
		return name, email, password, nil
	}
	field := func(value, prompt string, valid func(string) bool) (string, error) {
		if value != "" {
			if !valid(value) {
				return "", fmt.Errorf("invalid %s", prompt)
			}
			return value, nil
		}
		for {
			fmt.Print(prompt + ": ")
			v, e := line()
			if e != nil {
				return "", e
			}
			if valid(v) {
				return v, nil
			}
			fmt.Println("Invalid value; please try again.")
		}
	}
	var e error
	name, e = field(name, "First username (4-20 ASCII letters/digits/_)", auth.ValidUsername)
	if e != nil {
		return "", "", "", e
	}
	email, e = field(email, "Email (account identity; no verification mail sent)", auth.ValidEmail)
	if e != nil {
		return "", "", "", e
	}
	email = strings.ToLower(email)
	for password == "" {
		readPassword := func(prompt string) (string, error) {
			fmt.Print(prompt)
			p, e := terminalPassword(func() (string, error) {
				s, e := reader.ReadString('\n')
				return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), e
			})
			fmt.Println()
			return p, e
		}
		first, e := readPassword("Password: ")
		if e != nil {
			return "", "", "", errors.New("cannot read hidden password; use a terminal or --non-interactive with CYAPI_INSTALL_PASSWORD")
		}
		if !auth.ValidPassword(first) {
			fmt.Println("Password needs 8+ characters and at most 72 UTF-8 bytes; try again.")
			continue
		}
		second, e := readPassword("Confirm password: ")
		if e != nil {
			return "", "", "", e
		}
		if first != second {
			fmt.Println("Passwords do not match; try again.")
			continue
		}
		password = first
	}
	if e = auth.ValidateNewAccount(email, name, password); e != nil {
		return "", "", "", e
	}
	return name, email, password, nil
}

func createFirstAccount(ctx context.Context, db *persistence.Database, email, name, password string) error {
	if e := auth.ValidateNewAccount(email, name, password); e != nil {
		return e
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(password), 10)
	if e != nil {
		return e
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	return db.ORM.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if e := tx.Table("users").Count(&count).Error; e != nil {
			return e
		}
		if count != 0 {
			return errors.New("first account already exists")
		}
		return tx.Table("users").Create(map[string]any{"email": email, "username": name, "password_hash": string(hash), "created_at": now, "updated_at": now, "is_verified": 1, "verified_at": now, "token_version": 0}).Error
	})
}
func copyExecutable(from, to string) error {
	info, e := os.Lstat(from)
	if e != nil || !info.Mode().IsRegular() {
		return errors.New("binary must be a regular file")
	}
	f, e := os.Open(from)
	if e != nil {
		return e
	}
	defer f.Close()
	out, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if e != nil {
		return e
	}
	if e = out.Chmod(0755); e != nil {
		out.Close()
		os.Remove(to)
		return e
	}
	_, e = io.Copy(out, f)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e != nil {
		os.Remove(to)
		return e
	}
	return ce
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func inside(root, path string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func loadInstallation(c config.Config) (installation, error) {
	var i installation
	b, e := os.ReadFile(filepath.Join(filepath.Dir(c.FilePath), ".cyapi-install.json"))
	if errors.Is(e, os.ErrNotExist) {
		b, e = os.ReadFile(filepath.Join(c.RuntimeDirectory, "install.json"))
	}
	if e != nil {
		return i, e
	}
	e = json.Unmarshal(b, &i)
	if e != nil || (i.Format != 1 && i.Format != 2) || !filepath.IsAbs(i.Root) || !inside(i.Root, i.Binary) || !inside(i.Root, i.Config) {
		return installation{}, errors.New("invalid installation metadata")
	}
	if i.Unit != "" {
		hash := sha256.Sum256([]byte(i.Root))
		if i.Unit != "cyapi-"+hex.EncodeToString(hash[:6])+".service" {
			return installation{}, errors.New("service metadata does not match installation directory")
		}
	}
	return i, nil
}
func systemdAvailable() bool {
	if targetOS() != "linux" {
		return false
	}
	if _, e := os.Stat("/run/systemd/system"); e != nil {
		return false
	}
	_, e := exec.LookPath("systemctl")
	return e == nil
}
func isRoot() bool { u, e := user.Current(); return e == nil && u.Uid == "0" }
func systemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if e := exec.CommandContext(ctx, "systemctl", args...).Run(); e != nil {
		return fmt.Errorf("systemctl %s failed; inspect journalctl and logs/server.log", strings.Join(args, " "))
	}
	return nil
}
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}
func serviceUnit(i installation) string {
	return "[Unit]\nDescription=Cyapi music API\nWants=network-online.target\nAfter=network-online.target\nStartLimitIntervalSec=60\nStartLimitBurst=3\n[Service]\nUser=" + i.User + "\nExecStart=" + systemdQuote(i.Binary) + " serve --config " + systemdQuote(i.Config) + " --log-file " + systemdQuote(filepath.Join(i.Root, "logs", "server.log")) + "\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=40\nUMask=0077\nNoNewPrivileges=true\nProtectSystem=strict\nProtectHome=read-only\nPrivateTmp=true\nReadWritePaths=" + systemdQuote(i.Root) + "\n[Install]\nWantedBy=multi-user.target\n"
}
func registerService(i installation) error {
	path := filepath.Join("/etc/systemd/system", i.Unit)
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return errors.New("service unit is not a regular file")
	}
	if e := writeAtomic(path, []byte(serviceUnit(i)), 0644); e != nil {
		return e
	}
	if e := systemctl("daemon-reload"); e != nil {
		return e
	}
	return nil
}
func healthOrigin(listen string) string {
	host, port, e := net.SplitHostPort(listen)
	if e != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func identicalFiles(a, b string) (bool, error) {
	digest := func(path string) (string, error) {
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() {
			return "", errors.New("expected regular file")
		}
		f, e := os.Open(path)
		if e != nil {
			return "", e
		}
		defer f.Close()
		h := sha256.New()
		_, e = io.Copy(h, f)
		return hex.EncodeToString(h.Sum(nil)), e
	}
	da, e := digest(a)
	if e != nil {
		return false, e
	}
	db, e := digest(b)
	return da == db, e
}
