package sourcecheck

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cyapi-go/internal/sources"
)

//go:embed samples.json
var defaultSamples []byte

const maxReportBytes = 32 << 20

type Report struct {
	Format             int        `json:"format"`
	RunID              string     `json:"runId"`
	State              string     `json:"state"`
	Generation         string     `json:"generation"`
	ProbeVersion       string     `json:"probeVersion"`
	SampleSetVersion   string     `json:"sampleSetVersion"`
	Samples            []Sample   `json:"samples"`
	StartedAt          time.Time  `json:"startedAt"`
	FinishedAt         *time.Time `json:"finishedAt,omitempty"`
	Planned            int        `json:"planned"`
	Completed          int        `json:"completed"`
	Results            []Result   `json:"results"`
	Error              string     `json:"error,omitempty"`
	CancellationReason string     `json:"cancellationReason,omitempty"`
}
type Runner interface {
	Run(context.Context, Job) Result
}
type Service struct {
	// Accessed only under the exclusive check lock.
	recovered        map[string]os.FileInfo
	Now              func() time.Time
	Directory        string
	ArchiveDirectory string
	Sources          *sources.Manager
	Runner           Runner
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

type RunOptions struct {
	Selections  []ScriptSelection
	Timeout     time.Duration
	Path        string
	Platform    string
	SamplesFile string
	Progress    func(Result, int, int)
}

// Internal selection accepts catalog identities or a hash in the trusted archive.
// It never accepts a caller-provided filesystem path to read.
type ScriptSelection struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Archived bool   `json:"archived"`
}

func (s *Service) Prepare(ctx context.Context, o RunOptions) (*PreparedRun, error) {
	var report Report
	if s.Sources == nil || s.Runner == nil || s.Directory == "" {
		return nil, errors.New("sourcecheck service not configured")
	}
	if o.Platform != "" && o.Platform != "qq" && o.Platform != "netease" {
		return nil, fmt.Errorf("%w: platform must be qq or netease", ErrInvalid)
	}
	release, e := acquire(s.Directory)
	if e != nil {
		return nil, e
	}
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	if e = s.recoverInterrupted(); e != nil {
		return nil, e
	}
	var set SampleSet
	var hash string
	if o.SamplesFile == "" {
		set, hash, e = s.Samples(ctx)
	} else {
		var f *os.File
		f, e = os.Open(o.SamplesFile)
		if e == nil {
			set, hash, e = DecodeSamples(f)
			f.Close()
		}
	}
	if e != nil {
		return nil, e
	}
	enabled := []Sample{}
	for _, sample := range set.Samples {
		if sample.Enabled && (o.Platform == "" || o.Platform == sample.Platform) {
			enabled = append(enabled, sample)
		}
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("%w: no enabled samples for selected platform", ErrInvalid)
	}
	if preflight, ok := s.Runner.(interface{ Preflight(context.Context) error }); ok {
		if e = preflight.Preflight(ctx); e != nil {
			return nil, e
		}
	}
	generation, scripts, e := s.readSelection(ctx, o)
	if e != nil {
		return nil, e
	}
	if len(scripts)*len(enabled) > 4096 {
		return nil, errors.New("selection exceeds 4096 task batch limit")
	}
	var nonce [8]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	report = Report{Format: 1, RunID: strings.ReplaceAll(s.now().Format("20060102T150405.000000000"), ".", "") + "-" + hex.EncodeToString(nonce[:]), State: "running", Generation: generation, ProbeVersion: ProbeVersion, SampleSetVersion: hash, Samples: set.Samples, StartedAt: s.now(), Planned: len(scripts) * len(enabled), Results: []Result{}}
	if e = s.save(report); e != nil {
		return nil, e
	}
	owned = true
	return &PreparedRun{service: s, report: report, scripts: scripts, samples: enabled, options: o, release: release}, nil
}

