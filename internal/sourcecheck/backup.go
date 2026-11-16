package sourcecheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupSnapshot captures samples and retained reports under the short publication lock.
// It never waits for the network batch lock or changes live report state.
func BackupSnapshot(ctx context.Context, dir, dest string) (map[string]string, error) {
	hashes := map[string]string{}
	if dir == "" {
		return hashes, nil
	}
	info, e := os.Lstat(dir)
	if errors.Is(e, os.ErrNotExist) {
		return hashes, nil
	}
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalid
	}
	release, e := stateLock(ctx, dir)
	if e != nil {
		return nil, e
	}
	defer release()
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	names := []string{}
	if _, e = root.Lstat("samples.json"); e == nil {
		names = append(names, "samples.json")
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if stat, e := root.Lstat("runs"); e == nil {
		if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
		entries, e := os.ReadDir(filepath.Join(dir, "runs"))
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") {
				if !idPattern.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
					return nil, ErrInvalid
				}
				names = append(names, "runs/"+entry.Name())
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	var total int64
	for _, name := range names {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		stat, e := root.Lstat(name)
		if e != nil {
			return nil, e
		}
		limit := int64(maxReportBytes)
		if name == "samples.json" {
			limit = 1 << 20
		}
		if !stat.Mode().IsRegular() || stat.Size() > limit {
			return nil, ErrInvalid
		}
		total += stat.Size()
		if total > 512<<20 {
			return nil, ErrLimit
		}
		f, e := root.Open(name)
		if e != nil {
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(f, limit+1))
		f.Close()
		if e != nil {
			return nil, e
		}
		if int64(len(b)) > limit {
			return nil, ErrLimit
		}
		if name == "samples.json" {
			if _, _, e = decodeSamples(strings.NewReader(string(b)), false); e != nil {
				return nil, e
			}
		} else {
			var report Report
			if e = decodeLimited(strings.NewReader(string(b)), &report, maxReportBytes); e != nil {
				return nil, e
			}
			if report.RunID != strings.TrimSuffix(strings.TrimPrefix(name, "runs/"), ".json") {
				return nil, ErrInvalid
			}
			if report.State == "running" {
				report.State = "interrupted"
				report.Error = "backup snapshot of in-progress batch"
				now := time.Now().UTC()
				report.FinishedAt = &now
				b, e = json.MarshalIndent(report, "", "  ")
				if e != nil {
					return nil, e
				}
			}
		}
		path := filepath.Join(dest, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return nil, e
		}
		if e = atomicBytes(path, b); e != nil {
			return nil, e
		}
		hashes["source-checks/"+name] = digest(b)
	}
	return hashes, nil
}
