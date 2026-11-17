package sourcerecommendation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cyapi-go/internal/sourcecheck"
)

type AutomaticOptions struct {
	Enabled       bool
	FullInterval  time.Duration
	WatchInterval time.Duration
	Policy        Policy
}
type PlatformStatus struct {
	TargetSHA256   string        `json:"targetSha256"`
	Health         string        `json:"health"`
	Stale          bool          `json:"stale"`
	DecisionReason string        `json:"decisionReason"`
	Evidence       *Evidence     `json:"evidence,omitempty"`
	Baseline       *Evidence     `json:"baseline,omitempty"`
	Candidate      *Target       `json:"candidate,omitempty"`
	Confirmation   *Confirmation `json:"confirmation,omitempty"`
	NextCheck      time.Time     `json:"nextCheck"`
}
type scanItem struct {
	Platform string `json:"platform"`
	Target   Target `json:"target"`
}
type AutomaticRuntime struct {
	Policy             string                     `json:"policy"`
	ControlRevision    uint64                     `json:"controlRevision"`
	Platforms          map[string]*PlatformStatus `json:"platforms"`
	Queue              []scanItem                 `json:"queue"`
	Done               map[string]bool            `json:"done"`
	Generation         string                     `json:"generation"`
	FullDue            time.Time                  `json:"fullDue"`
	SliceStarted       time.Time                  `json:"sliceStarted"`
	ResumeAt           time.Time                  `json:"resumeAt"`
	LastAttempt        time.Time                  `json:"lastAttempt"`
	LastCompleted      time.Time                  `json:"lastCompleted"`
	RetryAt            time.Time                  `json:"retryAt"`
	Failures           int                        `json:"failures"`
	LastError          string                     `json:"lastError"`
	ActiveRunID        string                     `json:"activeRunId"`
	ActivePlatform     string                     `json:"activePlatform"`
	ActiveItems        []scanItem                 `json:"activeItems,omitempty"`
	ActiveRevision     uint64                     `json:"activeRevision"`
	RequestedID        string                     `json:"requestedId"`
	ClientCheckPending bool                       `json:"clientCheckPending"`
	LastClientCheck    time.Time                  `json:"lastClientCheck"`
	MetadataIndex      int                        `json:"metadataIndex"`
	MetadataDue        time.Time                  `json:"metadataDue"`
	LastEarlyScan      time.Time                  `json:"lastEarlyScan"`
	CompletedTasks     int64                      `json:"completedTasks"`
	ResourceBytes      int64                      `json:"resourceBytes"`
}
type Automatic struct {
	Store      *Store
	Controller *sourcecheck.Controller
	Options    AutomaticOptions
	Metadata   sourcecheck.MetadataLoader
	Log        *slog.Logger
	Now        func() time.Time
	mu         sync.Mutex
}

