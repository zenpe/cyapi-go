package sources

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/text/encoding/simplifiedchinese"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type zipEntry struct {
	name, body string
	mode       os.FileMode
	legacy     bool
}

func archive(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, v := range entries {
		h := &zip.FileHeader{Name: v.name, Method: zip.Store, NonUTF8: v.legacy}
		if v.mode != 0 {
			h.SetMode(v.mode)
		}
		f, e := w.CreateHeader(h)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write([]byte(v.body)); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(r *http.Request, status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}

type upstream struct {
	zip            []byte
	assetID        int64
	requests       int
	fail           bool
	truncate       bool
	custom         any
	seenURL        string
	digestOverride string
}

func (u *upstream) client(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		u.requests++
		if u.fail {
			return reply(r, 403, []byte("secret-provider-details")), nil
		}
		if r.URL.Host == "api.github.com" {
			if r.Header.Get("Authorization") != "Bearer test-private-token" {
				t.Error("API token missing")
			}
			if u.custom != nil {
				body, _ := json.Marshal(u.custom)
				return reply(r, 200, body), nil
			}
			digest := sha256.Sum256(u.zip)
			hash := "sha256:" + hex.EncodeToString(digest[:])
			if u.digestOverride != "" {
				hash = u.digestOverride
			}
			a := map[string]any{"id": u.assetID, "name": "first.ZIP", "size": len(u.zip), "browser_download_url": "https://github.com/guoyue2010/lxmusic-/releases/download/v1/first.zip", "digest": hash, "updated_at": "2026-10-29T00:00:00Z"}
			body, _ := json.Marshal([]any{map[string]any{"id": 3, "draft": true}, map[string]any{"id": 2, "prerelease": true}, map[string]any{"id": 1, "tag_name": "v1", "assets": []any{map[string]any{"name": "readme.txt"}, a, map[string]any{"id": 99, "name": "second.zip", "browser_download_url": "https://github.com/guoyue2010/lxmusic-/releases/download/v1/second.zip"}}}})
			return reply(r, 200, body), nil
		}
		u.seenURL = r.URL.String()
		if r.Header.Get("Authorization") != "" {
			t.Error("token leaked to download host")
		}
		data := u.zip
		if u.truncate && len(data) > 0 {
			data = data[:len(data)/2]
		}
		return reply(r, 200, data), nil
	})}
}
func fixture(t *testing.T, data []byte) (*Manager, *upstream) {
	t.Helper()
	u := &upstream{zip: data, assetID: 10}
	o := DefaultOptions(t.TempDir())
	o.Token = "test-private-token"
	o.MaxZipBytes = 2 << 20
	o.MaxFileBytes = 1 << 20
	o.MaxTotalBytes = 4 << 20
	o.ReserveBytes = 0
	o.Client = u.client(t)
	m, e := New(o)
	if e != nil {
		t.Fatal(e)
	}
	return m, u
}
func syncOK(t *testing.T, m *Manager) Report {
	t.Helper()
	out, e := m.Sync(context.Background(), false, false)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func emptyStaging(t *testing.T, m *Manager) {
	t.Helper()
	entries, e := os.ReadDir(filepath.Join(m.o.Directory, "staging"))
	if e != nil || len(entries) != 0 {
		t.Fatal("staging leak", entries, e)
	}
}
func handler(m *Manager) http.Handler {
	mux := http.NewServeMux()
	NewHandler(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	return mux
}
func request(t *testing.T, h http.Handler, target string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	r.Host = "nas.local:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s: %d want %d: %s", target, w.Code, want, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable response")
	}
	return w
}
func TestReleaseSyncHTTPAndRollback(t *testing.T) {
	legacy, _ := simplifiedchinese.GB18030.NewEncoder().String("包/中文源.js")
	data := archive(t, zipEntry{"./包//nested/a.js", "// first script", 0, false}, zipEntry{legacy, "// Chinese", 0, true}, zipEntry{"包/readme.txt", "notes", 0, false})
	m, u := fixture(t, data)
	h := handler(m)
	request(t, h, "/lx/sources", 503)
	first := syncOK(t, m)
	if first.Files != 3 || first.Unchanged || !strings.HasSuffix(u.seenURL, "/first.zip") {
		t.Fatal(first, u.seenURL)
	}
	s, e := m.Current()
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Verify(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(m.o.Directory, "versions", first.Generation, "files", "readme.txt")); e != nil {
		t.Fatal("non-JS file not mirrored", e)
	}
	w := request(t, h, "/lx/sources", 200)
	var list struct {
		Sources []struct{ Path, Name, Directory, DownloadURL string }
	}
	if e = json.Unmarshal(w.Body.Bytes(), &list); e != nil {
		t.Fatal(e)
	}
	if len(list.Sources) != 2 || list.Sources[0].Path != "nested/a.js" || list.Sources[1].Path != "中文源.js" || list.Sources[0].Directory != "nested" || !strings.HasPrefix(list.Sources[0].DownloadURL, "http://nas.local:8080/") {
		t.Fatal(list)
	}
	// Switching between LAN and public HTTPS must not reuse a configured origin.
	for _, origin := range []string{"http://192.168.3.100:8080", "https://music.example.com", "http://[::1]:8080"} {
		req := httptest.NewRequest("GET", origin+"/lx/sources", nil)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		if e = json.Unmarshal(response.Body.Bytes(), &list); e != nil {
			t.Fatal(e)
		}
		for _, source := range list.Sources {
			u, err := url.Parse(source.DownloadURL)
			if err != nil || !strings.HasPrefix(source.DownloadURL, origin+"/lx/script?") || u.Query().Get("path") != source.Path {
				t.Fatal("download URL does not follow request", source.DownloadURL)
			}
			request(t, h, u.RequestURI(), 200)
		}
	}
	w = request(t, h, "/lx/script?path=nested%2Fa.js", 200)
	if w.Body.String() != "// first script" || w.Header().Get("Content-Type") != "application/javascript" || w.Header().Get("ETag") == "" {
		t.Fatal(w.Body.String(), w.Header())
	}
	request(t, h, "/lx/script?path="+url.QueryEscape("中文源.js"), 200)
	etag := w.Header().Get("ETag")
	rangeRequest := httptest.NewRequest("GET", "/lx/script?path=nested%2Fa.js", nil)
	rangeRequest.Header.Set("Range", "bytes=0-1")
	rangeResponse := httptest.NewRecorder()
	h.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != 206 || rangeResponse.Body.String() != "//" {
		t.Fatal("range response", rangeResponse.Code, rangeResponse.Body.String())
	}
	conditional := httptest.NewRequest("GET", "/lx/script?path=nested%2Fa.js", nil)
	conditional.Header.Set("If-None-Match", etag)
	unchangedResponse := httptest.NewRecorder()
	h.ServeHTTP(unchangedResponse, conditional)
	if unchangedResponse.Code != 304 || unchangedResponse.Body.Len() != 0 {
		t.Fatal("conditional download", unchangedResponse.Code)
	}

	for _, q := range []string{"path=../config.yaml", "path=%2Fsecret.js", "path=a.js&path=b.js", "path=x%5Cy.js", "path=readme.txt", "path=CON.js", "path=a.js%00"} {
		request(t, h, "/lx/script?"+q, 400)
	}
	request(t, h, "/lx/script?path=missing.js", 404)
	request(t, h, "/lx/script?path=..%252fsecret.js", 404)
	unchanged := syncOK(t, m)
	if !unchanged.Unchanged || unchanged.Generation != first.Generation {
		t.Fatal(unchanged)
	}
	emptyStaging(t, m)
	// An older snapshot remains readable while a newer ZIP is published.
	entry := s.Manifest.Files[0]
	oldFile, e := s.Open(entry)
	if e != nil {
		t.Fatal(e)
	}
	defer oldFile.Close()
	u.assetID = 11
	u.zip = archive(t, zipEntry{"new.js", "// new script", 0, false})
	second := syncOK(t, m)
	if second.Generation == first.Generation {
		t.Fatal("not updated")
	}
	request(t, h, "/lx/script?path=nested%2Fa.js", 404)
	request(t, h, "/lx/script?path=new.js", 200)
	oldBytes, e := io.ReadAll(oldFile)
	if e != nil || string(oldBytes) != "// first script" {
		t.Fatal(string(oldBytes), e)
	}
	if _, e = m.Rollback(context.Background(), first.Generation); e != nil {
		t.Fatal(e)
	}
	request(t, h, "/lx/script?path=nested%2Fa.js", 200)
	if _, e = m.Rollback(context.Background(), "../escape"); e == nil {
		t.Fatal("rollback escaped root")
	}
}
func TestRepairLocalDrift(t *testing.T) {
	m, _ := fixture(t, archive(t, zipEntry{"script.js", "// GOOD", 0, false}))
	current := syncOK(t, m)
	for _, drift := range []string{"content", "missing-file", "missing-pointer", "missing-manifest", "extra-file", "invalid-pointer"} {
		t.Run(drift, func(t *testing.T) {
			base := filepath.Join(m.o.Directory, "versions", current.Generation)
			switch drift {
			case "content":
				if e := os.WriteFile(filepath.Join(base, "files", "script.js"), []byte("// BAD!"), 0600); e != nil {
					t.Fatal(e)
				}
				request(t, handler(m), "/lx/script?path=script.js", 503)
			case "missing-file":
				os.Remove(filepath.Join(base, "files", "script.js"))
				request(t, handler(m), "/lx/sources", 503)
			case "missing-pointer":
				os.Remove(filepath.Join(m.o.Directory, "current.json"))
			case "missing-manifest":
				os.Remove(filepath.Join(base, "manifest.json"))
			case "extra-file":
				os.WriteFile(filepath.Join(base, "files", "extra.js"), []byte("extra"), 0600)
			case "invalid-pointer":
				os.WriteFile(filepath.Join(m.o.Directory, "current.json"), []byte(`{"format":1,"generation":"../../escape"}`), 0600)
			}
			next := syncOK(t, m)
			if next.Unchanged || next.Generation == current.Generation {
				t.Fatal("drift not repaired", next)
			}
			if _, e := m.Status(context.Background()); e != nil {
				t.Fatal(e)
			}
			current = next
			emptyStaging(t, m)
		})
	}
}
func TestFailedDownloadAndProcessRecovery(t *testing.T) {
	m, u := fixture(t, archive(t, zipEntry{"script.js", "// old", 0, false}))
	old := syncOK(t, m)
	u.truncate = true
	for i := 0; i < 10; i++ {
		if _, e := m.Sync(context.Background(), false, false); e == nil {
			t.Fatal("truncated download accepted")
		}
		emptyStaging(t, m)
		s, e := m.Current()
		if e != nil || s.Manifest.Generation != old.Generation {
			t.Fatal("failed download changed current", e)
		}
		s.Close()
	}
	u.truncate = false
	for i := 0; i < 10; i++ {
		exe, e := os.Executable()
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(exe, "-test.run=^TestSourceCrashHelper$")
		cmd.Env = append(os.Environ(), "CYAPI_SOURCE_CRASH_TEST="+m.o.Directory)
		out, e := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(e, &exit) || exit.ExitCode() != 23 {
			t.Fatal(string(out), e)
		}
		if _, e = m.Sync(context.Background(), false, false); e != nil {
			t.Fatal("crashed lock or staging not reclaimed", e)
		}
		emptyStaging(t, m)
	}
	// Unknown/linked leftovers stop before any network request or new staging allocation.
	path := filepath.Join(m.o.Directory, "staging", "unmanaged")
	os.WriteFile(path, []byte("do not remove"), 0600)
	before := u.requests
	if _, e := m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("unmanaged staging ignored")
	}
	if before != u.requests {
		t.Fatal("download started before cleanup")
	}
	os.Remove(path)
	m.free = func(string) (uint64, error) { return 0, nil }
	before = u.requests
	if _, e := m.Sync(context.Background(), false, false); e == nil || u.requests != before {
		t.Fatal("space check did not stop download")
	}
}
func TestSourceCrashHelper(t *testing.T) {
	directory := os.Getenv("CYAPI_SOURCE_CRASH_TEST")
	if directory == "" {
		return
	}
	o := DefaultOptions(directory)
	m, _ := New(o)
	_, root, e := m.acquire()
	if e != nil {
		os.Exit(24)
	}
	root.Mkdir("staging/attempt-crashed", 0700)
	f, e := root.OpenFile("staging/attempt-crashed/partial.zip", os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		os.Exit(25)
	}
	f.Write([]byte("interrupted"))
	os.Exit(23)
}
func TestSourceProcessLock(t *testing.T) {
	m, _ := fixture(t, archive(t, zipEntry{"a.js", "//a", 0, false}))
	lock, root, e := m.acquire()
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	defer func() { unlockFile(lock); lock.Close() }()
	if _, e = m.Sync(context.Background(), false, false); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestSourceLockHelper$")
	cmd.Env = append(os.Environ(), "CYAPI_SOURCE_LOCK_TEST="+m.o.Directory)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
}
func TestSourceLockHelper(t *testing.T) {
	directory := os.Getenv("CYAPI_SOURCE_LOCK_TEST")
	if directory == "" {
		return
	}
	o := DefaultOptions(directory)
	m, _ := New(o)
	if _, _, e := m.acquire(); !errors.Is(e, ErrBusy) {
		t.Fatal("cross-process lock not held", e)
	}
}
func TestArchiveRejectionAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []zipEntry
	}{
		{"traversal", []zipEntry{{"../escape.js", "x", 0, false}}}, {"absolute", []zipEntry{{"/escape.js", "x", 0, false}}}, {"drive", []zipEntry{{"C:/escape.js", "x", 0, false}}}, {"backslash-traversal", []zipEntry{{"a\\..\\escape.js", "x", 0, false}}},
		{"symlink", []zipEntry{{"root/a.js", "outside", os.ModeSymlink | 0777, false}}}, {"duplicate", []zipEntry{{"root/a.js", "a", 0, false}, {"root/a.js", "b", 0, false}}}, {"case-collision", []zipEntry{{"root/A.js", "a", 0, false}, {"root/a.js", "b", 0, false}}}, {"prefix-collision", []zipEntry{{"root/a.js", "a", 0, false}, {"root/a.js/b.js", "b", 0, false}}}, {"no-scripts", []zipEntry{{"readme.txt", "notes", 0, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := fixture(t, archive(t, tc.entries...))
			if _, e := m.Sync(context.Background(), false, false); e == nil {
				t.Fatal("unsafe archive accepted")
			}
			emptyStaging(t, m)
			if _, e := os.Stat(filepath.Join(m.o.Directory, "current.json")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("failed archive published")
			}
		})
	}
	for _, limit := range []string{"zip", "file", "total", "count"} {
		t.Run(limit, func(t *testing.T) {
			data := archive(t, zipEntry{"a.js", "aaaa", 0, false}, zipEntry{"b.js", "bbbb", 0, false})
			m, _ := fixture(t, data)
			switch limit {
			case "zip":
				m.o.MaxZipBytes = int64(len(data) - 1)
			case "file":
				m.o.MaxFileBytes = 3
			case "total":
				m.o.MaxTotalBytes = 7
			case "count":
				m.o.MaxFiles = 1
			}
			if _, e := m.Sync(context.Background(), false, false); e == nil {
				t.Fatal("limit ignored")
			}
			emptyStaging(t, m)
		})
	}
	m, u := fixture(t, archive(t, zipEntry{"a.js", "PAYLOAD-CRC", 0, false}))
	i := bytes.Index(u.zip, []byte("PAYLOAD-CRC"))
	u.zip[i] = 'X'
	if _, e := m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("corrupt ZIP checksum accepted")
	}
	emptyStaging(t, m)
}
func TestDryForceConfigAndPublishFailure(t *testing.T) {
	m, u := fixture(t, archive(t, zipEntry{"a.js", "//a", 0, false}))
	dry, e := m.Sync(context.Background(), false, true)
	if e != nil || !dry.DryRun {
		t.Fatal(dry, e)
	}
	if _, e = m.Current(); e == nil {
		t.Fatal("dry run published")
	}
	first := syncOK(t, m)
	forced, e := m.Sync(context.Background(), true, false)
	if e != nil || forced.Unchanged || forced.Generation == first.Generation {
		t.Fatal(forced, e)
	}
	// A token change must not alter the output configuration digest.
	original := configHash(m.o)
	m.o.Token = "different"
	if configHash(m.o) != original {
		t.Fatal("token included in config digest")
	}
	m.o.Token = "test-private-token"
	m.o.AllowEmpty = true
	changed := syncOK(t, m)
	if changed.Unchanged {
		t.Fatal("config change ignored")
	}
	oldReplace := m.replace
	m.replace = func(string, string) error { return errors.New("injected pointer publication failure") }
	u.assetID++
	if _, e = m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("publish failure swallowed")
	}
	s, e := m.Current()
	if e != nil || s.Manifest.Generation != changed.Generation {
		t.Fatal("current lost on publish failure", e)
	}
	s.Close()
	m.replace = oldReplace
	emptyStaging(t, m)
	u.fail = true
	if _, e = m.Sync(context.Background(), false, false); e == nil || strings.Contains(e.Error(), "secret-provider-details") {
		t.Fatal(e)
	}
	request(t, handler(m), "/lx/script?path=a.js", 200)
}
func TestSourceRedirectsRetriesAndLatestRules(t *testing.T) {
	for _, target := range []string{"https://evil.example/payload", "http://github.com/insecure", "https://api.github.com@evil.example/x"} {
		t.Run(target, func(t *testing.T) {
			o := DefaultOptions(t.TempDir())
			calls := 0
			o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				response := reply(r, 302, nil)
				response.Header.Set("Location", target)
				return response, nil
			})}
			m, _ := New(o)
			if _, e := m.get(context.Background(), "https://api.github.com/releases", true); e == nil || calls != 1 {
				t.Fatal("redirect allowed", calls, e)
			}
		})
	}
	o := DefaultOptions(t.TempDir())
	o.Token = "token"
	calls := 0
	o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			response := reply(r, 302, nil)
			response.Header.Set("Location", "https://release-assets.githubusercontent.com/file")
			return response, nil
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatal("redirect leaked token")
		}
		return reply(r, 200, []byte("ok")), nil
	})}
	m, _ := New(o)
	response, e := m.get(context.Background(), "https://api.github.com/releases", true)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	calls = 0
	o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			response := reply(r, 429, nil)
			response.Header.Set("Retry-After", "0")
			return response, nil
		}
		return reply(r, 200, []byte("[]")), nil
	})}
	m, _ = New(o)
	response, e = m.get(context.Background(), "https://api.github.com/releases", true)
	if e != nil || calls != 3 {
		t.Fatal(calls, e)
	}
	response.Body.Close()
	m, u := fixture(t, archive(t, zipEntry{"a.js", "a", 0, false}))
	u.custom = []any{map[string]any{"id": 2, "assets": []any{}}, map[string]any{"id": 1, "assets": []any{map[string]any{"name": "old.zip"}}}}
	if _, _, e = m.latest(context.Background()); e == nil {
		t.Fatal("fell back to older release")
	}
	u.custom = nil
	u.digestOverride = "sha256:" + strings.Repeat("0", 64)
	if _, e = m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("upstream digest ignored")
	}
}
func TestGenerationGCProtectsReaders(t *testing.T) {
	m, _ := fixture(t, archive(t, zipEntry{"a.js", "//a", 0, false}))
	first := syncOK(t, m)
	reader, e := m.Current()
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if _, e = m.Sync(context.Background(), true, false); e != nil {
			t.Fatal(e)
		}
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if e = os.Chtimes(filepath.Join(m.o.Directory, "versions", first.Generation), oldTime, oldTime); e != nil {
		t.Fatal(e)
	}
	// GC is invoked by publication, and must skip a generation with a live shared lease.
	syncOKForce := func() {
		t.Helper()
		if _, e := m.Sync(context.Background(), true, false); e != nil {
			t.Fatal(e)
		}
	}
	syncOKForce()
	if _, e = os.Stat(filepath.Join(m.o.Directory, "versions", first.Generation)); e != nil {
		t.Fatal("active reader generation removed", e)
	}
	if e = reader.Verify(context.Background()); e != nil {
		t.Fatal(e)
	}
	reader.Close()
	syncOKForce()
	if _, e = os.Stat(filepath.Join(m.o.Directory, "versions", first.Generation)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("expired generation not reclaimed", e)
	}
}

