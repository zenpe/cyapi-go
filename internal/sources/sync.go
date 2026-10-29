package sources

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/unicode/norm"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func (m *Manager) acquire() (*os.File, *os.Root, error) {
	if e := os.MkdirAll(m.o.Directory, 0700); e != nil {
		return nil, nil, e
	}
	info, e := os.Lstat(m.o.Directory)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("source root must not be a symlink")
	}
	root, e := os.OpenRoot(m.o.Directory)
	if e != nil {
		return nil, nil, e
	}
	if info, e := root.Lstat("sync.lock"); e == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		root.Close()
		return nil, nil, errors.New("invalid source lock file")
	}
	f, e := root.OpenFile("sync.lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		root.Close()
		return nil, nil, e
	}
	if e = lockFile(f, true); e != nil {
		f.Close()
		root.Close()
		return nil, nil, e
	}
	for _, name := range []string{"staging", "versions"} {
		if e = root.MkdirAll(name, 0700); e != nil {
			unlockFile(f)
			f.Close()
			root.Close()
			return nil, nil, e
		}
		info, e := root.Lstat(name)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			unlockFile(f)
			f.Close()
			root.Close()
			return nil, nil, errors.New("invalid source managed directory")
		}
	}
	return f, root, nil
}
func (m *Manager) cleanupStaging(root *os.Root) error {
	f, e := root.Open("staging")
	if e != nil {
		return e
	}
	entries, e := f.ReadDir(-1)
	f.Close()
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "attempt-") || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unrecognized or linked staging entry")
		}
		if e = removeTree(root, "staging/"+entry.Name()); e != nil {
			return errors.New("could not reclaim source staging directory")
		}
	}
	return nil
}
func (m *Manager) space(required int64) error {
	free, e := m.free(m.o.Directory)
	if e != nil {
		return errors.New("cannot determine source disk space")
	}
	if required < 0 || free < uint64(required) {
		return errors.New("insufficient source disk space")
	}
	return nil
}

type diskWriter struct {
	m          *Manager
	w          io.Writer
	left       int64
	untilCheck int64
}

func (w *diskWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("source write budget exceeded")
	}
	if w.untilCheck <= 0 {
		if e := w.m.space(w.m.o.ReserveBytes + int64(len(p))); e != nil {
			return 0, e
		}
		w.untilCheck = 1 << 20
	}
	n, e := w.w.Write(p)
	w.left -= int64(n)
	w.untilCheck -= int64(n)
	return n, e
}
func repairName(f *zip.File) (string, error) {
	raw := strings.ReplaceAll(f.Name, "\\", "/")
	if f.Flags&0x800 != 0 && !utf8.ValidString(raw) {
		return "", errors.New("invalid UTF-8 ZIP filename")
	}
	if f.Flags&0x800 == 0 {
		legacy, e := charmap.CodePage437.NewDecoder().String(raw)
		if e != nil {
			return "", errors.New("invalid ZIP filename")
		}
		decoded, e := simplifiedchinese.GB18030.NewDecoder().String(raw)
		if e != nil || strings.ContainsRune(decoded, utf8.RuneError) {
			decoded = legacy
		}
		chinese := func(s string) int {
			n := 0
			for _, c := range s {
				if c >= 0x4e00 && c <= 0x9fff {
					n++
				}
			}
			return n
		}
		if chinese(decoded) > chinese(legacy) {
			raw = decoded
		} else {
			raw = legacy
		}
	}
	if strings.HasPrefix(raw, "/") {
		return "", errors.New("absolute ZIP path rejected")
	}
	for _, component := range strings.Split(raw, "/") {
		if component == ".." {
			return "", errors.New("parent ZIP path rejected")
		}
	}
	raw = path.Clean(strings.TrimSuffix(raw, "/"))
	if raw == "." && f.Mode().IsDir() {
		return "", nil
	}
	if !validPath(raw) {
		return "", errors.New("unsafe ZIP member path")
	}
	return raw, nil
}

type member struct {
	file *zip.File
	name string
}

