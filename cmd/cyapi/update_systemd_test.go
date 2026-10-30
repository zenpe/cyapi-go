package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutomaticUpgradeSystemd(t *testing.T) {
	binary := os.Getenv("NAS_UPGRADE_TEST_BINARY")
	legacy := os.Getenv("NAS_UPGRADE_TEST_LEGACY")
	if targetOS() != "linux" || !isRoot() || binary == "" || legacy == "" {
		t.Skip("requires disposable root/systemd fixture binaries")
	}
	root, e := os.MkdirTemp("/var/lib", "cyapi-online-")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(root, 0755); e != nil {
		t.Fatal(e)
	}
	unit := unitName(root)
	t.Cleanup(func() {
		_ = systemctl("disable", "--now", unit)
		_ = os.Remove(filepath.Join("/etc/systemd/system", unit))
		_ = systemctl("daemon-reload")
		_ = os.RemoveAll(root)
	})
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := l.Addr().String()
	l.Close()
	path := filepath.Join(root, "config.yaml")
	if e = os.WriteFile(path, []byte("server:\n  listen: "+address+"\nmaintenance:\n  enabled: false\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(legacy, "install", "--dir", root, "--non-interactive", "--username", "online_user", "--email", "123456@qq.com")
	cmd.Env = append(os.Environ(), "CYAPI_INSTALL_PASSWORD=OnlineTest!123")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("legacy installation: %v %s", e, b)
	}
	originalKey, e := os.ReadFile(filepath.Join(root, "secrets.env"))
	if e != nil {
		t.Fatal(e)
	}
	content, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	var mode atomic.Int32
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			http.Error(w, "offline", 503)
			return
		}
		switch r.URL.Path {
		case "/releases/latest":
			_ = json.NewEncoder(w).Encode(updateRelease{Tag: "v0.0.4", Assets: []updateAsset{{Name: "cyapi-linux-amd64", Size: int64(len(content))}, {Name: "SHA256SUMS", Size: 100}}})
		case "/v0.0.4/SHA256SUMS":
			sum := digest
			if mode.Load() == 2 {
				sum = strings.Repeat("0", 64)
			}
			fmt.Fprintln(w, sum+"  cyapi-linux-amd64")
		case "/v0.0.4/cyapi-linux-amd64":
			downloads.Add(1)
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldFactory := newReleaseUpdater
	newReleaseUpdater = func(string) releaseUpdater {
		return releaseUpdater{client: server.Client(), apiBase: server.URL, downloadBase: server.URL}
	}
	t.Cleanup(func() { newReleaseUpdater = oldFactory })
	for _, fail := range []int32{1, 2} {
		mode.Store(fail)
		if e = upgradeCommand([]string{"--config", path}, quietLog()); e == nil {
			t.Fatal("invalid online update accepted", fail)
		}
		if !serviceActive(unit) {
			t.Fatal("download failure stopped existing service")
		}
	}
	mode.Store(0)
	if e = upgradeCommand([]string{"--config", path}, quietLog()); e != nil {
		t.Fatal("automatic upgrade", e)
	}
	i := installation{Root: root, Binary: filepath.Join(root, "cyapi"), Config: path, Unit: unit, User: "cyapi"}
	if e = waitManagedReady(i, address, "v0.0.4", 5*time.Second); e != nil {
		t.Fatal(e)
	}
	afterKey, _ := os.ReadFile(filepath.Join(root, "secrets.env"))
	if string(afterKey) != string(originalKey) {
		t.Fatal("authentication key changed")
	}
	before := downloads.Load()
	if e = upgradeCommand([]string{"--config", path}, quietLog()); e != nil {
		t.Fatal(e)
	}
	if downloads.Load() != before {
		t.Fatal("up-to-date binary downloaded again")
	}
	if !serviceActive(unit) {
		t.Fatal("up-to-date check stopped service")
	}
}
