package sourcecheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// VisitRecentReports is a bounded opportunistic lookup, not a report archive
// query. A miss (including a large/corrupt report) falls back to real detection.
// Report names sort by run start time. Only one decoded report is retained.
func (s *Service) VisitRecentReports(ctx context.Context, visit func(Report) bool) error {
	entries, err := os.ReadDir(filepath.Join(s.Directory, "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	remaining := int64(16 << 20)
	files := 0
	for i := len(entries) - 1; i >= 0 && files < 16 && remaining > 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := entries[i]
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		files++
		info, err := entry.Info()
		if err != nil || info.Size() > remaining {
			return nil
		}
		f, err := os.Open(filepath.Join(s.Directory, "runs", entry.Name()))
		if err != nil {
			return nil
		}
		var r Report
		// Reserve a sentinel byte too; growing files cannot escape the total cap.
		limit := min(info.Size(), remaining-1)
		err = decodeLimited(f, &r, limit)
		f.Close()
		remaining -= limit + 1
		if err != nil || r.Format != 1 || r.RunID+".json" != entry.Name() {
			return nil
		}
		if !visit(r) {
			return nil
		}
	}
	return nil
}
