package sourcecheck

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	Format           int        `json:"format"`
	RunID            string     `json:"runId"`
	State            string     `json:"state"`
	Generation       string     `json:"generation"`
	ProbeVersion     string     `json:"probeVersion"`
	SampleSetVersion string     `json:"sampleSetVersion"`
	Samples          []Sample   `json:"samples"`
	StartedAt        time.Time  `json:"startedAt"`
	FinishedAt       *time.Time `json:"finishedAt,omitempty"`
	Planned          int        `json:"planned"`
	Completed        int        `json:"completed"`
	Results          []Result   `json:"results"`
	Error            string     `json:"error,omitempty"`
}
type Runner interface {
	Run(context.Context, Job) Result
}
type Service struct {
	Directory string
	Sources   *sources.Manager
	Runner    Runner
}
type RunOptions struct {
	Path        string
	Platform    string
	SamplesFile string
	Progress    func(Result, int, int)
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
	generation, scripts, e := s.Sources.ReadVerifiedScripts(ctx, o.Path, MaxScriptBytes, 64<<20)
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
	report = Report{Format: 1, RunID: strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000000000"), ".", "") + "-" + hex.EncodeToString(nonce[:]), State: "running", Generation: generation, ProbeVersion: ProbeVersion, SampleSetVersion: hash, Samples: set.Samples, StartedAt: time.Now().UTC(), Planned: len(scripts) * len(enabled), Results: []Result{}}
	if e = s.save(report); e != nil {
		return nil, e
	}
	owned = true
	return &PreparedRun{service: s, report: report, scripts: scripts, samples: enabled, options: o, release: release}, nil
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
	for _, script := range scripts {
		for _, sample := range enabled {
			if ctx.Err() != nil {
				report.State = "canceled"
				report.Error = safe(ctx.Err())
				break
			}
			index++
			j := Job{Format: ProtocolVersion, ID: fmt.Sprintf("%s-%d", report.RunID, index), ScriptPath: script.File.Path, ScriptSHA256: script.File.SHA256, Script: script.Content, Sample: sample, Quality: "128k"}
			result := s.Runner.Run(ctx, j)
			report.Results = append(report.Results, result)
			report.Completed++
			if ctx.Err() != nil {
				report.State = "canceled"
				report.Error = safe(ctx.Err())
			}
			if e = s.save(report); e != nil {
				return report, e
			}
			if o.Progress != nil {
				o.Progress(result, report.Completed, report.Planned)
			}
		}
		if report.State != "running" {
			break
		}
	}
	if report.State == "running" {
		report.State = "completed"
	}
	finished := time.Now().UTC()
	report.FinishedAt = &finished
	if e = s.save(report); e != nil {
		return report, e
	}
	if e = s.prune(); e != nil {
		return report, e
	}
	return report, ctx.Err()
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
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		f, e := os.Open(filepath.Join(s.Directory, "runs", entry.Name()))
		if e != nil {
			return e
		}
		var r Report
		e = decodeLimited(f, &r, maxReportBytes)
		f.Close()
		if e != nil {
			return fmt.Errorf("invalid existing report %s: %w", entry.Name(), e)
		}
		if !idPattern.MatchString(r.RunID) || entry.Name() != r.RunID+".json" || r.Format != 1 {
			return errors.New("invalid report identity")
		}
		if r.State == "running" {
			r.State = "interrupted"
			now := time.Now().UTC()
			r.FinishedAt = &now
			r.Error = "previous process stopped before batch completed"
			if e = s.save(r); e != nil {
				return e
			}
		}
	}
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
