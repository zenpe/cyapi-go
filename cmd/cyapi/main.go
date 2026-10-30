package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"cyapi-go/internal/auth"
	"cyapi-go/internal/backup"
	"cyapi-go/internal/compat"
	"cyapi-go/internal/config"
	"cyapi-go/internal/library"
	"cyapi-go/internal/mail"
	"cyapi-go/internal/music"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/persistence/librarystore"
	"cyapi-go/internal/persistence/userstatestore"
	"cyapi-go/internal/providers/kugou"
	"cyapi-go/internal/providers/netease"
	"cyapi-go/internal/providers/platformhttp"
	"cyapi-go/internal/providers/qq"
	"cyapi-go/internal/server"
	"cyapi-go/internal/sources"
	"cyapi-go/internal/userstate"
)

var version = "dev"
var commit = "unknown"

func main() {
	platformDefaults()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("command failed", "error", err.Error())
		os.Exit(1)
	}
}
func run(args []string, log *slog.Logger) error {
	originalCLI := append([]string{}, args...)
	if len(args) > 0 && args[0] == "_install-local" {
		return installLocal(args[1:], log, true)
	}
	if len(args) > 0 && args[0] == "_check-access" {
		return accessCommand(args[1:])
	}

	if handled, err := handleCLI(args, log); handled {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: cyapi <serve|migrate|schema-status|backup|sync-sources|sources|version> [--config path]")
	}
	command := args[0]
	if command == "version" {
		if len(args) != 1 {
			return errors.New("version takes no arguments")
		}
		fmt.Printf("cyapi %s commit=%s go=%s %s/%s\n", version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if command != "serve" && command != "migrate" && command != "schema-status" && command != "sync-sources" && command != "sources" && command != "backup" {
		return errors.New("unknown command; implemented commands: serve, migrate, schema-status, sync-sources, sources, backup, version")
	}
	sourceAction := ""
	if command == "sources" {
		if len(args) < 2 || (args[1] != "status" && args[1] != "rollback") {
			return errors.New("usage: cyapi sources <status|rollback> --config path [--generation id]")
		}
		sourceAction = args[1]
		args = append([]string{args[0]}, args[2:]...)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var force, dry *bool
	var generation *string
	var backupOutput *string
	var includeConfig *bool
	logPath := flags.String("log-file", "", "rotating log file (serve only; 10 MiB x 4)")
	if command == "backup" {
		backupOutput = flags.String("output", "", "new backup directory")
		includeConfig = flags.Bool("include-config", false, "include restricted config and secrets recovery files")
	}
	if command == "sync-sources" {
		force = flags.Bool("force", false, "publish even when unchanged")
		dry = flags.Bool("dry-run", false, "download and validate without publishing")
	}
	if command == "sources" && sourceAction == "rollback" {
		generation = flags.String("generation", "", "previous source generation")
	}

	path := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	// Root management commands must not leave root-owned business files behind.
	if isRoot() {
		if _, e := os.Lstat(filepath.Join(filepath.Dir(c.FilePath), ".cyapi-install.json")); e == nil {
			i, e := loadManagedInstallation(c)
			if e != nil {
				return e
			}
			self, e := os.Executable()
			if e != nil {
				return e
			}
			childArgs := append(append([]string{}, originalCLI...), "--config", c.FilePath)
			cmd, e := serviceCommand(context.Background(), i.User, self, childArgs...)
			if e != nil {
				return e
			}
			cmd.Dir, _ = os.Getwd()
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
	}
	if *logPath != "" {
		if command != "serve" {
			return errors.New("--log-file is only valid for serve")
		}
		writer, err := newRotatingLog(*logPath)
		if err != nil {
			return err
		}
		defer writer.Close()
		log = slog.New(slog.NewJSONHandler(writer, nil))
	}
	if command == "serve" {
		if _, err = c.Secret(); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "sync-sources" || command == "sources" {
		manager, e := sourceManager(c)
		if e != nil {
			return e
		}
		operation, cancel := context.WithTimeout(ctx, c.Sources.Timeout)
		defer cancel()
		if command == "sync-sources" {
			report, e := manager.Sync(operation, *force, *dry)
			if errors.Is(e, sources.ErrBusy) {
				log.Info("source sync skipped: another task holds the lock")
				return nil
			}
			if e != nil {
				return e
			}
			return json.NewEncoder(os.Stdout).Encode(report)
		}
		if sourceAction == "status" {
			manifest, e := manager.Status(operation)
			if e != nil {
				return e
			}
			return json.NewEncoder(os.Stdout).Encode(manifest)
		}
		if generation == nil || *generation == "" {
			return errors.New("rollback requires --generation")
		}
		report, e := manager.Rollback(operation, *generation)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	db, err := persistence.Open(startup, c.Database)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()
	if command == "migrate" {
		release, e := acquireLock(filepath.Join(c.RuntimeDirectory, "serve.lock"))
		if e != nil {
			return e
		}
		defer release()
		operation, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err = db.Migrate(operation); err != nil {
			return err
		}
		log.Info("database migrations applied", "driver", db.Driver)
		return nil
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = db.CheckSchema(check)
	cancel()
	if err != nil {
		return err
	}
	if command == "backup" {
		manager, e := sourceManager(c)
		if e != nil {
			return e
		}
		work, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		manifest, e := backup.Run(work, db, backup.Options{Output: *backupOutput, DSN: c.Database.DSN, Version: version, Commit: commit, SourcesDirectory: c.Sources.Directory, Manager: manager, IncludeConfig: *includeConfig, ConfigPath: *path, SecretsPath: c.SecretsFile})
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(manifest)
	}
	if command == "schema-status" {
		fmt.Println("schema is current")
		return nil
	}

	release, err := acquireLock(filepath.Join(c.RuntimeDirectory, "serve.lock"))
	if err != nil {
		return err
	}
	defer release()
	secret, err := c.Secret()
	if err != nil {
		return err
	}
	if c.Mail.Provider != "brevo" {
		return errors.New("unsupported mail provider")
	}
	mailer := mail.NewBrevo(mail.Options{APIKey: c.Env(c.Mail.APIKeyEnv), SenderEmail: c.Mail.SenderEmail, SenderName: c.Mail.SenderName, AppName: c.Mail.AppName})
	accounts, err := auth.New(authstore.New(db.ORM), mailer, auth.Options{Secret: secret, TokenMaxAge: c.Auth.TokenMaxAge, Logger: log})
	if err != nil {
		return err
	}
	cookie := c.Env(c.Providers.NeteaseCookieEnv)
	neteaseClient, err := netease.New(netease.Options{Cookie: cookie, MaxConcurrent: c.Providers.MaxConcurrent, RequestTimeout: c.Providers.RequestTimeout})
	if err != nil {
		return err
	}
	defer neteaseClient.Close()
	qqClient, err := qq.New(platformhttp.Options{Cookie: c.Env(c.Providers.QQCookieEnv), MaxConcurrent: c.Providers.MaxConcurrent, Timeout: c.Providers.RequestTimeout})
	if err != nil {
		return err
	}
	defer qqClient.Close()
	kugouClient, err := kugou.New(platformhttp.Options{Cookie: c.Env(c.Providers.KugouCookieEnv), MaxConcurrent: c.Providers.MaxConcurrent, Timeout: c.Providers.RequestTimeout})
	if err != nil {
		return err
	}
	defer kugouClient.Close()
	musicService, err := music.New(neteaseClient, music.Options{MaxBytes: c.Cache.MaxBytes, MaxEntries: c.Cache.MaxEntries, MaxItemBytes: c.Cache.MaxItemBytes, PrivateCache: cookie != "", UserRequestsPerMinute: c.Providers.UserRequestsPerMinute, IPRequestsPerMinute: c.Providers.IPRequestsPerMinute})
	if err != nil {
		return err
	}
	sourceFiles, err := sourceManager(c)
	if err != nil {
		return err
	}
	sourceRoutes := sources.NewHandler(sourceFiles, log)
	authRoutes := auth.NewHandler(accounts, server.ClientIP, log)
	musicRoutes := music.NewHandler(musicService, func(next http.HandlerFunc) http.HandlerFunc { return authRoutes.ProtectFor(next, 25*time.Second) })
	libraryStore := librarystore.New(db.ORM)
	libraryRoutes := library.NewHandler(library.NewWithRemote(libraryStore, libraryStore, library.RemoteProviders{"netease": neteaseClient, "qq": qqClient}), authRoutes.Protect, log).WithRemoteProtection(func(next http.HandlerFunc) http.HandlerFunc { return authRoutes.ProtectFor(next, 25*time.Second) })
	userRoutes := userstate.NewHandler(userstate.New(userstatestore.New(db.ORM)), authRoutes.Protect, log)
	compatibility := func(mux *http.ServeMux) { compat.Register(mux, authRoutes.Protect) }
	extraRoutes := func(mux *http.ServeMux) {
		musicService.RegisterExtensions(mux, func(next http.HandlerFunc) http.HandlerFunc { return authRoutes.ProtectFor(next, 25*time.Second) }, map[string]music.Extension{"netease": neteaseClient, "qq": qqClient, "kugou": kugouClient})
	}
	listener, err := net.Listen("tcp", c.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if !mailer.Ready() {
		log.Warn("mail is not configured; public registration and password recovery are unavailable")
	}
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	waitMaintenance := startMaintenance(maintenanceCtx, c, *path, db, sourceFiles, log)
	defer func() { stopMaintenance(); waitMaintenance() }()
	return serveListener(ctx, c.Server, db, log, listener, authRoutes.Register, libraryRoutes.Register, sourceRoutes.Register, musicRoutes.Register, userRoutes.Register, compatibility, extraRoutes)
}

func serveListener(ctx context.Context, c config.Server, db server.Readiness, log *slog.Logger, listener net.Listener, register ...func(*http.ServeMux)) error {
	var err error
	srv := &http.Server{Handler: identityHandler(server.NewHandler(c, db, log, register...)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	log.Info("server started", "listen", listener.Addr().String(), "version", version)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), c.ShutdownGrace)
		defer cancel()
		if err = srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
			return errors.New("graceful shutdown timed out")
		}
		err = <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func sourceManager(c config.Config) (*sources.Manager, error) {
	return sources.New(sources.Options{Directory: c.Sources.Directory, Repo: c.Sources.Repo, Token: c.Env(c.Sources.GitHubTokenEnv), AllowEmpty: c.Sources.AllowEmpty, MaxZipBytes: c.Sources.MaxZipBytes, MaxFileBytes: c.Sources.MaxFileBytes, MaxTotalBytes: c.Sources.MaxTotalBytes, MaxFiles: c.Sources.MaxFiles, ReserveBytes: c.Sources.ReserveBytes})
}

func identityHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/ready" {
			w.Header().Set("X-Cyapi-PID", fmt.Sprint(os.Getpid()))
			w.Header().Set("X-Cyapi-Version", version)
		}
		next.ServeHTTP(w, r)
	})
}
