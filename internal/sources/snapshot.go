package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

type Snapshot struct {
	Manifest Manifest
	root     *os.Root
	lease    *os.File
}

func (s *Snapshot) Close() {
	if s.lease != nil {
		unlockFile(s.lease)
		s.lease.Close()
	}
	s.root.Close()
}
func (m *Manager) Current() (*Snapshot, error) {
	r, e := os.OpenRoot(m.o.Directory)
	if e != nil {
		return nil, ErrUnavailable
	}
	defer r.Close()
	f, e := safeOpen(r, "current.json")
	if e != nil {
		return nil, ErrUnavailable
	}
	var p pointer
	e = readJSON(f, &p, 4096)
	f.Close()
	if e != nil || p.Format != Format || !generationPattern.MatchString(p.Generation) {
		return nil, ErrUnavailable
	}
	return m.snapshot(p.Generation)
}
func (m *Manager) snapshot(generation string) (*Snapshot, error) {
	if !generationPattern.MatchString(generation) {
		return nil, ErrUnavailable
	}
	base, e := os.OpenRoot(m.o.Directory)
	if e != nil {
		return nil, ErrUnavailable
	}
	defer base.Close()
	for _, name := range []string{"versions", "versions/" + generation} {
		info, e := base.Lstat(name)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrUnavailable
		}
	}
	root, e := base.OpenRoot("versions/" + generation)
	if e != nil {
		return nil, ErrUnavailable
	}
	lease, e := safeOpen(root, ".read.lock")
	if e != nil {
		root.Close()
		return nil, ErrUnavailable
	}
	if e = lockFile(lease, false); e != nil {
		lease.Close()
		root.Close()
		return nil, ErrUnavailable
	}
	s := &Snapshot{root: root, lease: lease}
	fail := func() (*Snapshot, error) { s.Close(); return nil, ErrUnavailable }
	f, e := safeOpen(root, "manifest.json")
	if e != nil {
		return fail()
	}
	e = readJSON(f, &s.Manifest, 16<<20)
	f.Close()
	if e != nil {
		return fail()
	}
	manifest := s.Manifest
	if manifest.Format != Format || manifest.Generation != generation || !repoPattern.MatchString(manifest.Repo) || !validDigest(manifest.ConfigSHA256) || !validDigest(manifest.ArchiveSHA256) || len(manifest.Files) > 10000 {
		return fail()
	}
	seen := map[string]bool{}
	var total int64
	previous := ""
	for _, entry := range manifest.Files {
		if !validPath(entry.Path) || !validDigest(entry.SHA256) || entry.Size < 0 || entry.Size > 8<<30 || entry.Size > 8<<30-total || seen[entry.Path] || entry.Path <= previous {
			return fail()
		}
		total += entry.Size
		seen[entry.Path] = true
		previous = entry.Path
	}
	return s, nil
}
func (s *Snapshot) Open(entry File) (*os.File, error) {
	f, e := safeOpen(s.root, "files/"+entry.Path)
	if e != nil {
		return nil, ErrUnavailable
	}
	info, e := f.Stat()
	if e != nil || info.Size() != entry.Size {
		f.Close()
		return nil, ErrUnavailable
	}
	return f, nil
}
func hashFile(ctx context.Context, f *os.File, size int64) (string, error) {
	if _, e := f.Seek(0, io.SeekStart); e != nil {
		return "", e
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(contextReader{ctx, f}, size+1))
	if e != nil || n != size {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Snapshot) Verify(ctx context.Context) error {
	expected := map[string]bool{}
	for _, entry := range s.Manifest.Files {
		if e := checkContext(ctx); e != nil {
			return e
		}
		f, e := s.Open(entry)
		if e != nil {
			return e
		}
		digest, e := hashFile(ctx, f, entry.Size)
		f.Close()
		if e != nil || digest != entry.SHA256 {
			return ErrUnavailable
		}
		expected[entry.Path] = true
	}
	info, e := s.root.Lstat("files")
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnavailable
	}
	tree, e := s.root.OpenRoot("files")
	if e != nil {
		return ErrUnavailable
	}
	defer tree.Close()
	seen := 0
	e = fs.WalkDir(tree.FS(), ".", func(name string, entry fs.DirEntry, e error) error {
		if e != nil {
			return ErrUnavailable
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnavailable
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !expected[filepath.ToSlash(name)] {
			return ErrUnavailable
		}
		seen++
		return nil
	})
	if e != nil || seen != len(expected) {
		return ErrUnavailable
	}
	return nil
}
func (m *Manager) Status(ctx context.Context) (Manifest, error) {
	s, e := m.Current()
	if e != nil {
		return Manifest{}, e
	}
	defer s.Close()
	if e = s.Verify(ctx); e != nil {
		return Manifest{}, e
	}
	return s.Manifest, nil
}
func removeTree(root *os.Root, name string) error {
	info, e := root.Lstat(name)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed source directory is not a regular directory")
	}
	tree, e := root.OpenRoot(name)
	if e != nil {
		return e
	}
	e = fs.WalkDir(tree.FS(), ".", func(name string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("source cleanup refuses symbolic links")
		}
		return nil
	})
	tree.Close()
	if e != nil {
		return e
	}
	return root.RemoveAll(name)
}
func (m *Manager) publish(generation string) error {
	if !generationPattern.MatchString(generation) {
		return ErrUnavailable
	}
	root, e := os.OpenRoot(m.o.Directory)
	if e != nil {
		return e
	}
	defer root.Close()
	f, e := root.OpenFile(".current-"+generation, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	name := ".current-" + generation
	defer root.Remove(name)
	e = writeJSON(f, pointer{Format, generation})
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	// Source root is private, and both paths are direct children on the same filesystem.
	if e = m.replace(filepath.Join(m.o.Directory, name), filepath.Join(m.o.Directory, "current.json")); e != nil {
		return errors.New("could not publish source generation")
	}
	return syncDir(m.o.Directory)
}
func writeJSON(f *os.File, value any) error {
	b, e := jsonBytes(value)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		return e
	}
	return f.Sync()
}
func jsonBytes(value any) ([]byte, error) {
	b, e := json.MarshalIndent(value, "", "  ")
	return append(b, '\n'), e
}

// Rollback shares the same process lock as synchronization and validates the whole version.
func (m *Manager) Rollback(ctx context.Context, generation string) (Report, error) {
	lock, root, e := m.acquire()
	if e != nil {
		return Report{}, e
	}
	defer root.Close()
	defer func() { unlockFile(lock); lock.Close() }()
	s, e := m.snapshot(generation)
	if e != nil {
		return Report{}, e
	}
	defer s.Close()
	if e = s.Verify(ctx); e != nil {
		return Report{}, e
	}
	if e = m.publish(generation); e != nil {
		return Report{}, e
	}
	return report(s.Manifest, false, false), nil
}
func report(m Manifest, unchanged, dry bool) Report {
	return Report{Generation: m.Generation, Tag: m.Tag, Files: len(m.Files), Unchanged: unchanged, DryRun: dry}
}
