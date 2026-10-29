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
	"runtime"
	"syscall"
	"time"

	"cyapi-go/internal/auth"
	"cyapi-go/internal/config"
	"cyapi-go/internal/library"
	"cyapi-go/internal/mail"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/persistence/librarystore"
	"cyapi-go/internal/server"
	"cyapi-go/internal/sources"
)

var version = "dev"
var commit = "unknown"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("command failed", "error", err.Error())
		os.Exit(1)
	}
}
func run(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return errors.New("usage: cyapi <serve|migrate|schema-status|sync-sources|sources|version> [--config path]")
	}
	command := args[0]
	if command == "version" {
		if len(args) != 1 {
			return errors.New("version takes no arguments")
		}
		fmt.Printf("cyapi %s commit=%s go=%s %s/%s\n", version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if command != "serve" && command != "migrate" && command != "schema-status" && command != "sync-sources" && command != "sources" {
		return errors.New("unknown command; implemented commands: serve, migrate, schema-status, sync-sources, sources, version")
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
	if command == "sync-sources" {
		force = flags.Bool("force", false, "publish even when unchanged")
		dry = flags.Bool("dry-run", false, "download and validate without publishing")
	}
	if command == "sources" && sourceAction == "rollback" {
		generation = flags.String("generation", "", "previous source generation")
	}

	path := flags.String("config", "config.yaml", "configuration file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
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
	if command == "schema-status" {
		fmt.Println("schema is current")
		return nil
	}

	secret, err := c.Secret()
	if err != nil {
		return err
	}
	if c.Mail.Provider != "brevo" {
		return errors.New("unsupported mail provider")
	}
	mailer := mail.NewBrevo(mail.Options{APIKey: os.Getenv(c.Mail.APIKeyEnv), SenderEmail: c.Mail.SenderEmail, SenderName: c.Mail.SenderName, AppName: c.Mail.AppName})
	accounts, err := auth.New(authstore.New(db.ORM), mailer, auth.Options{Secret: secret, TokenMaxAge: c.Auth.TokenMaxAge, Logger: log})
	if err != nil {
		return err
	}
	sourceFiles, err := sourceManager(c)
	if err != nil {
		return err
	}
	sourceRoutes := sources.NewHandler(sourceFiles, c.Server.PublicBaseURL, log)
	authRoutes := auth.NewHandler(accounts, server.ClientIP, log)
	libraryRoutes := library.NewHandler(library.New(librarystore.New(db.ORM)), authRoutes.Protect, log)
	listener, err := net.Listen("tcp", c.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return serveListener(ctx, c.Server, db, log, listener, authRoutes.Register, libraryRoutes.Register, sourceRoutes.Register)
}

func serveListener(ctx context.Context, c config.Server, db server.Readiness, log *slog.Logger, listener net.Listener, register ...func(*http.ServeMux)) error {
	var err error
	srv := &http.Server{Handler: server.NewHandler(c, db, log, register...), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
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
	return sources.New(sources.Options{Directory: c.Sources.Directory, Repo: c.Sources.Repo, Token: os.Getenv(c.Sources.GitHubTokenEnv), AllowEmpty: c.Sources.AllowEmpty, MaxZipBytes: c.Sources.MaxZipBytes, MaxFileBytes: c.Sources.MaxFileBytes, MaxTotalBytes: c.Sources.MaxTotalBytes, MaxFiles: c.Sources.MaxFiles, ReserveBytes: c.Sources.ReserveBytes})
}
