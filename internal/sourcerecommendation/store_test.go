package sourcerecommendation

import (
	"context"
	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sources"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	root := t.TempDir()
	generation := "g-20261113000000000000-0123456789abcdef"
	dir := filepath.Join(root, "versions", generation)
	os.MkdirAll(filepath.Join(dir, "files"), 0700)
	data := []byte("/* @name fixture */\r\nconst a=1;\r\n")
	hash := digest(data)
	manifest := sources.Manifest{Format: 1, Generation: generation, Repo: "guoyue2010/lxmusic-", ArchiveSHA256: digest([]byte("archive")), ConfigSHA256: digest([]byte("config")), Files: []sources.File{{Path: "test.js", SHA256: hash, Size: int64(len(data))}}}
	b, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600)
	os.WriteFile(filepath.Join(dir, ".read.lock"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "files", "test.js"), data, 0600)
	b, _ = json.Marshal(map[string]any{"format": 1, "generation": generation})
	os.WriteFile(filepath.Join(root, "current.json"), b, 0600)
	m, e := sources.New(sources.DefaultOptions(root))
	if e != nil {
		t.Fatal(e)
	}
	return New(t.TempDir(), root, m, nil), generation, hash
}

func TestConcurrentPublishAndHistoryRetention(t *testing.T) {
	s, g, hash := fixture(t)
	ctx := context.Background()
	in := PublishInput{Generation: g, Path: "test.js", SHA256: hash, Note: "concurrent"}
	start := make(chan struct{})
	results := make(chan error, 2)
	// Separate Store instances must serialize through the filesystem lock.
	other := New(s.Directory, filepath.Dir(s.ArchiveDirectory), s.Sources, nil)
	for _, store := range []*Store{s, other} {
		go func(store *Store) {
			<-start
			_, _, e := store.Publish(ctx, in, 1)
			results <- e
		}(store)
	}
	close(start)
	won, conflicted := 0, 0
	for range 2 {
		switch e := <-results; {
		case e == nil:
			won++
		case errors.Is(e, ErrConflict):
			conflicted++
		default:
			t.Fatal(e)
		}
	}
	if won != 1 || conflicted != 1 {
		t.Fatalf("won=%d conflicted=%d", won, conflicted)
	}
	first, _ := s.Current()
	// Publish unique content revisions so the retention boundary is exercised.
	snapshot, e := s.Sources.Current()
	if e != nil {
		t.Fatal(e)
	}
	manifest := snapshot.Manifest
	snapshot.Close()
	dir := filepath.Join(filepath.Dir(s.ArchiveDirectory), "versions", g)
	current := first
	for i := 0; i < 22; i++ {
		data := []byte{byte(i), 'j', 's'}
		manifest.Files[0].SHA256 = digest(data)
		manifest.Files[0].Size = int64(len(data))
		record, _ := json.Marshal(manifest)
		if e := os.WriteFile(filepath.Join(dir, "manifest.json"), record, 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dir, "files", "test.js"), data, 0600); e != nil {
			t.Fatal(e)
		}
		in.ExpectedReleaseID, in.SHA256 = current.ReleaseID, digest(data)
		current, _, e = s.Publish(ctx, in, 1)
		if e != nil {
			t.Fatal(i, e)
		}
	}
	_, history, e := s.History(ctx)
	if e != nil || len(history) != 20 || history[0].ReleaseID != current.ReleaseID {
		t.Fatal(len(history), e)
	}
	if _, e := os.Stat(filepath.Join(s.Directory, "releases", first.ReleaseID+".json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("old release retained", e)
	}
	if _, _, e := s.Script(ctx, hash); e != nil {
		t.Fatal("24h pinned grace", e)
	}
	old := time.Now().Add(-25 * time.Hour)
	if e := os.Chtimes(filepath.Join(s.ArchiveDirectory, hash+".js"), old, old); e != nil {
		t.Fatal(e)
	}
	if e := s.prune(current); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.Script(ctx, hash); !errors.Is(e, ErrGone) {
		t.Fatal("expired unreferenced script", e)
	}
	if _, _, e := s.Script(ctx, current.ScriptSHA256); e != nil {
		t.Fatal("current content pruned", e)
	}
}

