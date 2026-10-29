package main

import (
	"context"
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
	"cyapi-go/internal/mail"
	"cyapi-go/internal/persistence"
	"cyapi-go/internal/persistence/authstore"
	"cyapi-go/internal/server"
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
		return errors.New("usage: cyapi <serve|migrate|schema-status|version> [--config path]")
	}
	command := args[0]
	if command == "version" {
		if len(args) != 1 {
			return errors.New("version takes no arguments")
		}
		fmt.Printf("cyapi %s commit=%s go=%s %s/%s\n", version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if command != "serve" && command != "migrate" && command != "schema-status" {
		return errors.New("unknown command; implemented commands: serve, migrate, schema-status, version")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
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
	authRoutes := auth.NewHandler(accounts, server.ClientIP, log)
	listener, err := net.Listen("tcp", c.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return serveListener(ctx, c.Server, db, log, listener, authRoutes.Register)
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