func (m *Manager) inspect(archive *zip.Reader) ([]member, error) {
	if len(archive.File) > m.o.MaxFiles*4+100 {
		return nil, errors.New("too many ZIP entries")
	}
	members := []member{}
	total := int64(0)
	for _, f := range archive.File {
		name, e := repairName(f)
		if e != nil {
			return nil, e
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 || !mode.IsRegular() && !mode.IsDir() {
			return nil, errors.New("ZIP links or special files are not supported")
		}
		if mode.IsDir() {
			continue
		}
		if len(members) >= m.o.MaxFiles || f.UncompressedSize64 > uint64(m.o.MaxFileBytes) || f.UncompressedSize64 > uint64(m.o.MaxTotalBytes-total) {
			return nil, errors.New("ZIP uncompressed contents exceed limits")
		}
		total += int64(f.UncompressedSize64)
		members = append(members, member{f, name})
	}
	// Strip one common enclosing directory only when every file has a directory component.
	common := ""
	flatten := len(members) > 0
	for i, v := range members {
		parts := strings.Split(v.name, "/")
		if len(parts) < 2 {
			flatten = false
			break
		}
		if i == 0 {
			common = parts[0]
		} else if parts[0] != common {
			flatten = false
			break
		}
	}
	if flatten {
		for i := range members {
			members[i].name = strings.TrimPrefix(members[i].name, common+"/")
		}
	}
	seen := map[string]bool{}
	files := map[string]bool{}
	scripts := 0
	for _, v := range members {
		key := strings.ToLower(norm.NFC.String(v.name))
		if !validPath(v.name) || seen[key] {
			return nil, errors.New("ZIP paths collide after normalization")
		}
		seen[key] = true
		files[key] = true
		if isScript(v.name) {
			scripts++
		}
	}
	for key := range files {
		parent := path.Dir(key)
		for parent != "." {
			if files[parent] {
				return nil, errors.New("ZIP file and directory paths collide")
			}
			parent = path.Dir(parent)
		}
	}
	if !m.o.AllowEmpty && (len(members) == 0 || scripts == 0) {
		return nil, errors.New("release ZIP has no script files")
	}
	return members, nil
}
func (m *Manager) Sync(ctx context.Context, force, dry bool) (Report, error) {
	lock, root, e := m.acquire()
	if e != nil {
		return Report{}, e
	}
	defer root.Close()
	defer func() { unlockFile(lock); lock.Close() }()
	if e = m.cleanupStaging(root); e != nil {
		return Report{}, e
	}
	if e = m.space(m.o.MaxZipBytes + m.o.MaxTotalBytes + m.o.ReserveBytes); e != nil {
		return Report{}, e
	}
	rel, a, e := m.latest(ctx)
	if e != nil {
		return Report{}, e
	}
	var nonce [8]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return Report{}, e
	}
	suffix := hex.EncodeToString(nonce[:])
	generation := "g-" + strings.ReplaceAll(m.now().UTC().Format("20060102150405.000000"), ".", "") + "-" + suffix
	attempt := "staging/attempt-" + suffix
	if e = root.Mkdir(attempt, 0700); e != nil {
		return Report{}, e
	}
	defer root.RemoveAll(attempt)
	zipFile, e := root.OpenFile(attempt+"/download.zip", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return Report{}, e
	}
	closed := false
	defer func() {
		if !closed {
			zipFile.Close()
		}
	}()
	size, e := m.download(ctx, a, zipFile)
	if e != nil {
		return Report{}, e
	}
	digest, e := hashFile(ctx, zipFile, size)
	if e != nil {
		return Report{}, e
	}
	if a.Digest != "" && a.Digest != "sha256:"+digest {
		return Report{}, errors.New("ZIP digest differs from GitHub asset digest")
	}
	archive, e := zip.NewReader(zipFile, size)
	if e != nil {
		return Report{}, errors.New("release asset is not a valid ZIP")
	}
	members, e := m.inspect(archive)
	if e != nil {
		return Report{}, e
	}
	if e = root.Mkdir(attempt+"/files", 0700); e != nil {
		return Report{}, e
	}
	manifest := Manifest{Format: Format, Generation: generation, Repo: m.o.Repo, ReleaseID: rel.ID, AssetID: a.ID, Tag: rel.Tag, AssetName: a.Name, AssetUpdatedAt: a.UpdatedAt, ArchiveSHA256: digest, ConfigSHA256: configHash(m.o), PublishedAt: m.now().UTC().Format(timeFormat), Files: []File{}}
	remaining := m.o.MaxTotalBytes
	dirs := map[string]bool{attempt + "/files": true}
	for _, entry := range members {
		if e = checkContext(ctx); e != nil {
			return Report{}, e
		}
		name := attempt + "/files/" + entry.name
		dir := path.Dir(name)
		if e = root.MkdirAll(dir, 0700); e != nil {
			return Report{}, e
		}
		for d := dir; strings.HasPrefix(d, attempt+"/files"); d = path.Dir(d) {
			dirs[d] = true
			if d == attempt+"/files" {
				break
			}
		}
		input, e := entry.file.Open()
		if e != nil {
			return Report{}, errors.New("cannot open ZIP member")
		}
		output, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			input.Close()
			return Report{}, e
		}
		h := sha256.New()
		limit := int64(entry.file.UncompressedSize64)
		writer := &diskWriter{m: m, w: io.MultiWriter(output, h), left: remaining}
		n, copyErr := io.Copy(writer, io.LimitReader(contextReader{ctx, input}, limit+1))
		closeErr := input.Close()
		syncErr := output.Sync()
		outputErr := output.Close()
		if copyErr != nil || closeErr != nil || syncErr != nil || outputErr != nil || n != limit {
			return Report{}, errors.New("ZIP member failed integrity, space or size checks")
		}
		remaining -= n
		manifest.Files = append(manifest.Files, File{entry.name, n, hex.EncodeToString(h.Sum(nil))})
	}
	zipFile.Close()
	closed = true
	if e = root.Remove(attempt + "/download.zip"); e != nil {
		return Report{}, e
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	old, oldErr := m.Current()
	if oldErr == nil {
		same := old.Manifest.Repo == manifest.Repo && old.Manifest.ReleaseID == manifest.ReleaseID && old.Manifest.AssetID == manifest.AssetID && old.Manifest.AssetUpdatedAt == manifest.AssetUpdatedAt && old.Manifest.ArchiveSHA256 == manifest.ArchiveSHA256 && old.Manifest.ConfigSHA256 == manifest.ConfigSHA256 && equalFiles(old.Manifest.Files, manifest.Files)
		valid := same && old.Verify(ctx) == nil
		oldGeneration := old.Manifest.Generation
		old.Close()
		if valid && !force {
			return Report{Generation: oldGeneration, Tag: manifest.Tag, Files: len(manifest.Files), Unchanged: true, DryRun: dry}, nil
		}
	}
	if dry {
		return report(manifest, false, true), nil
	}
	{
		f, e := root.OpenFile(attempt+"/manifest.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return Report{}, e
		}
		e = writeJSON(f, manifest)
		closeErr := f.Close()
		if e != nil {
			return Report{}, e
		}
		if closeErr != nil {
			return Report{}, closeErr
		}
	}
	lease, e := root.OpenFile(attempt+"/.read.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return Report{}, e
	}
	e = lease.Sync()
	lease.Close()
	if e != nil {
		return Report{}, e
	}
	orderedDirs := []string{}
	for d := range dirs {
		orderedDirs = append(orderedDirs, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(orderedDirs)))
	orderedDirs = append(orderedDirs, attempt)
	for _, d := range orderedDirs {
		if e = syncDir(filepath.Join(m.o.Directory, filepath.FromSlash(d))); e != nil {
			return Report{}, e
		}
	}
	if e = checkContext(ctx); e != nil {
		return Report{}, e
	}
	if e = root.Rename(attempt, "versions/"+generation); e != nil {
		return Report{}, e
	}
	if e = syncDir(filepath.Join(m.o.Directory, "versions")); e != nil {
		return Report{}, e
	}
	if e = m.publish(generation); e != nil {
		return Report{}, e
	}
	out := report(manifest, false, false)
	out.CleanupWarnings = m.gc(root, generation)
	return out, nil
}
func equalFiles(a, b []File) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func (m *Manager) gc(root *os.Root, current string) int {
	f, e := root.Open("versions")
	if e != nil {
		return 1
	}
	entries, e := f.ReadDir(-1)
	f.Close()
	if e != nil {
		return 1
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	kept := 0
	warnings := 0
	for _, entry := range entries {
		if !generationPattern.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			warnings++
			continue
		}
		info, e := entry.Info()
		if e != nil {
			warnings++
			continue
		}
		kept++
		if entry.Name() == current || kept <= 2 || m.now().Sub(info.ModTime()) < 24*time.Hour {
			continue
		}
		version, e := root.OpenRoot("versions/" + entry.Name())
		if e != nil {
			warnings++
			continue
		}
		lease, e := safeOpen(version, ".read.lock")
		if e != nil {
			version.Close()
			warnings++
			continue
		}
		if e = lockFile(lease, true); e != nil {
			lease.Close()
			version.Close()
			continue
		}
		// Keep the lease file open while deleting data; Windows forbids deleting the open lease.
		cleanupErr := removeTree(version, "files")
		if cleanupErr == nil {
			cleanupErr = version.Remove("manifest.json")
		}
		unlockFile(lease)
		lease.Close()
		version.Close()
		if cleanupErr != nil {
			warnings++
			continue
		}
		if e = removeTree(root, "versions/"+entry.Name()); e != nil {
			warnings++
		}
	}
	return warnings
}