func (s *Service) readSelection(ctx context.Context, o RunOptions) (string, []sources.VerifiedScript, error) {
	if len(o.Selections) == 0 {
		return s.Sources.ReadVerifiedScripts(ctx, o.Path, MaxScriptBytes, 64<<20)
	}
	if o.Path != "" || o.SamplesFile != "" || len(o.Selections) > 64 {
		return "", nil, ErrInvalid
	}
	scripts := []sources.VerifiedScript{}
	seen := map[string]bool{}
	generation := ""
	var total int
	for _, sel := range o.Selections {
		if len(sel.SHA256) != 64 || sel.Path == "" || seen[sel.Path+sel.SHA256] {
			return "", nil, ErrInvalid
		}
		seen[sel.Path+sel.SHA256] = true
		var script sources.VerifiedScript
		if sel.Archived {
			rawHash, e := hex.DecodeString(sel.SHA256)
			if e != nil || len(rawHash) != 32 || s.ArchiveDirectory == "" {
				return "", nil, ErrInvalid
			}
			f, e := os.Open(filepath.Join(s.ArchiveDirectory, sel.SHA256+".js"))
			if e != nil {
				return "", nil, e
			}
			b, e := io.ReadAll(io.LimitReader(f, MaxScriptBytes+1))
			f.Close()
			if e != nil || len(b) == 0 || len(b) > MaxScriptBytes || digest(b) != sel.SHA256 {
				return "", nil, ErrInvalid
			}
			script = sources.VerifiedScript{File: sources.File{Path: sel.Path, SHA256: sel.SHA256, Size: int64(len(b))}, Content: b}
		} else {
			gen, found, e := s.Sources.ReadVerifiedScripts(ctx, sel.Path, MaxScriptBytes, MaxScriptBytes)
			if e != nil {
				return "", nil, e
			}
			if len(found) != 1 || found[0].File.SHA256 != sel.SHA256 {
				return "", nil, ErrConflict
			}
			if generation != "" && generation != gen {
				return "", nil, ErrConflict
			}
			generation = gen
			script = found[0]
		}
		total += len(script.Content)
		if total > 64<<20 {
			return "", nil, ErrLimit
		}
		scripts = append(scripts, script)
	}
	return generation, scripts, nil
}

type PreparedRun struct {
	service *Service
	report  Report
	scripts []sources.VerifiedScript
	samples []Sample
	options RunOptions
	release func()
}

func (p *PreparedRun) ID() string { return p.report.RunID }
func (s *Service) Run(ctx context.Context, o RunOptions) (Report, error) {
	p, e := s.Prepare(ctx, o)
	if e != nil {
		return Report{}, e
	}
	return p.Run(ctx)
}
func (p *PreparedRun) Run(ctx context.Context) (Report, error) {
	defer p.release()
	s, report, scripts, enabled, o := p.service, p.report, p.scripts, p.samples, p.options
	var e error
	index := 0
	duplicates := newDuplicateResources(report.Samples)
	for _, script := range scripts {
		for _, sample := range enabled {
			if ctx.Err() != nil {
				markCancellation(&report, ctx)
				break
			}
			index++
			j := Job{Format: ProtocolVersion, ID: fmt.Sprintf("%s-%d", report.RunID, index), ScriptPath: script.File.Path, ScriptSHA256: script.File.SHA256, Script: script.Content, Sample: sample, Quality: "128k"}
			observedAt := s.now()
			result := s.Runner.Run(ctx, j)
			// Host observation time includes process startup; finishing a long
			// report must never renew the freshness of its early jobs.
			result.CheckedAt = observedAt
			report.Results = append(report.Results, result)
			duplicates.add(report.Results, len(report.Results)-1)
			report.Completed++
			if ctx.Err() != nil {
				markCancellation(&report, ctx)
			}
			if e = s.save(report); e != nil {
				return report, e
			}
			if o.Progress != nil {
				o.Progress(report.Results[len(report.Results)-1], report.Completed, report.Planned)
			}
		}
		if report.State != "running" {
			break
		}
	}
	if report.State == "running" {
		report.State = "completed"
	}
	finished := s.now()
	report.FinishedAt = &finished
	if e = s.save(report); e != nil {
		return report, e
	}
	if e = s.prune(); e != nil {
		return report, e
	}
	return report, ctx.Err()
}