func TestEmptyZIPContextAndStreamingSpace(t *testing.T) {
	m, u := fixture(t, archive(t))
	if _, e := m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("empty ZIP accepted by default")
	}
	m.o.AllowEmpty = true
	out := syncOK(t, m)
	if out.Files != 0 {
		t.Fatal(out)
	}
	request(t, handler(m), "/lx/sources", 200)
	u.zip = archive(t, zipEntry{"a.js", "// payload", 0, false})
	u.assetID++
	calls := 0
	m.free = func(string) (uint64, error) {
		calls++
		if calls == 1 {
			return ^uint64(0), nil
		}
		return 0, nil
	}
	if _, e := m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("reserve checked only before download")
	}
	emptyStaging(t, m)
	m.free = diskFree
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := m.Sync(ctx, false, false); e == nil {
		t.Fatal("cancelled sync succeeded")
	}
	emptyStaging(t, m)
}
func TestSourceLinksAndTraversal(t *testing.T) {
	m, _ := fixture(t, archive(t, zipEntry{"a.js", "// original", 0, false}))
	out := syncOK(t, m)
	file := filepath.Join(m.o.Directory, "versions", out.Generation, "files", "a.js")
	outside := filepath.Join(t.TempDir(), "outside.js")
	if e := os.WriteFile(outside, []byte("// outside!"), 0600); e != nil {
		t.Fatal(e)
	}
	os.Remove(file)
	if e := os.Symlink(outside, file); e != nil {
		t.Skip("OS does not permit creating symlinks:", e)
	}
	request(t, handler(m), "/lx/script?path=a.js", 503)
	if _, e := m.Status(context.Background()); e == nil {
		t.Fatal("linked file verified")
	}
	repaired := syncOK(t, m)
	if repaired.Generation == out.Generation {
		t.Fatal("linked generation not rebuilt")
	}
	link := filepath.Join(m.o.Directory, "staging", "attempt-linked")
	if e := os.Symlink(filepath.Dir(outside), link); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Sync(context.Background(), false, false); e == nil {
		t.Fatal("linked cleanup target followed")
	}
	if b, e := os.ReadFile(outside); e != nil || string(b) != "// outside!" {
		t.Fatal("cleanup escaped staging", e)
	}
}
