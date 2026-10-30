package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func updateFixture(t *testing.T, content []byte) (releaseUpdater, *updateRelease, *string, func() int, *sync.Mutex) {
	t.Helper()
	digest := sha256.Sum256(content)
	sum := hex.EncodeToString(digest[:]) + "  cyapi-linux-amd64\n"
	release := updateRelease{Tag: "v0.0.4", Assets: []updateAsset{{Name: "cyapi-linux-amd64", Size: int64(len(content)), Digest: "sha256:" + hex.EncodeToString(digest[:])}, {Name: "SHA256SUMS", Size: int64(len(sum))}}}
	downloads := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/releases/latest":
			_ = json.NewEncoder(w).Encode(release)
		case "/v0.0.4/SHA256SUMS":
			fmt.Fprint(w, sum)
		case "/v0.0.4/cyapi-linux-amd64":
			downloads++
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return releaseUpdater{client: server.Client(), apiBase: server.URL, downloadBase: server.URL}, &release, &sum, func() int { mu.Lock(); defer mu.Unlock(); return downloads }, &mu
}
func TestAutomaticDownload(t *testing.T) {
	content := []byte("release executable")
	updater, _, _, downloads, _ := updateFixture(t, content)
	root := t.TempDir()
	file, digest, tag, e := updater.downloadLatest(context.Background(), root, "v0.0.3", "amd64")
	if e != nil || tag != "v0.0.4" || digest == "" || downloads() != 1 {
		t.Fatal(file, tag, e)
	}
	data, e := os.ReadFile(file)
	if e != nil || string(data) != string(content) {
		t.Fatal(e, string(data))
	}
	for _, installed := range []string{"v0.0.4", "v0.0.5", "v0.1.0"} {
		got, _, _, e := updater.downloadLatest(context.Background(), root, installed, "amd64")
		if e != nil || got != "" || downloads() != 1 {
			t.Fatal("up-to-date release downloaded", installed, e)
		}
	}
}
func TestAutomaticDownloadRejectsCorruption(t *testing.T) {
	for _, test := range []string{"bad checksum", "wrong size", "missing checksum", "duplicate checksum", "missing asset", "duplicate asset", "prerelease", "draft", "invalid version", "unsupported architecture"} {
		t.Run(test, func(t *testing.T) {
			updater, release, sums, _, mu := updateFixture(t, []byte("binary"))
			arch := "amd64"
			mu.Lock()
			switch test {
			case "bad checksum":
				*sums = strings.Repeat("0", 64) + "  cyapi-linux-amd64\n"
				release.Assets[0].Digest = ""
			case "wrong size":
				release.Assets[0].Size++
			case "missing checksum":
				*sums = strings.Repeat("0", 64) + "  another-binary\n"
			case "duplicate checksum":
				*sums += *sums
			case "missing asset":
				release.Assets = release.Assets[:1]
			case "duplicate asset":
				release.Assets = append(release.Assets, release.Assets[0])
			case "prerelease":
				release.Prerelease = true
			case "draft":
				release.Draft = true
			case "invalid version":
				release.Tag = "../../evil"
			case "unsupported architecture":
				arch = "386"
			}
			mu.Unlock()
			root := t.TempDir()
			_, _, _, e := updater.downloadLatest(context.Background(), root, "v0.0.3", arch)
			if e == nil {
				t.Fatal("invalid release accepted")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("partial download retained", entries)
			}
		})
	}
}
func TestAutomaticDownloadBoundsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2000000")
		w.WriteHeader(200)
	}))
	defer server.Close()
	updater := releaseUpdater{client: server.Client(), apiBase: server.URL}
	if _, e := updater.latest(context.Background()); e == nil {
		t.Fatal("oversized metadata accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := updater.latest(ctx); e == nil {
		t.Fatal("cancellation ignored")
	}
	updater.apiBase = server.URL + "/missing"
	if _, e := updater.latest(context.Background()); e == nil {
		t.Fatal("bad response accepted")
	}
}
func TestStableVersionOrdering(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"v0.0.3", "v0.0.4", -1}, {"v1.10.0", "v1.9.0", 1}, {"v1.0.0", "v1.0.0", 0}} {
		if got, e := compareStable(tt.a, tt.b); e != nil || got != tt.want {
			t.Fatal(tt, got, e)
		}
	}
	if _, e := stableVersion("v01.0.0"); e == nil {
		t.Fatal("noncanonical version accepted")
	}
	if _, e := checksumFor("garbage  cyapi-linux-amd64\n", "cyapi-linux-amd64"); e == nil {
		t.Fatal("bad hash accepted")
	}
}