func markCancellation(report *Report, ctx context.Context) {
	report.State = "interrupted"
	report.CancellationReason = "shutdown"
	report.Error = safe(ctx.Err())
	switch context.Cause(ctx) {
	case errUserCanceled:
		report.State = "canceled"
		report.CancellationReason = "user"
	case context.DeadlineExceeded:
		report.State = "canceled"
		report.CancellationReason = "timeout"
	case errControlChanged:
		report.CancellationReason = "control_changed"
	}
}
func acquire(dir string) (func(), error) {
	if e := os.MkdirAll(filepath.Join(dir, "runs"), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "check.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = lockFile(f); e != nil {
		f.Close()
		return nil, e
	}
	return func() { unlockFile(f); f.Close() }, nil
}
func atomicBytes(path string, b []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".check-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
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
	if e = replaceFile(tmp, path); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(path))
}
func (s *Service) save(r Report) error {
	release, e := stateLock(context.Background(), s.Directory)
	if e != nil {
		return e
	}
	defer release()
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > maxReportBytes {
		return fmt.Errorf("%w: batch report size", ErrLimit)
	}
	return atomicBytes(filepath.Join(s.Directory, "runs", r.RunID+".json"), b)
}
func (s *Service) recoverInterrupted() error {
	entries, e := os.ReadDir(filepath.Join(s.Directory, "runs"))
	if e != nil {
		return e
	}
	next := make(map[string]os.FileInfo, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if old := s.recovered[entry.Name()]; old != nil && old.Size() == info.Size() && old.ModTime().Equal(info.ModTime()) && os.SameFile(old, info) {
			next[entry.Name()] = info
			continue
		}
		f, e := os.Open(filepath.Join(s.Directory, "runs", entry.Name()))
		if e != nil {
			return e
		}
		var r Report
		e = decodeLimited(f, &r, maxReportBytes)
		f.Close()
		if e == nil && (!idPattern.MatchString(r.RunID) || entry.Name() != r.RunID+".json" || r.Format != 1) {
			e = errors.New("invalid report identity")
		}
		if e != nil {
			var diskError *os.PathError
			if errors.As(e, &diskError) {
				return e
			}
			path := filepath.Join(s.Directory, "runs", entry.Name())
			unlock, err := stateLock(context.Background(), s.Directory)
			if err != nil {
				return err
			}
			err = os.Rename(path, path+".invalid-"+time.Now().UTC().Format("20060102T150405.000000000"))
			unlock()
			if err != nil {
				return err
			}
			slog.Warn("quarantined unreadable source check report", "report", entry.Name(), "error", e)
			continue
		}
		if r.State != "running" {
			next[entry.Name()] = info
		} else {
			r.State = "interrupted"
			now := time.Now().UTC()
			r.FinishedAt = &now
			r.Error = "previous process stopped before batch completed"
			if e = s.save(r); e != nil {
				return e
			}
		}
	}
	s.recovered = next
	return nil
}
func (s *Service) prune() error {
	release, e := stateLock(context.Background(), s.Directory)
	if e != nil {
		return e
	}
	defer release()
	entries, e := os.ReadDir(filepath.Join(s.Directory, "runs"))
	if e != nil {
		return e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		count++
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if count > 100 || time.Since(info.ModTime()) > 30*24*time.Hour {
			if e = os.Remove(filepath.Join(s.Directory, "runs", entry.Name())); e != nil {
				return e
			}
		}
	}
	return nil
}

// LatestReport is read-only: it does not acquire a task lock or create files.
func LatestReport(dir string) (Report, error) {
	var r Report
	entries, e := os.ReadDir(filepath.Join(dir, "runs"))
	if e != nil {
		return r, e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		f, e := os.Open(filepath.Join(dir, "runs", entry.Name()))
		if e != nil {
			return r, e
		}
		e = decodeLimited(f, &r, maxReportBytes)
		f.Close()
		return r, e
	}
	return r, errors.New("no source check reports")
}
