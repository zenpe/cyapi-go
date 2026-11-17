package sourcerecommendation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
)

func evidenceReport(now time.Time, a, b []string) sourcecheck.Report {
	samples := []sourcecheck.Sample{}
	results := []sourcecheck.Result{}
	for i := 0; i < len(a); i++ {
		id := fmt.Sprint(i)
		samples = append(samples, sourcecheck.Sample{ID: id, TrackID: id, Platform: "qq", Enabled: true})
	}
	for index, kinds := range [][]string{a, b} {
		if kinds == nil {
			continue
		}
		hash := digest([]byte(fmt.Sprint(index)))
		for i, kind := range kinds {
			r := sourcecheck.Result{SampleID: fmt.Sprint(i), ScriptPath: fmt.Sprintf("%d.js", index), ScriptSHA256: hash, Platform: "qq", Quality: "128k", ProbeVersion: sourcecheck.ProbeVersion, Isolation: "cgroup_v2_512MiB", Initialization: "ok", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio"}}
			switch kind {
			case "F":
				r.Resolution = "failed"
			case "U":
				r.Resource.Status = "unverified"
			case "E":
				r.Resolution = "failed"
				r.ProbeIssues = []string{"probe_policy"}
			case "duplicate":
				r.Resource.DuplicateSampleIDs = []string{"another"}
			case "short":
				r.Resource.Assessment = "suspected_short_resource"
			case "unsupported":
				r.Initialization = "unsupported"
			}
			results = append(results, r)
		}
	}
	finish := now
	return sourcecheck.Report{State: "completed", RunID: now.Format(time.RFC3339Nano), StartedAt: now.Add(-time.Minute), FinishedAt: &finish, Planned: len(results), Completed: len(results), Samples: samples, Results: results, SampleSetVersion: "samples", ProbeVersion: sourcecheck.ProbeVersion}
}
func TestEvidenceComparisonAndConfirmation(t *testing.T) {
	now := time.Now().UTC()
	p := DefaultPolicy()
	for _, tc := range []struct {
		name string
		a, b []string
		win  bool
	}{
		{"coverage", []string{"P", "P", "F"}, []string{"P", "P", "P"}, true},
		{"unknown increases evidence", []string{"P", "P", "U"}, []string{"P", "P", "P"}, true},
		{"environment cannot lose", []string{"P", "P", "E"}, []string{"P", "P", "P"}, false},
		{"mutual wins", []string{"P", "P", "F"}, []string{"F", "P", "P"}, false},
		{"common failure", []string{"P", "P", "F"}, []string{"P", "P", "F"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := evidenceReport(now, tc.a, tc.b)
			out, e := Evaluate(r, "qq", p, now)
			if e != nil {
				t.Fatal(e)
			}
			byPath := map[string]Evidence{}
			for _, x := range out {
				byPath[x.Target.Path] = x
			}
			g := Gains(byPath["0.js"], byPath["1.js"])
			if (len(g) > 0) != tc.win {
				t.Fatalf("gains %v", g)
			}
		})
	}
	first, e := Evaluate(evidenceReport(now, []string{"P", "P", "F"}, []string{"P", "P", "P"}), "qq", p, now)
	if e != nil {
		t.Fatal(e)
	}
	var current, candidate Evidence
	for _, x := range first {
		if x.Target.Path == "0.js" {
			current = x
		} else {
			candidate = x
		}
	}
	c, ready := Confirm(nil, &current, candidate, p, now)
	if ready || c == nil {
		t.Fatal("published before confirmation")
	}
	if _, ready = Confirm(c, &current, candidate, p, now); ready {
		t.Fatal("same report counted twice")
	}
	later := now.Add(11 * time.Minute)
	current.RunID = "later"
	candidate.RunID = "later"
	current.StartedAt = later
	candidate.StartedAt = later
	current.CheckedAt = later
	candidate.CheckedAt = later
	c, ready = Confirm(c, &current, candidate, p, later)
	if !ready || len(c.Rounds) != 2 {
		t.Fatal("sustained gain not confirmed")
	}
	_, ready = Confirm(c, &current, candidate, p, later.Add(7*time.Hour))
	if ready {
		t.Fatal("expired evidence published")
	}
}
func TestEvidenceRejectsIncompleteUnconfinedAndFalsePositives(t *testing.T) {
	now := time.Now()
	r := evidenceReport(now, []string{"P", "short", "duplicate"}, nil)
	out, e := Evaluate(r, "qq", DefaultPolicy(), now)
	if e != nil || out[0].P != 1 || out[0].V != 0 {
		t.Fatal(out, e)
	}
	r.Results[0].Isolation = "explicitly_unconfined"
	if _, e = Evaluate(r, "qq", DefaultPolicy(), now); e == nil {
		t.Fatal("unconfined accepted")
	}
	r.Results[0].Isolation = "windows_job_512MiB"
	r.Completed--
	if _, e = Evaluate(r, "qq", DefaultPolicy(), now); e == nil {
		t.Fatal("partial report accepted")
	}
}

func TestConfirmationRequiresSameGainAcrossRounds(t *testing.T) {
	now := time.Now().UTC()
	p := DefaultPolicy()
	round := func(at time.Time, kinds []string) (Evidence, Evidence) {
		xs, err := Evaluate(evidenceReport(at, kinds, []string{"P", "P", "P"}), "qq", p, at)
		if err != nil {
			t.Fatal(err)
		}
		byPath := map[string]Evidence{}
		for _, x := range xs {
			byPath[x.Target.Path] = x
		}
		return byPath["0.js"], byPath["1.js"]
	}
	a, b := round(now, []string{"F", "P", "P"})
	c, _ := Confirm(nil, &a, b, p, now)
	later := now.Add(12 * time.Minute)
	a, b = round(later, []string{"P", "F", "P"})
	if c, ready := Confirm(c, &a, b, p, later); ready || c != nil {
		t.Fatal("different winning songs confirmed", c)
	}
}

func TestSchedulerControlAndStaleCandidateRecovery(t *testing.T) {
	s, _, hash := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now().UTC()
	s.Checks = &sourcecheck.Service{Directory: t.TempDir(), Sources: s.Sources, ArchiveDirectory: s.ArchiveDirectory, Runner: automaticRunner{}, Now: func() time.Time { return now }}
	controller := sourcecheck.NewController(ctx, s.Checks)
	defer controller.Close(context.Background())
	a := &Automatic{Store: s, Controller: controller, Options: AutomaticOptions{Enabled: true, FullInterval: 72 * time.Hour, WatchInterval: 3 * time.Hour, Policy: DefaultPolicy()}, Now: func() time.Time { return now }}
	r, _ := a.runtime()
	r.FullDue = now.Add(72 * time.Hour)
	r.Platforms["qq"].Confirmation = &Confirmation{LastAt: now, Candidate: Target{"test.js", hash, "test"}}
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	st, err := s.AutomaticControl(ctx, ControlInput{Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	if r.Platforms["qq"].Confirmation != nil {
		t.Fatal("pause preserved confirmation")
	}
	files, err := s.Backup(ctx, t.TempDir())
	if err != nil || files["source-recommendations/automatic/state.json"] == "" {
		t.Fatal("unpublished pause omitted from backup", err)
	}
	_, err = s.AutomaticControl(ctx, ControlInput{ExpectedRevision: st.ControlRevision, Paused: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	missing := Target{"removed.js", strings.Repeat("a", 64), "removed"}
	r.Platforms["qq"].Candidate = &missing
	r.Platforms["qq"].Confirmation = &Confirmation{LastAt: now.Add(-time.Hour), Candidate: missing}
	r.FullDue = now
	if err = a.save(r); err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	if r.Platforms["qq"].Candidate != nil || r.ActiveRunID == "" {
		t.Fatal("stale candidate blocked full scan", r)
	}
	deadline := time.Now().Add(3 * time.Second)
	for controller.ActiveID() != "" {
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if err = a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	if r.LastCompleted.IsZero() {
		t.Fatal("scan did not complete")
	}
}
func installAutomatic(t *testing.T, s *Store, hash string) AutomaticState {
	t.Helper()
	st, e := s.commitPlan(AutomaticState{Format: 1}, Plan{Targets: map[string]Target{"qq": {"auto.js", hash, "auto"}}}, Decision{At: time.Now(), Actor: "system", Reason: "fixture"})
	if e != nil {
		t.Fatal(e)
	}
	return st
}
func TestAutomaticBackupLegacyPruneAndRollbackTransaction(t *testing.T) {
	s, g, hash := fixture(t)
	ctx := context.Background()
	auto := []byte("automatic bytes")
	autoHash := digest(auto)
	if e := atomic(filepath.Join(s.ArchiveDirectory, autoHash+".js"), auto); e != nil {
		t.Fatal(e)
	}
	st := installAutomatic(t, s, autoHash)
	backup := t.TempDir()
	files, e := s.Backup(ctx, backup)
	if e != nil || files["source-recommendations/automatic/state.json"] == "" || files["published/"+autoHash+".js"] == "" {
		t.Fatal("automatic-only backup incomplete", files, e)
	}
	restored := &Store{Directory: filepath.Join(backup, "source-recommendations"), ArchiveDirectory: filepath.Join(backup, "published")}
	_, restoredPlan, err := restored.AutomaticCurrent(ctx)
	if err != nil || restoredPlan.Targets["qq"].SHA256 != autoHash {
		t.Fatal("restored automatic plan unavailable", err)
	}
	if _, err = restored.archive(autoHash); err != nil {
		t.Fatal("restored script unavailable", err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(s.ArchiveDirectory, autoHash+".js"), old, old)
	if _, _, e = s.Publish(ctx, PublishInput{Generation: g, Path: "test.js", SHA256: hash, Note: "legacy publish"}, 1); e != nil {
		t.Fatal(e)
	}
	if _, e = s.archive(autoHash); e != nil {
		t.Fatal("legacy prune removed automatic target", e)
	}
	// Create a second immutable plan, then rollback must also pause in one state.
	second, e := s.commitPlan(st, Plan{Targets: map[string]Target{"qq": {"test.js", hash, "test"}}}, Decision{At: time.Now(), Actor: "system"})
	if e != nil {
		t.Fatal(e)
	}
	rolled, e := s.AutomaticRollback(ctx, ControlInput{ExpectedPlanID: second.CurrentPlanID, ExpectedRevision: second.ControlRevision, FromPlanID: st.CurrentPlanID}, 1)
	if e != nil || !rolled.Paused || rolled.ControlRevision != second.ControlRevision+1 || rolled.CurrentPlanID == st.CurrentPlanID {
		t.Fatal(rolled, e)
	}
	_, plan, e := s.AutomaticCurrent(ctx)
	if e != nil || plan.Targets["qq"].SHA256 != autoHash {
		t.Fatal(plan, e)
	}
	if _, e = s.AutomaticControl(ctx, ControlInput{ExpectedPlanID: second.CurrentPlanID}); !errors.Is(e, ErrConflict) {
		t.Fatal("stale control accepted", e)
	}
	orphan := Decision{ID: "d-" + strings.Repeat("f", 32), At: time.Now()}
	_ = saveJSON(s.autoPath("decisions", orphan.ID+".json"), orphan)
	history, e := s.AutomaticHistory(ctx)
	if e != nil || len(history) != 3 {
		t.Fatal("uncommitted decision exposed", history, e)
	}
}

type automaticRunner struct{}

func (automaticRunner) Run(ctx context.Context, j sourcecheck.Job) sourcecheck.Result {
	return sourcecheck.Result{Format: 1, JobID: j.ID, ScriptPath: j.ScriptPath, ScriptSHA256: j.ScriptSHA256, SampleID: j.Sample.ID, Platform: j.Sample.Platform, Quality: j.Quality, ProbeVersion: sourcecheck.ProbeVersion, Isolation: "windows_job_512MiB", Initialization: "ok", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio"}}
}
func TestAutomaticSchedulerPublishesBothPlatformsWithoutAdmin(t *testing.T) {
	s, _, hash := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now().UTC()
	checks := &sourcecheck.Service{Directory: t.TempDir(), Sources: s.Sources, ArchiveDirectory: s.ArchiveDirectory, Runner: automaticRunner{}, Now: func() time.Time { return now }}
	s.Checks = checks
	controller := sourcecheck.NewController(ctx, checks)
	defer controller.Close(context.Background())
	a := &Automatic{Store: s, Controller: controller, Options: AutomaticOptions{Enabled: true, FullInterval: 72 * time.Hour, WatchInterval: 3 * time.Hour, Policy: DefaultPolicy()}, Now: func() time.Time { return now }}
	runtime, _ := a.runtime()
	runtime.FullDue = now.Add(72 * time.Hour)
	if err := a.save(runtime); err != nil {
		t.Fatal(err)
	}
	if result, err := a.RequestClientCheck(ctx); err != nil || result.Code != "accepted" {
		t.Fatal("client request did not advance initial scan", result, err)
	}
	tick := func() {
		t.Helper()
		if e := a.Tick(ctx); e != nil {
			t.Fatal(e)
		}
		deadline := time.Now().Add(3 * time.Second)
		for controller.ActiveID() != "" {
			if time.Now().After(deadline) {
				t.Fatal("worker did not finish")
			}
			time.Sleep(time.Millisecond)
		}
	}
	tick()
	tick()
	tick()
	tick()
	if st, _, _ := s.AutomaticCurrent(ctx); st.CurrentPlanID != "" {
		t.Fatal("published first round")
	}
	now = now.Add(11 * time.Minute)
	tick()
	tick()
	tick()
	tick()
	st, plan, e := s.AutomaticCurrent(ctx)
	if e != nil || plan.Targets["qq"].SHA256 != hash || plan.Targets["netease"].SHA256 != hash {
		t.Fatal("platform automation incomplete", plan, e)
	}
	paused, e := s.AutomaticControl(ctx, ControlInput{ExpectedPlanID: st.CurrentPlanID, ExpectedRevision: st.ControlRevision, Paused: true})
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(4 * time.Hour)
	tick()
	if controller.ActiveID() != "" {
		t.Fatal("paused scheduler started")
	}
	a.Options.Enabled = false
	mux := http.NewServeMux()
	a.Register(mux)
	req := httptest.NewRequest("GET", "/lx/recommendation-plan", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	etag := w.Header().Get("ETag")
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 304 {
		t.Fatal("stable plan not 304", w.Code)
	}
	var got Plan
	req = httptest.NewRequest("GET", "/lx/recommendation-plan", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PlanID != paused.CurrentPlanID {
		t.Fatal("disabled flag hid published plan")
	}
}
