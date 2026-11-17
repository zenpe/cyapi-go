package sourcerecommendation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sources"
)

type availabilityRunner struct {
	mu       sync.Mutex
	outcomes map[string]string
	calls    map[string]int
}

type cancellationRunner struct{ entered chan struct{} }

func (r cancellationRunner) Run(ctx context.Context, j sourcecheck.Job) sourcecheck.Result {
	close(r.entered)
	<-ctx.Done()
	return automaticRunner{}.Run(ctx, j)
}

func TestCancelAutomaticBatchEndsCycleWithoutRetry(t *testing.T) {
	a, now, _ := availabilityFixture(t)
	entered := make(chan struct{})
	a.Store.Checks.Runner = cancellationRunner{entered}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic batch did not start")
	}
	r, _ := a.runtime()
	if err := a.Controller.Cancel(r.ActiveRunID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Controller.ActiveID() != "" {
		if time.Now().After(deadline) {
			t.Fatal("canceled worker did not stop")
		}
		time.Sleep(time.Millisecond)
	}
	// Restart before the scheduler has consumed the cancellation report.
	restartCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() { a.Run(restartCtx); close(done) }()
	deadline = time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		r, err := a.runtime()
		a.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if r.ActiveRunID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart did not consume cancellation")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	<-done
	*now = now.Add(90 * time.Minute)
	for i := 0; i < 10; i++ {
		if err := a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = a.runtime()
	if len(r.Queue) != 0 || r.ActiveRunID != "" || r.Failures != 0 || r.LastError != "" || r.Platforms["netease"].DecisionReason != "check_canceled" {
		t.Fatal("canceled automatic work was requeued", r)
	}
}

func TestBatchDeadlineStillUsesFailureBackoff(t *testing.T) {
	a, _, _ := availabilityFixture(t)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := a.runtime()
	deadline := time.Now().Add(3 * time.Second)
	for a.Controller.ActiveID() != "" {
		if time.Now().After(deadline) {
			t.Fatal("worker stalled")
		}
		time.Sleep(time.Millisecond)
	}
	report, err := a.Store.Checks.Report(r.ActiveRunID)
	if err != nil {
		t.Fatal(err)
	}
	// Both a deadline and an explicit cancellation use the canceled report
	// state; only the latter is an instruction to stop the cycle.
	report.State = "canceled"
	report.Error = context.DeadlineExceeded.Error()
	if err = saveJSON(filepath.Join(a.Store.Checks.Directory, "runs", report.RunID+".json"), report); err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(context.Background()); err == nil {
		t.Fatal("deadline incorrectly treated as explicit cancellation")
	}
	r, _ = a.runtime()
	if r.Failures != 1 || r.RetryAt.IsZero() || len(r.Queue) == 0 {
		t.Fatal("deadline lost failure backoff", r)
	}
}

func (r *availabilityRunner) set(platform, path, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes[platform+path] = outcome
}
func (r *availabilityRunner) count(platform, path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[platform+path]
}
func (r *availabilityRunner) Run(ctx context.Context, j sourcecheck.Job) sourcecheck.Result {
	r.mu.Lock()
	kind := r.outcomes[j.Sample.Platform+j.ScriptPath]
	r.calls[j.Sample.Platform+j.ScriptPath]++
	r.mu.Unlock()
	result := automaticRunner{}.Run(ctx, j)
	result.Resource.Bytes = 1000
	if kind == "failed" || kind == "limited" {
		result.Resolution = "failed"
	}
	if kind == "limited" {
		result.ProbeIssues = []string{"probe_policy"}
	}
	return result
}

func availabilityFixture(t *testing.T) (*Automatic, *time.Time, *availabilityRunner) {
	t.Helper()
	s, g, _ := fixture(t)
	root := filepath.Dir(s.ArchiveDirectory)
	manifestPath := filepath.Join(root, "versions", g, "manifest.json")
	var manifest sources.Manifest
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"z-alternative.js", "zz-unused.js"} {
		body := []byte("/* @name " + name + " */")
		manifest.Files = append(manifest.Files, sources.File{Path: name, SHA256: digest(body), Size: int64(len(body))})
		if err = os.WriteFile(filepath.Join(root, "versions", g, "files", name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = saveJSON(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runner := &availabilityRunner{outcomes: map[string]string{}, calls: map[string]int{}}
	s.Checks = &sourcecheck.Service{Directory: t.TempDir(), Sources: s.Sources, ArchiveDirectory: s.ArchiveDirectory, Runner: runner, Now: func() time.Time { return now }}
	controller := sourcecheck.NewController(context.Background(), s.Checks)
	t.Cleanup(func() { controller.Close(context.Background()) })
	a := &Automatic{Store: s, Controller: controller, Options: AutomaticOptions{Enabled: true, FullInterval: 7 * 24 * time.Hour, WatchInterval: 3 * time.Hour, Policy: DefaultPolicy()}, Now: func() time.Time { return now }}
	return a, &now, runner
}

func runAvailabilityBatch(t *testing.T, a *Automatic) {
	t.Helper()
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Controller.ActiveID() != "" {
		if time.Now().After(deadline) {
			t.Fatal("worker stalled")
		}
		time.Sleep(time.Millisecond)
	}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func bootstrapAvailability(t *testing.T, a *Automatic) {
	t.Helper()
	runAvailabilityBatch(t, a)
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil || plan.Targets["qq"].Path != "test.js" || plan.Targets["netease"].Path != "test.js" {
		t.Fatal(plan, err)
	}
}

func dueQQ(t *testing.T, a *Automatic, now time.Time) {
	t.Helper()
	r, err := a.runtime()
	if err != nil {
		t.Fatal(err)
	}
	r.FullDue = now.Add(7 * 24 * time.Hour)
	r.Platforms["qq"].NextCheck = now
	r.Platforms["netease"].NextCheck = now.Add(24 * time.Hour)
	if err = a.save(r); err != nil {
		t.Fatal(err)
	}
}

func TestInitialAvailabilityPublishesOnceAndStopsPlatformSearch(t *testing.T) {
	a, _, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	r, _ := a.runtime()
	if len(r.Queue) != 0 || r.CompletedTasks != 6 || r.ResourceBytes != 6000 {
		t.Fatal("initial selection wasted candidate reads", r)
	}
	for _, platform := range []string{"qq", "netease"} {
		if runner.count(platform, "test.js") != 3 || runner.count(platform, "z-alternative.js") != 0 || runner.count(platform, "zz-unused.js") != 0 {
			t.Fatal("scanned after finding eligible source")
		}
	}
	before, _, _ := a.Store.AutomaticCurrent(context.Background())
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _, _ := a.Store.AutomaticCurrent(context.Background())
	if before.CurrentPlanID != after.CurrentPlanID || a.Controller.ActiveID() != "" {
		t.Fatal("later work changed the first recommendation")
	}
}

func TestHealthyAvailabilityWatchReadsOnlyIncumbent(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	before, _, _ := a.Store.AutomaticCurrent(context.Background())
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	runAvailabilityBatch(t, a)
	after, _, _ := a.Store.AutomaticCurrent(context.Background())
	if before.CurrentPlanID != after.CurrentPlanID || runner.count("qq", "test.js") != 6 || runner.count("qq", "z-alternative.js") != 0 {
		t.Fatal("healthy watch read alternatives or published")
	}
}

func TestCatalogQueueRebuildDoesNotRestartSettledPlatform(t *testing.T) {
	a, _, runner := availabilityFixture(t)
	runAvailabilityBatch(t, a)
	r, _ := a.runtime()
	if !r.Done["platform:netease"] {
		t.Fatal("settled platform not recorded")
	}
	r.Generation = "previous-catalog"
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil || len(plan.Targets) != 2 || runner.count("netease", "z-alternative.js") != 0 {
		t.Fatal("queue rebuild restarted settled platform", err)
	}
}

func TestTransientFailureRechecksAndKeepsCurrentSource(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	before, _, _ := a.Store.AutomaticCurrent(context.Background())
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	runner.set("qq", "test.js", "failed")
	runAvailabilityBatch(t, a)
	r, _ := a.runtime()
	if r.Platforms["qq"].FailedChecks != 1 || r.Platforms["qq"].DecisionReason != "awaiting_failure_recheck" || runner.count("qq", "z-alternative.js") != 0 {
		t.Fatal("first failure switched or searched", r)
	}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Controller.ActiveID() != "" {
		t.Fatal("rechecked before scheduled time")
	}
	runner.set("qq", "test.js", "")
	*now = now.Add(time.Minute)
	runAvailabilityBatch(t, a)
	after, _, _ := a.Store.AutomaticCurrent(context.Background())
	r, _ = a.runtime()
	if after.CurrentPlanID != before.CurrentPlanID || r.Platforms["qq"].FailedChecks != 0 || runner.count("qq", "test.js") != 9 || runner.count("qq", "z-alternative.js") != 0 {
		t.Fatal("transient failure replaced source")
	}
}

func TestPersistentFailureSelectsOneReplacementAndStops(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	runner.set("qq", "test.js", "failed")
	runAvailabilityBatch(t, a)
	// Restarting preserves the single failed check, without replaying discovery.
	a = &Automatic{Store: a.Store, Controller: a.Controller, Options: a.Options, Now: a.Now}
	*now = now.Add(time.Minute)
	runAvailabilityBatch(t, a)
	_, plan, _ := a.Store.AutomaticCurrent(context.Background())
	if plan.Targets["qq"].Path != "test.js" {
		t.Fatal("replaced before searching")
	}
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	r, _ := a.runtime()
	if err != nil || plan.Targets["qq"].Path != "z-alternative.js" || plan.Targets["netease"].Path != "test.js" || r.hasPlatform("qq") || runner.count("qq", "zz-unused.js") != 0 {
		t.Fatal("replacement or early stop failed", plan, err)
	}
	if r.Platforms["qq"].FailedChecks != 0 || r.Platforms["qq"].DecisionReason != "replaced_after_failed_recheck" {
		t.Fatal(r.Platforms["qq"])
	}
}

func TestFailedIncumbentReopensSearchWhileOtherPlatformHasPendingCandidates(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	r, _ := a.runtime()
	r.Queue = []scanItem{{Platform: "netease", Target: Target{Path: "zz-unused.js"}}}
	r.SliceStarted = time.Time{}
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	runner.set("qq", "test.js", "failed")
	runAvailabilityBatch(t, a)
	*now = now.Add(time.Minute)
	runAvailabilityBatch(t, a)
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil || plan.Targets["qq"].Path != "z-alternative.js" {
		r, _ = a.runtime()
		t.Fatal("settled platform could not reopen replacement search", plan, r.Platforms["qq"], r.Queue, runner.calls, err)
	}
}

func TestOverdueDiscoveryNeverScansAlternativesForHealthyRecommendations(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	before, _, _ := a.Store.AutomaticCurrent(context.Background())
	r, _ := a.runtime()
	r.FullDue = *now
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	after, _, _ := a.Store.AutomaticCurrent(context.Background())
	if before.CurrentPlanID != after.CurrentPlanID || runner.count("qq", "test.js") != 3 || runner.count("netease", "test.js") != 3 {
		t.Fatal("discovery changed or repeatedly downloaded healthy incumbents")
	}
	if runner.count("qq", "z-alternative.js") != 0 || runner.count("qq", "zz-unused.js") != 0 || runner.count("netease", "z-alternative.js") != 0 {
		t.Fatal("healthy recommendations started background discovery")
	}
	r, _ = a.runtime()
	if len(r.Queue) != 0 {
		t.Fatal("discovery never drained")
	}
}

func TestHealthyWatchDropsInheritedCandidateQueue(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	r, _ := a.runtime()
	r.Queue = []scanItem{{Platform: "netease", Target: Target{Path: "z-alternative.js"}}}
	r.FullDue = *now
	r.Platforms["netease"].NextCheck = *now
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	for i := 0; i < 10; i++ {
		if err := a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = a.runtime()
	if len(r.Queue) != 0 || runner.count("netease", "z-alternative.js") != 0 || a.Controller.ActiveID() != "" {
		t.Fatal("healthy incumbent left legacy discovery running")
	}
}

func TestManualFullCheckCompletesWithoutPublishingOrRestartingDiscovery(t *testing.T) {
	a, _, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	before, _, _ := a.Store.AutomaticCurrent(context.Background())
	id, _, err := a.Controller.Start(context.Background(), sourcecheck.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Controller.ActiveID() != "" {
		if err = a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("manual full check stalled")
		}
		time.Sleep(time.Millisecond)
	}
	report, err := a.Store.Checks.Report(id)
	if err != nil || report.State != "completed" || report.Completed != 18 {
		t.Fatal("manual full check stopped early", report, err)
	}
	for i := 0; i < 10; i++ {
		if err = a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	after, _, _ := a.Store.AutomaticCurrent(context.Background())
	if before.CurrentPlanID != after.CurrentPlanID || a.Controller.ActiveID() != "" || runner.count("netease", "z-alternative.js") != 3 {
		t.Fatal("manual report changed recommendation or restarted automatic scans")
	}
}

func TestExhaustedInheritedSearchDoesNotRestartImmediately(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	for _, platform := range []string{"qq", "netease"} {
		for _, path := range []string{"test.js", "z-alternative.js", "zz-unused.js"} {
			runner.set(platform, path, "failed")
		}
	}
	// Model the final batch of a persisted search with an overdue retry date.
	r, _ := a.runtime()
	r.Generation = "old-catalog"
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		runAvailabilityBatch(t, a)
	}
	r, _ = a.runtime()
	r.FullDue = *now
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	for i := 0; i < 10; i++ {
		if err := a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = a.runtime()
	if a.Controller.ActiveID() != "" || r.CompletedTasks != 18 || !r.FullDue.After(*now) {
		t.Fatal("exhausted queue restarted without waiting", r)
	}
}

func TestAvailabilityUpgradeResumesInitialSearchWithoutOldConfirmations(t *testing.T) {
	a, now, _ := availabilityFixture(t)
	r, _ := a.runtime()
	r.Policy = "platform-evidence-v1"
	r.FullDue = now.Add(72 * time.Hour)
	r.CompletedTasks = 168
	r.ResourceBytes = 12345
	r.Platforms["qq"].DecisionReason = "awaiting_confirmation"
	r.Platforms["qq"].Confirmation = &Confirmation{LastAt: *now}
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	bootstrapAvailability(t, a)
	r, _ = a.runtime()
	if r.Platforms["qq"].Confirmation != nil || r.CompletedTasks != 174 || r.ResourceBytes != 18345 {
		t.Fatal("upgrade lost accounting or kept old confirmation flow", r)
	}
}

func TestProbeLimitsDoNotTriggerReplacement(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	before, _, _ := a.Store.AutomaticCurrent(context.Background())
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	runner.set("qq", "test.js", "limited")
	runAvailabilityBatch(t, a)
	r, _ := a.runtime()
	after, _, _ := a.Store.AutomaticCurrent(context.Background())
	if r.Platforms["qq"].FailedChecks != 0 || r.Platforms["qq"].Health != "limited" || before.CurrentPlanID != after.CurrentPlanID {
		t.Fatal("probe restriction treated as source failure")
	}
}
