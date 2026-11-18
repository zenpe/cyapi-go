package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cyapi-go/internal/sourcecheck"
)

type sampleObservation struct {
	ScriptPath, ScriptSHA256, Platform, SampleID, Category string
	CheckedAt                                              time.Time
	Limited, Duplicate, Encrypted                          bool
}

type reportSummary struct {
	sourcecheck.RunSummary
	SampleSetVersion, ProbeVersion string
	Results                        []sampleObservation
	file                           os.FileInfo
	invalid                        bool
}

type reportCache struct {
	mu      sync.Mutex
	entries map[string]*reportSummary
}

func (c *reportCache) summaries(ctx context.Context, svc *sourcecheck.Service) ([]*reportSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	files, err := os.ReadDir(filepath.Join(svc.Directory, "runs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	next := make(map[string]*reportSummary, len(files))
	ordered := make([]*reportSummary, 0, len(files))
	for i := len(files) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := files[i]
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		summary := c.entries[id]
		if summary == nil || summary.file.Size() != info.Size() || !summary.file.ModTime().Equal(info.ModTime()) || !os.SameFile(summary.file, info) {
			r, err := svc.Report(id)
			if errors.Is(err, sourcecheck.ErrNotFound) {
				continue
			}
			if errors.Is(err, sourcecheck.ErrReportContent) || errors.Is(err, sourcecheck.ErrInvalid) {
				next[id] = &reportSummary{file: info, invalid: true}
				ordered = append(ordered, next[id])
				continue
			}
			if err != nil {
				return nil, err
			}
			summary = &reportSummary{RunSummary: sourcecheck.RunSummary{RunID: r.RunID, State: r.State, StartedAt: r.StartedAt, Completed: r.Completed, Planned: r.Planned, Error: r.Error}, file: info, SampleSetVersion: r.SampleSetVersion, ProbeVersion: r.ProbeVersion}
			for _, x := range r.Results {
				summary.Results = append(summary.Results, sampleObservation{ScriptPath: x.ScriptPath, ScriptSHA256: x.ScriptSHA256, Platform: x.Platform, SampleID: x.SampleID, Category: sourcecheck.ClassifyResult(x), CheckedAt: x.CheckedAt, Limited: sourcecheck.ProbeLimited(x), Duplicate: len(x.Resource.DuplicateSampleIDs) > 0, Encrypted: x.Resource.ReasonCode == "suspected_encrypted_resource"})
			}
		}
		next[id] = summary
		ordered = append(ordered, summary)
	}
	c.entries = next
	return ordered, nil
}
