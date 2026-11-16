package sourcecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrConflict = errors.New("samples changed; reload before editing")
var ErrNotFound = errors.New("item not found")
var ErrInvalid = errors.New("invalid source check input")

func stateLock(parent context.Context, dir string) (func(), error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	for {
		if e = lockFile(f); e == nil {
			return func() { unlockFile(f); f.Close() }, nil
		}
		if !errors.Is(e, ErrBusy) {
			f.Close()
			return nil, e
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func (s *Service) readSamples() (SampleSet, string, error) {
	path := filepath.Join(s.Directory, "samples.json")
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		if e = atomicBytes(path, defaultSamples); e != nil {
			return SampleSet{}, "", e
		}
		f, e = os.Open(path)
	}
	if e != nil {
		return SampleSet{}, "", e
	}
	defer f.Close()
	return decodeSamples(f, false)
}
func (s *Service) Samples(ctx context.Context) (SampleSet, string, error) {
	release, e := stateLock(ctx, s.Directory)
	if e != nil {
		return SampleSet{}, "", e
	}
	defer release()
	return s.readSamples()
}

// ChangeSamples applies a checked mutation to the latest version, never to stale UI state.
func (s *Service) ChangeSamples(ctx context.Context, version string, change func(*SampleSet) error) (SampleSet, string, error) {
	release, e := stateLock(ctx, s.Directory)
	if e != nil {
		return SampleSet{}, "", e
	}
	defer release()
	set, current, e := s.readSamples()
	if e != nil {
		return set, "", e
	}
	if version == "" || current != version {
		return set, current, ErrConflict
	}
	if e = change(&set); e != nil {
		return set, current, e
	}
	if set.Samples == nil {
		set.Samples = []Sample{}
	}
	b, e := json.Marshal(set)
	if e != nil {
		return set, current, e
	}
	checked, next, e := decodeSamples(bytes.NewReader(b), false)
	if e != nil {
		return set, current, ErrInvalid
	}
	if e = ctx.Err(); e != nil {
		return set, current, e
	}
	if e = atomicBytes(filepath.Join(s.Directory, "samples.json"), b); e != nil {
		return set, current, e
	}
	return checked, next, nil
}
func (s *Service) Report(id string) (Report, error) {
	var r Report
	if !idPattern.MatchString(id) {
		return r, ErrInvalid
	}
	f, e := os.Open(filepath.Join(s.Directory, "runs", id+".json"))
	if errors.Is(e, os.ErrNotExist) {
		return r, ErrNotFound
	}
	if e != nil {
		return r, e
	}
	defer f.Close()
	if e = decodeLimited(f, &r, maxReportBytes); e != nil {
		return r, e
	}
	if r.RunID != id || r.Format != 1 {
		return r, ErrInvalid
	}
	return r, nil
}

type RunSummary struct {
	RunID     string    `json:"runId"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"startedAt"`
	Completed int       `json:"completed"`
	Planned   int       `json:"planned"`
	Error     string    `json:"error,omitempty"`
}

func summary(r Report) RunSummary {
	return RunSummary{r.RunID, r.State, r.StartedAt, r.Completed, r.Planned, r.Error}
}
func (s *Service) Reports(limit int) ([]Report, error) {
	entries, e := os.ReadDir(filepath.Join(s.Directory, "runs"))
	if errors.Is(e, os.ErrNotExist) {
		return []Report{}, nil
	}
	if e != nil {
		return nil, e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	out := []Report{}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return nil, e
		}
		if info.Size() > maxReportBytes {
			return nil, ErrLimit
		}
		if total+info.Size() > 64<<20 {
			break
		}
		total += info.Size()
		r, e := s.Report(strings.TrimSuffix(entry.Name(), ".json"))
		if e != nil {
			return nil, e
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Controller owns in-process cancellation; workers belong to the service lifetime, not HTTP requests.
type Controller struct {
	service *Service
	ctx     context.Context
	mu      sync.Mutex
	active  *activeRun
	closed  bool
}
type activeRun struct {
	id, path, platform string
	cancel             context.CancelFunc
	done               chan struct{}
}

func NewController(ctx context.Context, s *Service) *Controller {
	return &Controller{service: s, ctx: ctx}
}
func (c *Controller) Start(ctx context.Context, o RunOptions) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return "", false, context.Canceled
	}
	if c.active != nil {
		if c.active.path == o.Path && c.active.platform == o.Platform {
			return c.active.id, true, nil
		}
		return "", false, ErrBusy
	}
	p, e := c.service.Prepare(ctx, o)
	if e != nil {
		return "", false, e
	}
	work, cancel := context.WithCancel(c.ctx)
	a := &activeRun{id: p.ID(), path: o.Path, platform: o.Platform, cancel: cancel, done: make(chan struct{})}
	c.active = a
	go func() {
		defer close(a.done)
		defer cancel()
		r, e := p.Run(work)
		if e != nil && r.RunID != "" && r.State == "running" {
			r.State = "failed"
			r.Error = safe(e)
			now := time.Now().UTC()
			r.FinishedAt = &now
			_ = c.service.save(r)
		}
		c.mu.Lock()
		if c.active == a {
			c.active = nil
		}
		c.mu.Unlock()
	}()
	return a.id, false, nil
}
func (c *Controller) Cancel(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil && c.active.id == id {
		c.active.cancel()
		return nil
	}
	r, e := c.service.Report(id)
	if e != nil {
		return e
	}
	if r.State == "running" {
		return ErrBusy
	}
	return nil
}
func (c *Controller) ActiveID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return ""
	}
	return c.active.id
}
func (c *Controller) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	a := c.active
	if a != nil {
		a.cancel()
	}
	c.mu.Unlock()
	if a == nil {
		return nil
	}
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recover marks orphaned reports only if no CLI or server process owns the batch lock.
func (s *Service) Recover() error {
	release, e := acquire(s.Directory)
	if errors.Is(e, ErrBusy) {
		return nil
	}
	if e != nil {
		return e
	}
	defer release()
	return s.recoverInterrupted()
}

// HealthVersion is a cheap invalidation token; no report bodies are read here.
func (s *Service) HealthVersion() string {
	var token strings.Builder
	if info, e := os.Stat(filepath.Join(s.Directory, "samples.json")); e == nil {
		fmt.Fprintf(&token, "samples:%d:%d;", info.Size(), info.ModTime().UnixNano())
	}
	entries, e := os.ReadDir(filepath.Join(s.Directory, "runs"))
	if e != nil {
		return token.String()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if info, e := entry.Info(); e == nil {
			fmt.Fprintf(&token, "%s:%d:%d;", entry.Name(), info.Size(), info.ModTime().UnixNano())
			count++
			if count >= 20 {
				break
			}
		}
	}
	return token.String()
}
