package main

import (
	"context"
	"cyapi-go/internal/config"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	for _, args := range [][]string{nil, {"sync-sources"}, {"version", "extra"}} {
		if run(args, slog.Default()) == nil {
			t.Fatalf("accepted unimplemented/invalid command %v", args)
		}
	}
}
