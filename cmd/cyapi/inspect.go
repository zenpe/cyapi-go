package main

import (
	"context"
	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/neteaseaccount"
	"cyapi-go/internal/persistence"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func ready(listen string) error {
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Get(healthOrigin(listen) + "/health/ready")
	if e != nil {
		return errors.New("API health endpoint is unreachable")
	}
	defer response.Body.Close()
	var body struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&body) != nil || body.Status != 200 || body.Message != "ready" {
		return errors.New("API is not ready")
	}
	return nil
}
func waitReady(listen string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if e := ready(listen); e == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("readiness timeout")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
func inspectCommand(args []string, doctor bool) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	path := flags.String("config", "config.yaml", "configuration file")
	network := flags.Bool("network", false, "check GitHub connectivity")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments; run cyapi help")
	}
	if !doctor && *network {
		return errors.New("--network is only valid for doctor")
	}
	failed := false
	report := func(level, message string) {
		fmt.Printf("[%s] %s\n", level, message)
		if level == "FAIL" {
			failed = true
		}
	}
	c, e := config.Load(*path)
	if e != nil {
		report("FAIL", e.Error())
		return errors.New("configuration requires repair")
	}
	report("PASS", "Configuration is valid")
	fmt.Println("Program:", version, "commit="+commit)
	if !doctor {
		e = ready(c.Server.Listen)
		if i, err := loadManagedInstallation(c); err == nil {
			e = waitManagedReady(i, c.Server.Listen, "", time.Second)
		}
		if e != nil {
			report("FAIL", e.Error())
		} else {
			report("PASS", "API is ready")
		}
	} else {
		if i, err := loadManagedInstallation(c); err == nil {
			if err = checkAsService(i, false); err != nil {
				report("FAIL", err.Error())
			} else {
				report("PASS", "Service user "+i.User+" can access configuration, secrets, logs and data")
			}
		}
		secret, e := c.Secret()
		if e != nil {
			report("FAIL", "Authentication secret is missing")
		} else if _, e = auth.NewSigner(secret, c.Auth.TokenMaxAge); e != nil {
			report("FAIL", "Authentication secret must contain at least 32 bytes")
		} else {
			report("PASS", "Authentication secret is configured (value hidden)")
		}
		if info, e := os.Lstat(c.SecretsFile); e == nil && targetOS() == "linux" && info.Mode().Perm()&0077 != 0 {
			report("WARN", "secrets.env is readable by other users; use chmod 600")
		}
		directories := []string{c.RuntimeDirectory, c.Maintenance.BackupDirectory}
		if c.Database.Driver == "sqlite" {
			directories = append(directories, filepath.Dir(c.Database.DSN))
		}
		for _, path := range directories {
			if e := checkWritableDirectory(path); e != nil {
				report("FAIL", "Directory is missing or not writable: "+path)
			} else {
				report("PASS", "Directory writable by current user: "+path)
				if free, e := availableBytes(path); e == nil && free < uint64(c.Sources.ReserveBytes) {
					report("WARN", fmt.Sprintf("Low disk space at %s: %d MiB available", path, free>>20))
				}
			}
		}
		var accountCredentialCount int64
		if c.Database.Driver == "sqlite" {
			info, e := os.Stat(c.Database.DSN)
			if e != nil || !info.Mode().IsRegular() {
				report("FAIL", "SQLite database is missing or not a regular file; run install or migrate")
			} else {
				raw, e := sql.Open("sqlite", filepath.ToSlash(c.Database.DSN)+"?mode=ro&_pragma=busy_timeout(3000)")
				if e != nil {
					report("FAIL", "Cannot open database read-only")
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					e = (&persistence.Database{SQL: raw, Driver: "sqlite"}).CheckSchema(ctx)
					if e == nil {
						e = raw.QueryRowContext(ctx, "SELECT count(*) FROM account_bindings WHERE credentials <> ''").Scan(&accountCredentialCount)
					}
					cancel()
					raw.Close()
					if e != nil {
						report("FAIL", "Database schema does not match; inspect schema-status before upgrading")
					} else {
						report("PASS", "Database schema matches")
					}
				}
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			db, e := persistence.Open(ctx, c.Database)
			if e == nil {
				e = db.CheckSchema(ctx)
				if e == nil {
					e = db.ORM.Table("account_bindings").Where("credentials <> ?", "").Count(&accountCredentialCount).Error
				}
				db.Close()
			}
			cancel()
			if e != nil {
				report("FAIL", "PostgreSQL connection/schema check failed")
			} else {
				report("PASS", "PostgreSQL schema matches")
			}
		}

		keyPath := filepath.Join(c.RuntimeDirectory, "netease.key")
		if _, err := os.Lstat(keyPath); err == nil {
			if _, err = neteaseaccount.LoadKey(keyPath, true); err != nil {
				report("FAIL", err.Error())
			} else {
				report("PASS", "Netease credential encryption key is present (value hidden)")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			report("FAIL", "Cannot inspect Netease credential key")
		} else if accountCredentialCount > 0 {
			report("FAIL", "Netease credential key missing; restore netease.key into runtime_directory")
		}

		if c.Env(c.Mail.APIKeyEnv) == "" || c.Mail.SenderEmail == "" {
			report("WARN", "Mail is not configured; public registration/password recovery unavailable")
		} else {
			report("PASS", "Mail fields configured; delivery has not been tested")
		}
		for _, key := range []string{c.Providers.NeteaseCookieEnv, c.Providers.QQCookieEnv, c.Providers.KugouCookieEnv} {
			if c.Env(key) == "" {
				report("WARN", key+" is not configured; platform access may be limited")
			}
		}
		manager, e := sourceManager(c)
		if e != nil {
			report("FAIL", "Source configuration is invalid")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			manifest, e := manager.Status(ctx)
			cancel()
			if e != nil {
				report("WARN", "No verified source generation; run sync-sources or inspect existing files")
			} else {
				report("PASS", fmt.Sprintf("Sources: %s (%d files)", manifest.Tag, len(manifest.Files)))
			}
		}
		if !c.Maintenance.Enabled {
			report("WARN", "Internal scheduled maintenance is disabled")
		}
		if *network {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+c.Sources.Repo+"/releases?per_page=1", nil)
			request.Header.Set("User-Agent", "cyapi-doctor/1")
			client := &http.Client{Timeout: 10 * time.Second}
			r, e := client.Do(request)
			cancel()
			if e != nil {
				report("WARN", "GitHub connectivity failed")
			} else {
				r.Body.Close()
				if r.StatusCode == 200 {
					report("PASS", "GitHub reachable")
				} else {
					report("WARN", fmt.Sprintf("GitHub HTTP %d", r.StatusCode))
				}
			}
		}
	}
	if i, e := loadInstallation(c); e == nil && i.Unit != "" {
		if e = systemctl("is-active", "--quiet", i.Unit); e != nil {
			level := "WARN"
			if !doctor {
				level = "FAIL"
			}
			report(level, "Systemd service is not active: "+i.Unit)
			if !doctor {
				fmt.Println(serviceFailure(i.Unit))
			}
		} else {
			report("PASS", "Systemd service active: "+i.Unit)
		}
	} else {
		report("WARN", "No managed systemd service; configure NAS boot startup if running manually")
	}
	if b, e := os.ReadFile(filepath.Join(c.RuntimeDirectory, "maintenance.json")); e == nil {
		var state maintenanceState
		if json.Unmarshal(b, &state) == nil && state.Format == 1 {
			for _, item := range []struct {
				name string
				task taskResult
			}{{"Source sync", state.Sync}, {"Backup", state.Backup}} {
				if item.task.Running {
					report("INFO", item.name+": in progress (check service state if interrupted)")
				} else if item.task.Error != "" {
					report("WARN", item.name+": "+item.task.Error)
				} else if item.task.LastSuccess.IsZero() {
					report("WARN", item.name+": no recorded success")
				} else {
					report("PASS", item.name+" last success: "+item.task.LastSuccess.Local().Format(time.RFC3339))
				}
			}
		} else {
			report("WARN", "Maintenance state cannot be parsed")
		}
	} else {
		report("WARN", "No maintenance history yet")
	}
	if failed {
		return errors.New("one or more checks failed")
	}
	return nil
}
