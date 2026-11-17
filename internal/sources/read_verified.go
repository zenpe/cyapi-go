package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// VerifiedScript is a bounded byte snapshot. No filesystem lease survives this read.
type VerifiedScript struct {
	File    File
	Content []byte
}

// WithVerifiedScript serializes the short final publication against catalog sync
// and rollback. No network or worker execution is permitted in commit.
func (m *Manager) WithVerifiedScript(ctx context.Context, path, hash string, limit int64, commit func(VerifiedScript) error) error {
	lock, root, e := m.acquire()
	if e != nil {
		return e
	}
	defer root.Close()
	defer func() { unlockFile(lock); lock.Close() }()
	_, scripts, e := m.ReadVerifiedScripts(ctx, path, limit, limit)
	if e != nil {
		return e
	}
	if len(scripts) != 1 || scripts[0].File.SHA256 != hash {
		return ErrUnavailable
	}
	return commit(scripts[0])
}

func (m *Manager) ReadVerifiedScripts(ctx context.Context, selected string, maxFile, maxTotal int64) (string, []VerifiedScript, error) {
	if maxFile <= 0 || maxTotal < maxFile || maxTotal > 128<<20 {
		return "", nil, errors.New("invalid script read limits")
	}
	if selected != "" && !isScript(selected) {
		return "", nil, errors.New("invalid source path")
	}
	snapshot, e := m.Current()
	if e != nil {
		return "", nil, e
	}
	defer snapshot.Close()
	var out []VerifiedScript
	var total int64
	for _, entry := range snapshot.Manifest.Files {
		if !isScript(entry.Path) || (selected != "" && entry.Path != selected) {
			continue
		}
		if e = checkContext(ctx); e != nil {
			return "", nil, e
		}
		if entry.Size > maxFile || entry.Size > maxTotal-total {
			return "", nil, errors.New("script selection exceeds bounded snapshot size")
		}
		f, e := snapshot.Open(entry)
		if e != nil {
			return "", nil, e
		}
		b, e := io.ReadAll(io.LimitReader(contextReader{ctx, f}, entry.Size+1))
		f.Close()
		if e != nil {
			return "", nil, e
		}
		h := sha256.Sum256(b)
		if int64(len(b)) != entry.Size || hex.EncodeToString(h[:]) != entry.SHA256 {
			return "", nil, ErrUnavailable
		}
		total += int64(len(b))
		out = append(out, VerifiedScript{File: entry, Content: b})
	}
	if len(out) == 0 {
		return "", nil, errors.New("no matching scripts in current snapshot")
	}
	return snapshot.Manifest.Generation, out, nil
}