func (a *Automatic) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}
func (a *Automatic) runtime() (AutomaticRuntime, error) {
	var r AutomaticRuntime
	e := automaticJSON(a.Store.autoPath("runtime.json"), &r)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return r, e
	}
	if r.Platforms == nil {
		r.Platforms = map[string]*PlatformStatus{}
	}
	if r.Done == nil {
		r.Done = map[string]bool{}
	}
	for _, p := range []string{"qq", "netease"} {
		if r.Platforms[p] == nil {
			r.Platforms[p] = &PlatformStatus{Health: "unavailable"}
		}
	}
	if r.Policy != a.Options.Policy.Fingerprint() {
		for _, p := range r.Platforms {
			p.Confirmation = nil
		}
		r.Policy = a.Options.Policy.Fingerprint()
	}
	return r, nil
}
func (a *Automatic) load() (AutomaticState, Plan, AutomaticRuntime, error) {
	unlock, e := a.Store.lock(context.Background())
	if e != nil {
		return AutomaticState{}, Plan{}, AutomaticRuntime{}, e
	}
	defer unlock()
	st, e := a.Store.automaticState()
	if e != nil {
		return st, Plan{}, AutomaticRuntime{}, e
	}
	p := Plan{Targets: map[string]Target{}}
	if st.CurrentPlanID != "" {
		p, e = a.Store.plan(st.CurrentPlanID)
		if e != nil {
			return st, p, AutomaticRuntime{}, e
		}
	}
	r, e := a.runtime()
	return st, p, r, e
}
func (a *Automatic) save(r AutomaticRuntime) error {
	unlock, e := a.Store.lock(context.Background())
	if e != nil {
		return e
	}
	defer unlock()
	return saveJSON(a.Store.autoPath("runtime.json"), r)
}
func (a *Automatic) Run(ctx context.Context) {
	a.mu.Lock()
	_, _, r, e := a.load()
	if e == nil {
		r.requeueActive()
		r.ActiveRunID = ""
		r.ActivePlatform = ""
		for _, p := range r.Platforms {
			p.Confirmation = nil
		}
		e = a.save(r)
	}
	a.mu.Unlock()
	if e != nil && a.Log != nil {
		a.Log.Error("automatic recommendation recovery failed", "error", e)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if e = a.Tick(ctx); e != nil && a.Log != nil {
			a.Log.Warn("automatic recommendation deferred", "error", e)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *Automatic) deferFailure(r AutomaticRuntime, e error) error {
	if p := r.Platforms[r.ActivePlatform]; p != nil {
		p.NextCheck = a.now().Add(a.Options.WatchInterval)
		p.DecisionReason = "check_failed_retry_scheduled"
	}
	r.requeueActive()
	r.Failures++
	n := r.Failures
	if n > 4 {
		n = 4
	}
	r.RetryAt = a.now().Add(time.Duration(1<<uint(n-1)) * 10 * time.Minute)
	r.LastError = "automatic_check_failed"
	r.ActiveRunID = ""
	r.ActivePlatform = ""
	for _, p := range r.Platforms {
		p.Confirmation = nil
	}
	if save := a.save(r); save != nil {
		return save
	}
	return e
}
func (a *Automatic) Tick(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	st, plan, r, e := a.load()
	if e != nil {
		return e
	}
	now := a.now()
	if r.ControlRevision != st.ControlRevision {
		r.ControlRevision = st.ControlRevision
		r.RequestedID = ""
		r.ClientCheckPending = false
		for _, p := range r.Platforms {
			p.Confirmation = nil
		}
		if e := a.save(r); e != nil {
			return e
		}
	}
	if !a.Options.Enabled || st.Paused {
		if r.ActiveRunID != "" {
			_ = a.Controller.Cancel(r.ActiveRunID)
			r.requeueActive()
			r.ActiveRunID = ""
			r.ActivePlatform = ""
			for _, p := range r.Platforms {
				p.Confirmation = nil
			}
			return a.save(r)
		}
		return nil
	}
	if r.ActiveRunID != "" {
		report, e := a.Store.Checks.Report(r.ActiveRunID)
		if e != nil {
			return a.deferFailure(r, e)
		}
		if report.State == "running" {
			return nil
		}
		r.CompletedTasks += int64(report.Completed)
		for _, result := range report.Results {
			r.ResourceBytes += int64(result.Resource.Bytes)
		}
		platform := r.ActivePlatform
		if st.ControlRevision != r.ActiveRevision {
			r.requeueActive()
			r.ActiveRunID = ""
			r.ActivePlatform = ""
			return a.save(r)
		}
		observations, e := Evaluate(report, platform, a.Options.Policy, now)
		if e != nil {
			return a.deferFailure(r, e)
		}
		r.ActiveRunID = ""
		r.ActivePlatform = ""
		r.ActiveItems = nil
		r.Failures = 0
		r.LastError = ""
		r.LastCompleted = now
		r.RequestedID = ""
		status := r.Platforms[platform]
		status.NextCheck = now.Add(a.Options.WatchInterval)
		var current *Evidence
		for i := range observations {
			x := &observations[i]
			if t, ok := plan.Targets[platform]; ok && x.Target == t {
				current = x
			}
		}
		if current != nil {
			status.TargetSHA256 = current.Target.SHA256
			status.Evidence = current
			status.Health = "available"
			status.DecisionReason = "no_sustained_improvement"
			if status.Baseline == nil || status.Baseline.Target != current.Target || status.Baseline.SampleVersion != current.SampleVersion || status.Baseline.ProbeVersion != current.ProbeVersion {
				status.Baseline = current
			}
			for id, kind := range status.Baseline.Songs {
				if kind == "P" && (current.Songs[id] == "F" || current.Songs[id] == "unsupported") {
					status.Health = "degraded"
				}
			}
			if status.Health != "degraded" && current.E > 0 {
				status.Health = "limited"
			}
			if current.P < a.Options.Policy.MinPositive && status.Health == "available" {
				status.Health = "unavailable"
			}
			if status.Health == "degraded" && now.Sub(r.LastEarlyScan) >= 6*time.Hour {
				r.FullDue = now
				r.LastEarlyScan = now
			}
		}
		chosen := false
		for _, x := range observations {
			if x.Target == plan.Targets[platform] || x.P < a.Options.Policy.MinPositive {
				continue
			}
			if current != nil && len(Gains(*current, x)) == 0 {
				continue
			}
			if current == nil && plan.Targets[platform].SHA256 != "" {
				continue
			}
			status.Candidate = &x.Target
			c, ready := Confirm(status.Confirmation, current, x, a.Options.Policy, now)
			status.Confirmation = c
			chosen = true
			if ready {
				expected := st
				expected.ControlRevision = r.ActiveRevision
				_, e = a.Store.AutomaticPublish(ctx, expected, platform, *c, a.Options.Policy, now, a.Options.Enabled)
				if e != nil {
					status.Confirmation = nil
					status.DecisionReason = "publication_inputs_changed"
				} else {
					status.TargetSHA256 = x.Target.SHA256
					status.Baseline = &x
					status.Evidence = &x
					status.Health = "available"
					status.DecisionReason = "sustained_evidence_improvement"
					status.Confirmation = nil
					status.Candidate = nil
				}
			} else {
				status.DecisionReason = "awaiting_confirmation"
			}
			break
		}
		if !chosen {
			status.Confirmation = nil
			for _, x := range observations {
				if x.Target != plan.Targets[platform] {
					copy := x.Target
					status.Candidate = &copy
					break
				}
			}
		}
		return a.save(r)
	}
	if now.Before(r.RetryAt) || a.Controller.ActiveID() != "" {
		return nil
	}
	if r.ClientCheckPending {
		// Advance ordinary scheduling, preserving the scan queue, backoff,
		// slice budget and confirmation timestamps.
		r.ClientCheckPending = false
		r.FullDue = now
		for _, status := range r.Platforms {
			status.NextCheck = time.Time{}
		}
		if e := a.save(r); e != nil {
			return e
		}
	}
	// Refresh one old sample at a time; metadata I/O is bounded and never becomes
	// a script failure. Compare stable fields, not the refresh timestamp.
	if a.Metadata != nil && !now.Before(r.MetadataDue) {
		set, version, e := a.Store.Checks.Samples(ctx)
		if e != nil {
			return a.deferFailure(r, e)
		}
		if r.MetadataIndex < len(set.Samples) {
			index := r.MetadataIndex
			old := set.Samples[index]
			work, cancel := context.WithTimeout(ctx, 8*time.Second)
			m, err := a.Metadata(work, old.Platform, old.TrackID)
			cancel()
			if err == nil {
				next, err := sourcecheck.SampleFromMetadata(old.Platform, m)
				if err == nil {
					next.ID = old.ID
					next.Enabled = old.Enabled
					next.DurationEvidence = old.DurationEvidence
					if next.ExpectedDurationMS != nil && next.DurationEvidence == "" {
						next.DurationEvidence = "official song-detail metadata"
					}
					ob, _ := json.Marshal(old)
					nb, _ := json.Marshal(next)
					if !bytesEqual(ob, nb) {
						_, _, err = a.Store.Checks.ChangeSamples(ctx, version, func(s *sourcecheck.SampleSet) error { s.Samples[index] = next; return nil })
					}
				}
				if err != nil {
					r.LastError = "sample_metadata_refresh_failed"
				}
			} else {
				r.LastError = "sample_metadata_refresh_failed"
			}
			r.MetadataIndex++
			if r.MetadataIndex >= len(set.Samples) {
				r.MetadataIndex = 0
				r.MetadataDue = now.Add(6 * time.Hour)
			}
			return a.save(r)
		}
		r.MetadataIndex = 0
		r.MetadataDue = now.Add(6 * time.Hour)
	}
	var platform string
	var candidates []Target
	full := false
	// Candidates belong to the mutable catalog; incumbents belong to the
	// immutable archive. Drop stale candidates before they can starve scans.
	manifest, e := a.Store.Sources.Status(ctx)
	if e != nil {
		return a.deferFailure(r, e)
	}
	available := map[string]string{}
	for _, f := range manifest.Files {
		available[f.Path] = f.SHA256
	}
	for _, status := range r.Platforms {
		if c := status.Candidate; c != nil && available[c.Path] != c.SHA256 {
			status.Candidate = nil
			status.Confirmation = nil
		}
		if c := status.Confirmation; c != nil && available[c.Candidate.Path] != c.Candidate.SHA256 {
			status.Confirmation = nil
		}
	}
	for _, p := range []string{"qq", "netease"} {
		status := r.Platforms[p]
		c := status.Confirmation
		if c != nil && !now.Before(c.LastAt.Add(10*time.Minute)) {
			platform = p
			candidates = []Target{c.Candidate}
			break
		}
	}
	if platform == "" {
		for _, p := range []string{"qq", "netease"} {
			status := r.Platforms[p]
			if !now.Before(status.NextCheck) && plan.Targets[p].SHA256 != "" {
				platform = p
				if status.Candidate != nil {
					candidates = []Target{*status.Candidate}
				}
				break
			}
		}
	}
	if platform == "" && (!now.Before(r.FullDue) || len(r.Queue) > 0 || r.RequestedID != "") && !now.Before(r.ResumeAt) {
		manifest, e := a.Store.Sources.Status(ctx)
		if e != nil {
			return a.deferFailure(r, e)
		}
		if len(r.Queue) == 0 || manifest.Generation != r.Generation {
			if len(r.Queue) == 0 {
				r.Done = map[string]bool{}
				r.FullDue = now.Add(a.Options.FullInterval)
			}
			r.Generation = manifest.Generation
			r.Queue = nil
			for _, p := range []string{"qq", "netease"} {
				for _, f := range manifest.Files {
					if strings.HasSuffix(f.Path, ".js") {
						t := Target{f.Path, f.SHA256, scriptName(f.Path)}
						key := p + f.Path + f.SHA256
						if !r.Done[key] {
							r.Queue = append(r.Queue, scanItem{p, t})
						}
					}
				}
			}
			sort.SliceStable(r.Queue, func(i, j int) bool {
				return r.Queue[i].Platform+r.Queue[i].Target.Path < r.Queue[j].Platform+r.Queue[j].Target.Path
			})
		}
		if len(r.Queue) > 0 {
			platform = r.Queue[0].Platform
			full = true
		}
	}
	if platform == "" {
		r.RequestedID = ""
		return a.save(r)
	}
	set, _, e := a.Store.Checks.Samples(ctx)
	if e != nil {
		return a.deferFailure(r, e)
	}
	n := 0
	for _, s := range set.Samples {
		if s.Enabled && s.Platform == platform {
			n++
		}
	}
	if n < a.Options.Policy.MinSamples {
		r.RequestedID = ""
		status := r.Platforms[platform]
		status.Confirmation = nil
		status.DecisionReason = "insufficient_samples"
		status.NextCheck = now.Add(a.Options.WatchInterval)
		if full {
			for len(r.Queue) > 0 && r.Queue[0].Platform == platform {
				r.Queue = r.Queue[1:]
			}
		}
		return a.save(r)
	}
	slots := int((89 * time.Minute) / (time.Duration(n) * sourcecheck.TaskTimeout))
	if slots > 8 {
		slots = 8
	}
	if t := plan.Targets[platform]; t.SHA256 != "" {
		slots--
	}
	if full {
		for len(r.Queue) > 0 && r.Queue[0].Platform == platform && len(candidates) < slots {
			item := r.Queue[0]
			r.ActiveItems = append(r.ActiveItems, item)
			r.Queue = r.Queue[1:]
			r.Done[item.Platform+item.Target.Path+item.Target.SHA256] = true
			if item.Target != plan.Targets[platform] {
				candidates = append(candidates, item.Target)
			}
		}
	}
	selections := []sourcecheck.ScriptSelection{}
	if t := plan.Targets[platform]; t.SHA256 != "" {
		selections = append(selections, sourcecheck.ScriptSelection{Path: t.Path, SHA256: t.SHA256, Archived: true})
	}
	for _, t := range candidates {
		if t != plan.Targets[platform] {
			selections = append(selections, sourcecheck.ScriptSelection{Path: t.Path, SHA256: t.SHA256})
		}
	}
	if len(selections) == 0 {
		return a.save(r)
	}
	budget := time.Duration(n*len(selections))*sourcecheck.TaskTimeout + time.Minute
	if full {
		if r.SliceStarted.IsZero() {
			r.SliceStarted = now
		}
		if now.Sub(r.SliceStarted)+budget > 3*time.Hour {
			r.requeueActive()
			r.ResumeAt = now.Add(10 * time.Minute)
			r.SliceStarted = time.Time{}
			return a.save(r)
		}
	}
	r.ActivePlatform = platform
	id, _, e := a.Controller.Start(ctx, sourcecheck.RunOptions{Platform: platform, Selections: selections, Timeout: budget})
	if errors.Is(e, sourcecheck.ErrBusy) {
		r.ActivePlatform = ""
		r.requeueActive()
		return a.save(r)
	}
	if e != nil {
		return a.deferFailure(r, e)
	}
	r.ActiveRunID = id
	r.ActivePlatform = platform
	r.ActiveRevision = st.ControlRevision
	r.LastAttempt = now
	return a.save(r)
}
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
func (a *Automatic) Request(ctx context.Context, in ControlInput) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, _, r, e := a.load()
	if e != nil {
		return "", e
	}
	if st.CurrentPlanID != in.ExpectedPlanID || st.ControlRevision != in.ExpectedRevision || !a.Options.Enabled || st.Paused {
		return "", ErrConflict
	}
	if r.RequestedID != "" {
		return r.RequestedID, nil
	}
	if a.Controller.ActiveID() != "" {
		return "", sourcecheck.ErrBusy
	}
	r.RequestedID, e = newID("check-")
	if e != nil {
		return "", e
	}
	r.RetryAt = time.Time{}
	for _, status := range r.Platforms {
		status.NextCheck = time.Time{}
	}
	e = a.save(r)
	return r.RequestedID, e
}
func (a *Automatic) Status(ctx context.Context, private bool) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, p, r, e := a.load()
	if e != nil {
		return nil, e
	}
	now := a.now()
	platforms := map[string]any{}
	for _, platform := range []string{"qq", "netease"} {
		status := r.Platforms[platform]
		status.Stale = status.Evidence == nil || now.Before(status.Evidence.CheckedAt) || now.Sub(status.Evidence.CheckedAt) > a.Options.Policy.MaxAge
		if p.Targets[platform].SHA256 != status.TargetSHA256 {
			status = &PlatformStatus{TargetSHA256: p.Targets[platform].SHA256, Health: "unavailable", Stale: true, DecisionReason: "awaiting_check"}
		}
		if private {
			platforms[platform] = status
		} else {
			row := map[string]any{"targetSha256": status.TargetSHA256, "health": status.Health, "stale": status.Stale, "decisionReason": status.DecisionReason}
			if x := status.Evidence; x != nil {
				row["checkedAt"] = x.CheckedAt
				row["n"] = x.N
				row["p"] = x.P
				row["v"] = x.V
				row["f"] = x.F
				row["e"] = x.E
				row["u"] = x.U
				row["unsupported"] = x.Unsupported
			}
			platforms[platform] = row
		}
	}
	out := map[string]any{"format": 1, "planId": st.CurrentPlanID, "enabled": a.Options.Enabled, "paused": st.Paused, "platforms": platforms}
	if private {
		out["state"] = st
		out["runtime"] = r
		if r.ActiveRunID != "" {
			if report, err := a.Store.Checks.Report(r.ActiveRunID); err == nil {
				out["progress"] = map[string]any{"runId": report.RunID, "completed": report.Completed, "planned": report.Planned, "state": report.State}
			}
		}
	}
	return out, nil
}
func (a *Automatic) CancelOwned() {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _, r, e := a.load()
	if e == nil && r.ActiveRunID != "" {
		_ = a.Controller.Cancel(r.ActiveRunID)
	}
}

func (r *AutomaticRuntime) requeueActive() {
	if len(r.ActiveItems) == 0 {
		return
	}
	r.Queue = append(r.ActiveItems, r.Queue...)
	for _, item := range r.ActiveItems {
		delete(r.Done, item.Platform+item.Target.Path+item.Target.SHA256)
	}
	r.ActiveItems = nil
}