func TestDescriptorETagChangesWithDiagnosticEvidence(t *testing.T) {
	s, g, hash := fixture(t)
	s.Checks = &sourcecheck.Service{Directory: t.TempDir()}
	_, version, e := s.Checks.Samples(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.Publish(context.Background(), PublishInput{Generation: g, Path: "test.js", SHA256: hash, Note: "publish"}, 1); e != nil {
		t.Fatal(e)
	}
	call := func(etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/lx/recommendation", nil)
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		s.description(w, r)
		return w
	}
	before := call("").Header().Get("ETag")
	report := sourcecheck.Report{Format: 1, RunID: "test-run", ProbeVersion: sourcecheck.ProbeVersion, SampleSetVersion: version, Results: []sourcecheck.Result{{ScriptSHA256: hash, SampleID: "test", Platform: "qq", CheckedAt: time.Now().UTC(), Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio"}}}}
	b, _ := json.Marshal(report)
	if e := atomic(filepath.Join(s.Checks.Directory, "runs", "test-run.json"), b); e != nil {
		t.Fatal(e)
	}
	w := call(before)
	if w.Code != 200 || w.Header().Get("ETag") == before {
		t.Fatal("stale evidence ETag", w.Code, w.Body.String())
	}
	r, _ := s.Current()
	if h := s.Describe(r).Health["qq"]; h == nil || h.Recognized != 1 {
		t.Fatal("missing evidence", h)
	}
	if w := call(w.Header().Get("ETag")); w.Code != 304 {
		t.Fatal("stable ETag", w.Code)
	}
}
func TestPublishPinnedRollbackCASAndBackup(t *testing.T) {
	s, g, hash := fixture(t)
	ctx := context.Background()
	if _, e := s.Current(); !errors.Is(e, ErrUnpublished) {
		t.Fatal(e)
	}
	input := PublishInput{Generation: g, Path: "test.js", SHA256: hash, Note: "first"}
	r, reused, e := s.Publish(ctx, input, 7)
	if e != nil || reused {
		t.Fatal(e)
	}
	_, b, e := s.Script(ctx, hash)
	if e != nil || string(b) != "/* @name fixture */\r\nconst a=1;\r\n" {
		t.Fatal("original bytes", e)
	}
	if _, _, e = s.Publish(ctx, input, 7); !errors.Is(e, ErrConflict) {
		t.Fatal("stale CAS", e)
	}
	input.ExpectedReleaseID = r.ReleaseID
	again, reused, e := s.Publish(ctx, input, 7)
	if e != nil || !reused || again.ReleaseID != r.ReleaseID {
		t.Fatal("same hash", e)
	}
	input.Generation = "changed"
	if _, _, e = s.Publish(ctx, input, 7); !errors.Is(e, ErrConflict) {
		t.Fatal("generation", e)
	}
	// Publish another exact source revision, then roll back from the archive.
	other := []byte("second-script")
	otherHash := digest(other)
	snapshot, e := s.Sources.Current()
	if e != nil {
		t.Fatal(e)
	}
	manifest := snapshot.Manifest
	snapshot.Close()
	manifest.Files = append([]sources.File{{Path: "second.js", SHA256: otherHash, Size: int64(len(other))}}, manifest.Files...)
	sourceDir := filepath.Join(filepath.Dir(s.ArchiveDirectory), "versions", g)
	record, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(sourceDir, "manifest.json"), record, 0600)
	os.WriteFile(filepath.Join(sourceDir, "files", "second.js"), other, 0600)
	next, _, e := s.Publish(ctx, PublishInput{ExpectedReleaseID: r.ReleaseID, Generation: g, Path: "second.js", SHA256: otherHash, Note: "switch"}, 8)
	if e != nil {
		t.Fatal(e)
	}
	if next.ReleaseID == r.ReleaseID || next.ScriptSHA256 != otherHash {
		t.Fatal(next)
	}
	if _, old, e := s.Script(ctx, hash); e != nil || digest(old) != hash {
		t.Fatal("pinned download changed", e)
	}
	rollback, _, e := s.Publish(ctx, PublishInput{ExpectedReleaseID: next.ReleaseID, SHA256: hash, FromReleaseID: r.ReleaseID, Note: "rollback"}, 9)
	if e != nil || rollback.ReleaseID == r.ReleaseID || rollback.PreviousReleaseID != next.ReleaseID {
		t.Fatal("rollback", e)
	}
	dest := t.TempDir()
	hashes, e := s.Backup(ctx, dest)
	if e != nil {
		t.Fatal(e)
	}
	for rel, want := range hashes {
		b, e := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if e != nil || digest(b) != want {
			t.Fatal(rel, e)
		}
	}
	restored := &Store{Directory: filepath.Join(dest, "source-recommendations"), ArchiveDirectory: filepath.Join(dest, "published")}
	current, _, e := restored.History(ctx)
	if e != nil || current.ReleaseID != rollback.ReleaseID {
		t.Fatal("restored", e)
	}
	// Orphan immutable records from a failed pointer update never appear as published history.
	orphan := rollback
	orphan.ReleaseID = "r-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b, _ = json.Marshal(orphan)
	atomic(filepath.Join(s.Directory, "releases", orphan.ReleaseID+".json"), b)
	_, history, e := s.History(ctx)
	if e != nil || len(history) != 3 {
		t.Fatal("orphan history", len(history), e)
	}
}
func TestRecommendationHTTPAndCorruptData(t *testing.T) {
	s, g, hash := fixture(t)
	r, _, e := s.Publish(context.Background(), PublishInput{Generation: g, Path: "test.js", SHA256: hash, Note: "publish"}, 1)
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	sources.NewHandler(s.Sources, slog.Default()).WithRecommendation(s).Register(mux)
	s.Register(mux)
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	w := call("/lx/sources")
	var body struct{ Sources []map[string]string }
	json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || len(body.Sources) != 2 || body.Sources[0]["kind"] != "recommendation" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("/lx/script?path=" + CatalogPath + "&sha256=" + hash)
	if w.Code != 200 || w.Header().Get("ETag") != `"`+hash+`"` {
		t.Fatal(w.Code)
	}
	etag := call("/lx/recommendation").Header().Get("ETag")
	req := httptest.NewRequest("GET", "/lx/recommendation", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
	// Archived download remains independent of the current source snapshot.
	os.Remove(filepath.Join(filepath.Dir(s.ArchiveDirectory), "current.json"))
	w = call("/lx/sources")
	if w.Code != 200 {
		t.Fatal("recommendation lost with source snapshot", w.Code)
	}
	atomic(filepath.Join(s.Directory, "current.json"), []byte(`{"format":1,"releaseId":"broken"}`))
	if _, e = s.Current(); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if w = call("/lx/recommendation"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	// Immutable hash downloads are unaffected by a broken mutable recommendation pointer.
	if w = call("/lx/script?path=" + CatalogPath + "&sha256=" + hash); w.Code != 200 {
		t.Fatal(w.Code)
	}
	_ = r
}
