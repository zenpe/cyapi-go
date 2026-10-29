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

type Config struct {
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
	PublicBaseURL      string        `yaml:"public_base_url"`
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
	Directory      string `yaml:"directory"`
	ManifestFile   string `yaml:"manifest_file"`
	GitHubTokenEnv string `yaml:"github_token_env"`
	AllowEmpty     bool   `yaml:"allow_empty"`
}
type Mail struct {
	SenderName  string `yaml:"sender_name"`
	AppName     string `yaml:"app_name"`
	Provider    string `yaml:"provider"`
	APIKeyEnv   string `yaml:"api_key_env"`
	SenderEmail string `yaml:"sender_email"`
}
type Providers struct {
	NeteaseCookieEnv string `yaml:"netease_cookie_env"`
	QQCookieEnv      string `yaml:"qq_cookie_env"`
	KugouCookieEnv   string `yaml:"kugou_cookie_env"`
}

func Defaults() Config {
	var c Config
	c.Server = Server{Listen: "127.0.0.1:8080", ShutdownGrace: 30 * time.Second}
	c.Database = Database{Driver: "sqlite", DSN: "data/cyapi.db", BusyTimeout: 5 * time.Second}
	c.Auth = Auth{SecretEnv: "CYAPI_AUTH_SECRET", TokenMaxAge: 720 * time.Hour}
	c.Cache = Cache{MaxBytes: 16 << 20, MaxEntries: 2000, MaxItemBytes: 1 << 20}
	c.Sources = Sources{Directory: "data/sources", ManifestFile: "sources.yaml", GitHubTokenEnv: "GITHUB_TOKEN"}
	c.Mail = Mail{Provider: "brevo", APIKeyEnv: "BREVO_API_KEY", SenderName: "CyreneMusic", AppName: "CyreneMusic"}
	c.Providers = Providers{"NETEASE_COOKIE", "QQ_COOKIE", "KUGOU_COOKIE"}
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
	for _, v := range []struct {
		key  string
		dest *string
	}{
		{"CYAPI_LISTEN", &c.Server.Listen}, {"CYAPI_PUBLIC_BASE_URL", &c.Server.PublicBaseURL},
		{"CYAPI_DATABASE_DRIVER", &c.Database.Driver}, {"CYAPI_DATABASE_DSN", &c.Database.DSN},
	} {
		if value, ok := os.LookupEnv(v.key); ok {
			*v.dest = value
		}
	}
	if _, override := os.LookupEnv("CYAPI_DATABASE_DSN"); !override && c.Database.DSNEnv != "" {
		c.Database.DSN = os.Getenv(c.Database.DSNEnv)
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
	c.Sources.ManifestFile = resolve(c.Sources.ManifestFile)
	return c, nil
}

func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return errors.New("server.listen must be host:port")
	}
	if c.Server.ShutdownGrace <= 0 {
		return errors.New("shutdown_grace must be positive")
	}
	if c.Server.PublicBaseURL != "" {
		u, err := url.Parse(c.Server.PublicBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("public_base_url must be an HTTP(S) origin")
		}
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
	if c.Sources.Directory == "" || c.Sources.ManifestFile == "" {
		return errors.New("source paths cannot be empty")
	}
	return nil
}
func (c Config) Secret() (string, error) {
	value := os.Getenv(c.Auth.SecretEnv)
	if strings.TrimSpace(value) == "" {
		return "", errors.New("authentication secret is missing")
	}
	return value, nil
}
