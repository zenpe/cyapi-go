package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Maintenance struct {
	Enabled         bool          `yaml:"enabled"`
	SyncInterval    time.Duration `yaml:"sync_interval"`
	BackupInterval  time.Duration `yaml:"backup_interval"`
	BackupDirectory string        `yaml:"backup_directory"`
	BackupKeep      int           `yaml:"backup_keep"`
}

type Config struct {
	FilePath         string      `yaml:"-"`
	SecretsFile      string      `yaml:"secrets_file"`
	RuntimeDirectory string      `yaml:"runtime_directory"`
	Maintenance      Maintenance `yaml:"maintenance"`
	values           map[string]string

	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Auth      Auth      `yaml:"auth"`
	Cache     Cache     `yaml:"cache"`
	Sources   Sources   `yaml:"sources"`
	Mail      Mail      `yaml:"mail"`
	Providers Providers `yaml:"providers"`
}
type Server struct {
	Listen             string        `yaml:"listen"`
	TrustedProxies     []string      `yaml:"trusted_proxies"`
	CORSAllowedOrigins []string      `yaml:"cors_allowed_origins"`
	ShutdownGrace      time.Duration `yaml:"shutdown_grace"`
}
type Database struct {
	Driver      string        `yaml:"driver"`
	DSN         string        `yaml:"dsn"`
	DSNEnv      string        `yaml:"dsn_env"`
	BusyTimeout time.Duration `yaml:"busy_timeout"`
}
type Auth struct {
	SecretEnv   string        `yaml:"secret_env"`
	TokenMaxAge time.Duration `yaml:"token_max_age"`
}
type Cache struct {
	MaxBytes     int64 `yaml:"max_bytes"`
	MaxEntries   int   `yaml:"max_entries"`
	MaxItemBytes int64 `yaml:"max_item_bytes"`
}
type Sources struct {
	Repo           string        `yaml:"repo"`
	MaxZipBytes    int64         `yaml:"max_zip_bytes"`
	MaxFileBytes   int64         `yaml:"max_file_bytes"`
	MaxTotalBytes  int64         `yaml:"max_total_bytes"`
	MaxFiles       int           `yaml:"max_files"`
	ReserveBytes   int64         `yaml:"reserve_bytes"`
	Timeout        time.Duration `yaml:"timeout"`
	Directory      string        `yaml:"directory"`
	GitHubTokenEnv string        `yaml:"github_token_env"`
	AllowEmpty     bool          `yaml:"allow_empty"`
}
type Mail struct {
	SenderName  string `yaml:"sender_name"`
	AppName     string `yaml:"app_name"`
	Provider    string `yaml:"provider"`
	APIKeyEnv   string `yaml:"api_key_env"`
	SenderEmail string `yaml:"sender_email"`
}
type Providers struct {
	MaxConcurrent         int           `yaml:"max_concurrent"`
	RequestTimeout        time.Duration `yaml:"request_timeout"`
	UserRequestsPerMinute int           `yaml:"user_requests_per_minute"`
	IPRequestsPerMinute   int           `yaml:"ip_requests_per_minute"`
	NeteaseCookieEnv      string        `yaml:"netease_cookie_env"`
	QQCookieEnv           string        `yaml:"qq_cookie_env"`
	KugouCookieEnv        string        `yaml:"kugou_cookie_env"`
}

func Defaults() Config {
	var c Config
	c.RuntimeDirectory = "data"
	c.SecretsFile = "secrets.env"
	c.Maintenance = Maintenance{SyncInterval: 6 * time.Hour, BackupInterval: 24 * time.Hour, BackupDirectory: "backups", BackupKeep: 7}
	c.Server = Server{Listen: "127.0.0.1:8080", ShutdownGrace: 30 * time.Second}
	c.Database = Database{Driver: "sqlite", DSN: "data/cyapi.db", BusyTimeout: 5 * time.Second}
	c.Auth = Auth{SecretEnv: "CYAPI_AUTH_SECRET", TokenMaxAge: 720 * time.Hour}
	c.Cache = Cache{MaxBytes: 16 << 20, MaxEntries: 2000, MaxItemBytes: 1 << 20}
	c.Sources = Sources{Repo: "guoyue2010/lxmusic-", MaxZipBytes: 512 << 20, MaxFileBytes: 64 << 20, MaxTotalBytes: 2 << 30, MaxFiles: 10000, ReserveBytes: 1 << 30, Timeout: 10 * time.Minute, Directory: "data/sources", GitHubTokenEnv: "GITHUB_TOKEN"}
	c.Mail = Mail{Provider: "brevo", APIKeyEnv: "BREVO_API_KEY", SenderName: "CyreneMusic", AppName: "CyreneMusic"}
	c.Providers = Providers{MaxConcurrent: 4, RequestTimeout: 8 * time.Second, UserRequestsPerMinute: 120, IPRequestsPerMinute: 300, NeteaseCookieEnv: "NETEASE_COOKIE", QQCookieEnv: "QQ_COOKIE", KugouCookieEnv: "KUGOU_COOKIE"}
	return c
}

