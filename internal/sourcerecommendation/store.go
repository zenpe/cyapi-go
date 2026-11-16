// Package sourcerecommendation publishes immutable scripts behind a stable catalog entry.
package sourcerecommendation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"strings"
	"sync"
	"time"

	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sources"
)

const CatalogPath = "推荐/recommended.js"
const MaxScriptBytes = 8 << 20

var ErrBusy = errors.New("recommendation storage busy")
var ErrConflict = errors.New("recommendation or source version changed")
var ErrInvalid = errors.New("invalid recommendation input")
var ErrUnpublished = errors.New("recommendation unpublished")
var ErrUnavailable = errors.New("recommendation data unavailable")
var ErrGone = errors.New("published content expired")
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var releasePattern = regexp.MustCompile(`^r-[a-f0-9]{32}$`)

type Release struct {
	Format            int       `json:"format"`
	ReleaseID         string    `json:"releaseId"`
	PreviousReleaseID string    `json:"previousReleaseId"`
	ScriptSHA256      string    `json:"scriptSha256"`
	OriginalPath      string    `json:"originalPath"`
	OriginalName      string    `json:"originalName"`
	PublishedAt       time.Time `json:"publishedAt"`
	PublishedBy       int64     `json:"publishedBy"`
	Note              string    `json:"note"`
	ReportRunID       string    `json:"reportRunId,omitempty"`
}
type PublishInput struct {
	ExpectedReleaseID string `json:"expectedReleaseId"`
	Generation        string `json:"generation"`
	Path              string `json:"path"`
	SHA256            string `json:"sha256"`
	FromReleaseID     string `json:"fromReleaseId"`
	Note              string `json:"note"`
	ReportRunID       string `json:"reportRunId"`
}
type Store struct {
	healthMu                    sync.Mutex
	healthKey                   string
	health                      map[string]*PlatformHealth
	Directory, ArchiveDirectory string
	Sources                     *sources.Manager
	Checks                      *sourcecheck.Service
}

