package main

import (
	"context"
	"cyapi-go/internal/config"
	"cyapi-go/internal/sources"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type readyDB struct{}

func (readyDB) CheckSchema(context.Context) error { return nil }
func TestGracefulCancellationClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveListener(ctx, config.Server{ShutdownGrace: time.Second}, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), listener)
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("listener remains open")
	}
}
func TestUnknownCommandIsExplicit(t *testing.T) {
	for _, args := range [][]string{nil, {"not-a-command"}, {"version", "extra"}} {
		if run(args, slog.Default()) == nil {
			t.Fatalf("accepted unimplemented/invalid command %v", args)
		}
	}
}

func TestSourceCommandsDoNotOpenDatabaseOrRequireAuth(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "sources")
	generation := "g-20261029120000000000-0123456789abcdef"
	base := filepath.Join(directory, "versions", generation)
	if e := os.MkdirAll(filepath.Join(base, "files"), 0700); e != nil {
		t.Fatal(e)
	}
	manifest := sources.Manifest{Format: 1, Generation: generation, Repo: "guoyue2010/lxmusic-", ArchiveSHA256: strings.Repeat("0", 64), ConfigSHA256: strings.Repeat("0", 64), Files: []sources.File{}}
	b, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(base, "manifest.json"), b, 0600)
	os.WriteFile(filepath.Join(base, ".read.lock"), nil, 0600)
	os.WriteFile(filepath.Join(directory, "current.json"), []byte(`{"format":1,"generation":"`+generation+`"}`), 0600)
	configPath := filepath.Join(root, "config.yaml")
	if e := os.WriteFile(configPath, []byte("sources:\n  directory: sources\ndatabase:\n  dsn: must-not-exist/db.sqlite\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("CYAPI_AUTH_SECRET", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if e := run([]string{"sources", "status", "--config", configPath}, log); e != nil {
		t.Fatal(e)
	}
	if e := run([]string{"sources", "rollback", "--config", configPath, "--generation", generation}, log); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(root, "must-not-exist")); !os.IsNotExist(e) {
		t.Fatal("source command opened DB", e)
	}
	if e := run([]string{"sources", "rollback", "--config", configPath}, log); e == nil {
		t.Fatal("rollback without generation succeeded")
	}
}
