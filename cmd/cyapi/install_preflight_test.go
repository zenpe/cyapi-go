package main

import (
	"cyapi-go/internal/config"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOccupiedPortDoesNotInitialize(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	root := filepath.Join(t.TempDir(), "fresh")
	e = installCommand([]string{"--dir", root, "--service", "none", "--listen", l.Addr().String()}, quietLog())
	if e == nil || !strings.Contains(e.Error(), "cannot listen") {
		t.Fatal(e)
	}
	if _, e = os.Stat(root); !os.IsNotExist(e) {
		t.Fatal("preflight created installation state")
	}
}
func TestManagedPathsRejectExternalFiles(t *testing.T) {
	root := t.TempDir()
	c := config.Defaults()
	c.FilePath = filepath.Join(root, "config.yaml")
	c.RuntimeDirectory = filepath.Join(root, "data")
	c.Sources.Directory = filepath.Join(root, "data/sources")
	c.Maintenance.BackupDirectory = filepath.Join(root, "backups")
	c.SecretsFile = filepath.Join(root, "secrets.env")
	c.Database.DSN = filepath.Join(root, "data/cyapi.db")
	i := installation{Root: root, Config: c.FilePath}
	if e := validateManagedPaths(i, c); e != nil {
		t.Fatal(e)
	}
	c.Database.DSN = filepath.Join(t.TempDir(), "outside.db")
	if e := validateManagedPaths(i, c); e == nil {
		t.Fatal("external data path accepted")
	}
}