// Load resolves paths against the config file, not the working directory.
// Secrets are read only by the subcommand needing them, never serialized here.
func Load(path string) (Config, error) {
	c := Defaults()
	absolute, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	c.FilePath = absolute
	f, err := os.Open(absolute)
	if err != nil {
		return c, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	info, statErr := f.Stat()
	if statErr != nil {
		return c, statErr
	}
	if info.Size() > 1<<20 {
		return c, errors.New("config exceeds 1 MiB")
	}
	dec := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return c, errors.New("invalid config YAML or unknown field")
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return c, errors.New("config must contain one YAML document")
	}
	baseDir := filepath.Dir(absolute)
	if c.SecretsFile != "" {
		if !filepath.IsAbs(c.SecretsFile) {
			c.SecretsFile = filepath.Join(baseDir, c.SecretsFile)
		}
		c.values, err = ReadSecrets(c.SecretsFile)
		if err != nil {
			return c, err
		}
	}
	for _, v := range []struct {
		key  string
		dest *string
	}{
		{"CYAPI_LISTEN", &c.Server.Listen},
		{"CYAPI_DATABASE_DRIVER", &c.Database.Driver}, {"CYAPI_DATABASE_DSN", &c.Database.DSN},
	} {
		if value, ok := os.LookupEnv(v.key); ok {
			*v.dest = value
		}
	}
	if _, override := os.LookupEnv("CYAPI_DATABASE_DSN"); !override && c.Database.DSNEnv != "" {
		c.Database.DSN = c.Env(c.Database.DSNEnv)
	}
	if err = c.Validate(); err != nil {
		return c, err
	}
	base := filepath.Dir(absolute)
	resolve := func(value string) string {
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Join(base, value)
	}
	if c.Database.Driver == "sqlite" {
		c.Database.DSN = resolve(c.Database.DSN)
	}
	c.Sources.Directory = resolve(c.Sources.Directory)
	c.Maintenance.BackupDirectory = resolve(c.Maintenance.BackupDirectory)
	c.RuntimeDirectory = resolve(c.RuntimeDirectory)
	return c, nil
}

func (c Config) Validate() error {
	if c.RuntimeDirectory == "" {
		return errors.New("runtime_directory is required")
	}
	if c.Maintenance.SyncInterval < time.Minute || c.Maintenance.BackupInterval < time.Minute || c.Maintenance.BackupKeep < 1 || c.Maintenance.BackupKeep > 365 || c.Maintenance.BackupDirectory == "" {
		return errors.New("invalid maintenance interval, backup directory or retention")
	}

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return errors.New("server.listen must be host:port")
	}
	if c.Server.ShutdownGrace <= 0 {
		return errors.New("shutdown_grace must be positive")
	}
	for _, p := range c.Server.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			return errors.New("trusted_proxies must contain CIDR prefixes")
		}
	}
	for _, o := range c.Server.CORSAllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("cors_allowed_origins must contain explicit HTTP(S) origins")
		}
	}
	if c.Database.Driver != "sqlite" && c.Database.Driver != "postgres" {
		return errors.New("database.driver must be sqlite or postgres")
	}
	if strings.TrimSpace(c.Database.DSN) == "" {
		return errors.New("database DSN is required")
	}
	if c.Database.Driver == "sqlite" && (c.Database.DSN == ":memory:" || strings.HasPrefix(c.Database.DSN, "file:") || strings.ContainsAny(c.Database.DSN, "?\x00")) {
		return errors.New("sqlite DSN must be a local file path without URI parameters")
	}
	if c.Database.BusyTimeout < time.Millisecond || c.Database.BusyTimeout > time.Minute {
		return errors.New("busy_timeout must be between 1ms and 1m")
	}
	if c.Auth.TokenMaxAge <= 0 || c.Auth.SecretEnv == "" {
		return errors.New("auth secret_env and positive token_max_age are required")
	}
	if c.Cache.MaxBytes < 0 || c.Cache.MaxEntries < 0 || c.Cache.MaxItemBytes < 0 || c.Cache.MaxItemBytes > c.Cache.MaxBytes {
		return errors.New("invalid cache capacity")
	}
	if c.Providers.MaxConcurrent < 1 || c.Providers.MaxConcurrent > 16 || c.Providers.RequestTimeout < time.Second || c.Providers.RequestTimeout > 20*time.Second || c.Providers.UserRequestsPerMinute < 1 || c.Providers.IPRequestsPerMinute < 1 {
		return errors.New("invalid provider request limits")
	}
	if c.Sources.Timeout < time.Second || c.Sources.Timeout > time.Hour {
		return errors.New("sources.timeout must be between 1s and 1h")
	}
	if c.Sources.Directory == "" {
		return errors.New("source paths cannot be empty")
	}
	return nil
}
func (c Config) Secret() (string, error) {
	value := c.Env(c.Auth.SecretEnv)
	if strings.TrimSpace(value) == "" {
		return "", errors.New("authentication secret is missing")
	}
	return value, nil
}