func New(directory, sourcesDirectory string, m *sources.Manager, c *sourcecheck.Service) *Store {
	return &Store{Directory: directory, ArchiveDirectory: filepath.Join(sourcesDirectory, "published"), Sources: m, Checks: c}
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *Store) lock(ctx context.Context) (func(), error) {
	if e := os.MkdirAll(s.Directory, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(s.Directory, "publish.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		e = lockFile(f)
		if e == nil {
			return func() { unlockFile(f); f.Close() }, nil
		}
		if !errors.Is(e, ErrBusy) {
			f.Close()
			return nil, e
		}
		select {
		case <-timeout.Done():
			f.Close()
			return nil, timeout.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func atomic(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".publish-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = replaceFile(f.Name(), path); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(path))
}
func read(path string, limit int64) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, ErrUnavailable
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, ErrUnavailable
	}
	return b, nil
}
func readJSON(path string, v any) error {
	b, e := read(path, 64<<10)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return ErrUnavailable
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrUnavailable
	}
	return nil
}
func valid(r Release) bool {
	return r.Format == 1 && releasePattern.MatchString(r.ReleaseID) && hashPattern.MatchString(r.ScriptSHA256) && !r.PublishedAt.IsZero() && r.PublishedBy > 0 && len(r.Note) <= 2000 && r.OriginalName != ""
}
func (s *Store) release(id string) (Release, error) {
	var r Release
	if !releasePattern.MatchString(id) {
		return r, ErrInvalid
	}
	if e := readJSON(filepath.Join(s.Directory, "releases", id+".json"), &r); e != nil {
		return r, e
	}
	if !valid(r) || r.ReleaseID != id {
		return r, ErrUnavailable
	}
	return r, nil
}
func (s *Store) current() (Release, error) {
	var p struct {
		Format    int    `json:"format"`
		ReleaseID string `json:"releaseId"`
	}
	e := readJSON(filepath.Join(s.Directory, "current.json"), &p)
	if errors.Is(e, os.ErrNotExist) {
		return Release{}, ErrUnpublished
	}
	if e != nil || p.Format != 1 {
		return Release{}, ErrUnavailable
	}
	r, e := s.release(p.ReleaseID)
	if e != nil {
		return r, ErrUnavailable
	}
	return r, nil
}
func (s *Store) Current() (Release, error) { return s.current() }
func (s *Store) history() ([]Release, error) {
	current, e := s.current()
	if errors.Is(e, ErrUnpublished) {
		return []Release{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Release{}
	seen := map[string]bool{}
	for len(out) < 20 {
		if seen[current.ReleaseID] {
			return nil, ErrUnavailable
		}
		seen[current.ReleaseID] = true
		out = append(out, current)
		if current.PreviousReleaseID == "" || len(out) >= 20 {
			break
		}
		next, e := s.release(current.PreviousReleaseID)
		if errors.Is(e, os.ErrNotExist) && len(out) >= 20 {
			break
		}
		if e != nil {
			return nil, ErrUnavailable
		}
		current = next
	}
	return out, nil
}
func (s *Store) History(ctx context.Context) (Release, []Release, error) {
	release, e := s.lock(ctx)
	if e != nil {
		return Release{}, nil, e
	}
	defer release()
	current, e := s.current()
	if e != nil && !errors.Is(e, ErrUnpublished) {
		return current, nil, e
	}
	history, e := s.history()
	return current, history, e
}
func (s *Store) archive(hash string) ([]byte, error) {
	if !hashPattern.MatchString(hash) {
		return nil, ErrInvalid
	}
	b, e := read(filepath.Join(s.ArchiveDirectory, hash+".js"), MaxScriptBytes)
	if errors.Is(e, os.ErrNotExist) {
		if _, t := os.Stat(filepath.Join(s.ArchiveDirectory, hash+".gone")); t == nil {
			return nil, ErrGone
		}
		return nil, os.ErrNotExist
	}
	if e != nil || digest(b) != hash || len(b) == 0 {
		return nil, ErrUnavailable
	}
	return b, nil
}
func (s *Store) Script(ctx context.Context, hash string) (Release, []byte, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return Release{}, nil, e
	}
	defer unlock()
	var r Release
	if hash == "" {
		r, e = s.current()
		if e != nil {
			return r, nil, e
		}
		hash = r.ScriptSHA256
	}
	b, e := s.archive(hash)
	return r, b, e
}
func (s *Store) Publish(ctx context.Context, in PublishInput, actor int64) (Release, bool, error) {
	if actor <= 0 || !hashPattern.MatchString(in.SHA256) || len(in.Note) > 2000 || strings.TrimSpace(in.Note) == "" || len(in.Path) > 1024 || len(in.Generation) > 100 {
		return Release{}, false, ErrInvalid
	}
	unlock, e := s.lock(ctx)
	if e != nil {
		return Release{}, false, e
	}
	defer unlock()
	current, e := s.current()
	if e != nil && !errors.Is(e, ErrUnpublished) {
		return Release{}, false, e
	}
	if current.ReleaseID != in.ExpectedReleaseID {
		return Release{}, false, ErrConflict
	}
	var b []byte
	var originalPath, name string
	if in.FromReleaseID != "" {
		if in.Path != "" || in.Generation != "" {
			return Release{}, false, ErrInvalid
		}
		history, e := s.history()
		if e != nil {
			return Release{}, false, e
		}
		found := false
		for _, item := range history {
			if item.ReleaseID == in.FromReleaseID {
				found = true
			}
		}
		if !found {
			return Release{}, false, ErrConflict
		}
		old, e := s.release(in.FromReleaseID)
		if e != nil || old.ScriptSHA256 != in.SHA256 {
			return Release{}, false, ErrConflict
		}
		originalPath, name = old.OriginalPath, old.OriginalName
		b, e = s.archive(in.SHA256)
		if e != nil {
			return Release{}, false, e
		}
	} else {
		if in.Path == CatalogPath || in.Path == "" || in.Generation == "" {
			return Release{}, false, ErrInvalid
		}
		generation, scripts, e := s.Sources.ReadVerifiedScripts(ctx, in.Path, MaxScriptBytes, MaxScriptBytes)
		if e != nil || generation != in.Generation || len(scripts) != 1 || scripts[0].File.SHA256 != in.SHA256 {
			return Release{}, false, ErrConflict
		}
		b = scripts[0].Content
		originalPath = in.Path
		name = strings.TrimSuffix(filepath.Base(filepath.FromSlash(in.Path)), ".js")
	}
	if current.ScriptSHA256 == in.SHA256 {
		return current, true, nil
	}
	if in.ReportRunID != "" {
		if s.Checks == nil {
			return Release{}, false, ErrInvalid
		}
		r, e := s.Checks.Report(in.ReportRunID)
		if e != nil {
			return Release{}, false, ErrInvalid
		}
		matches := false
		for _, x := range r.Results {
			if x.ScriptSHA256 == in.SHA256 {
				matches = true
			}
		}
		if !matches {
			return Release{}, false, ErrInvalid
		}
	}
	if e = ctx.Err(); e != nil {
		return Release{}, false, e
	}
	if e = atomic(filepath.Join(s.ArchiveDirectory, in.SHA256+".js"), b); e != nil {
		return Release{}, false, e
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return Release{}, false, e
	}
	r := Release{1, "r-" + hex.EncodeToString(nonce[:]), current.ReleaseID, in.SHA256, originalPath, name, time.Now().UTC(), actor, strings.TrimSpace(in.Note), in.ReportRunID}
	record, _ := json.Marshal(r)
	if e = atomic(filepath.Join(s.Directory, "releases", r.ReleaseID+".json"), record); e != nil {
		return Release{}, false, e
	}
	if e = ctx.Err(); e != nil {
		return Release{}, false, e
	}
	pointer, _ := json.Marshal(map[string]any{"format": 1, "releaseId": r.ReleaseID})
	if e = atomic(filepath.Join(s.Directory, "current.json"), pointer); e != nil {
		return Release{}, false, e
	}
	// Cleanup cannot turn a successfully committed publication into an apparent failure.
	_ = s.prune(r)
	return r, false, nil
}
func (s *Store) prune(current Release) error {
	history, e := s.history()
	if e != nil {
		return e
	}
	keep := map[string]bool{}
	referenced := map[string]bool{}
	for _, r := range history {
		keep[r.ReleaseID] = true
		referenced[r.ScriptSHA256] = true
	}
	entries, e := os.ReadDir(filepath.Join(s.Directory, "releases"))
	if e != nil {
		return e
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if releasePattern.MatchString(id) && !keep[id] {
			if e = os.Remove(filepath.Join(s.Directory, "releases", entry.Name())); e != nil {
				return e
			}
		}
	}
	entries, e = os.ReadDir(s.ArchiveDirectory)
	if e != nil {
		return e
	}
	for _, x := range entries {
		if !strings.HasSuffix(x.Name(), ".js") {
			continue
		}
		hash := strings.TrimSuffix(x.Name(), ".js")
		if !hashPattern.MatchString(hash) || referenced[hash] {
			continue
		}
		st, e := x.Info()
		if e != nil {
			return e
		}
		if time.Since(st.ModTime()) < 24*time.Hour {
			continue
		}
		if e = atomic(filepath.Join(s.ArchiveDirectory, hash+".gone"), []byte("expired")); e != nil {
			return e
		}
		if e = os.Remove(filepath.Join(s.ArchiveDirectory, x.Name())); e != nil {
			return e
		}
	}
	return nil
}

// Backup copies one locked reference set. Already buffered downloads do not hold publication locks.
func (s *Store) Backup(ctx context.Context, dest string) (map[string]string, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return nil, e
	}
	defer unlock()
	hashes := map[string]string{}
	current, e := s.current()
	if errors.Is(e, ErrUnpublished) {
		return hashes, nil
	}
	if e != nil {
		return nil, e
	}
	history, e := s.history()
	if e != nil {
		return nil, e
	}
	copyFile := func(rel string, b []byte) error {
		if e := atomic(filepath.Join(dest, filepath.FromSlash(rel)), b); e != nil {
			return e
		}
		hashes[rel] = digest(b)
		return nil
	}
	pointer, _ := json.Marshal(map[string]any{"format": 1, "releaseId": current.ReleaseID})
	if e = copyFile("source-recommendations/current.json", pointer); e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	for _, r := range history {
		b, _ := json.Marshal(r)
		if e = copyFile("source-recommendations/releases/"+r.ReleaseID+".json", b); e != nil {
			return nil, e
		}
		if seen[r.ScriptSHA256] {
			continue
		}
		seen[r.ScriptSHA256] = true
		b, e = s.archive(r.ScriptSHA256)
		if e != nil {
			return nil, fmt.Errorf("recommendation backup archive: %w", e)
		}
		if e = copyFile("published/"+r.ScriptSHA256+".js", b); e != nil {
			return nil, e
		}
	}
	return hashes, nil
}
