package sourcerecommendation

import (
	"context"
	"cyapi-go/internal/sourcecheck"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdleTickPreservesRuntimeFile(t *testing.T) {
	a, _, _ := availabilityFixture(t)
	bootstrapAvailability(t, a)
	ctx := context.Background()
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	file := a.Store.autoPath("runtime.json")
	stamp := time.Unix(1600000000, 0)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := a.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(file)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("idle ticks rewrote runtime: %v %v", info, err)
	}
}

type reviewPreflightFailure struct{ runner *availabilityRunner }

func TestDamagedReportIsQuarantinedBeforeNextRun(t *testing.T) {
	a, _, _ := availabilityFixture(t)
	bootstrapAvailability(t, a)
	checks := a.Store.Checks
	path := filepath.Join(checks.Directory, "runs", "20000101-old.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"qq", "netease"} {
		_, err := checks.Run(context.Background(), sourcecheck.RunOptions{Platform: platform, Path: "test.js"})
		if err != nil {
			t.Fatal(platform, err)
		}
	}
	files, err := filepath.Glob(path + ".invalid-*")
	if err != nil || len(files) != 1 {
		t.Fatalf("quarantine: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != "{" {
		t.Fatalf("damaged report was not preserved: %v", err)
	}
}

func (r reviewPreflightFailure) Run(ctx context.Context, j sourcecheck.Job) sourcecheck.Result {
	return r.runner.Run(ctx, j)
}
func (r reviewPreflightFailure) Preflight(context.Context) error { return sourcecheck.ErrEnvironment }

func TestRealPreflightFailureRetriesAtAdvertisedTime(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	a.Store.Checks.Runner = reviewPreflightFailure{runner}
	if err := a.Tick(context.Background()); !errors.Is(err, sourcecheck.ErrEnvironment) {
		t.Fatal("wrong failure", err)
	}
	r, _ := a.runtime()
	a.Store.Checks.Runner = runner
	*now = r.RetryAt
	before := runner.count("qq", "test.js")
	runAvailabilityBatch(t, a)
	if runner.count("qq", "test.js") == before {
		t.Fatal("real preflight failure recovered, but scheduled retry did not run")
	}
}

func TestRequestImmediatelyAfterResumeRemainsFresh(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	manualEvidence(t, a, "qq")
	manualEvidence(t, a, "netease")
	st, _, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err = a.Store.AutomaticControl(context.Background(), ControlInput{ExpectedPlanID: st.CurrentPlanID, ExpectedRevision: st.ControlRevision, Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	st, err = a.Store.AutomaticControl(context.Background(), ControlInput{ExpectedPlanID: st.CurrentPlanID, ExpectedRevision: st.ControlRevision, Paused: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Request(context.Background(), ControlInput{ExpectedPlanID: st.CurrentPlanID, ExpectedRevision: st.ControlRevision}); err != nil {
		t.Fatal(err)
	}
	before := runner.count("qq", "test.js")
	runAvailabilityBatch(t, a)
	if runner.count("qq", "test.js") == before {
		t.Fatal("accepted immediate inspection lost ForceFresh after resume and reused an old manual report")
	}
}

func TestExhaustedReplacementHonorsFullInterval(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	for _, path := range []string{"test.js", "z-alternative.js", "zz-unused.js"} {
		runner.set("qq", path, "failed")
	}
	runAvailabilityBatch(t, a)
	*now = now.Add(time.Minute)
	runAvailabilityBatch(t, a)
	runAvailabilityBatch(t, a)
	runAvailabilityBatch(t, a)
	r, _ := a.runtime()
	if len(r.Queue) != 0 || !r.FullDue.After(now.Add(6*24*time.Hour)) {
		t.Fatal("bad fixture", r)
	}
	before := runner.count("qq", "z-alternative.js")
	*now = now.Add(3 * time.Hour)
	runAvailabilityBatch(t, a) // Recheck the unhealthy incumbent.
	runAvailabilityBatch(t, a) // Must not restart exhausted discovery before FullDue.
	if runner.count("qq", "z-alternative.js") != before {
		t.Fatal("exhausted replacement search restarted after 3h instead of FullInterval")
	}
}

func TestFailedWatchRetriesAtAdvertisedRetryTime(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for a.Controller.ActiveID() != "" {
		time.Sleep(time.Millisecond)
	}
	r, _ := a.runtime()
	report, err := a.Store.Checks.Report(r.ActiveRunID)
	if err != nil {
		t.Fatal(err)
	}
	report.State = "failed"
	if err = saveJSON(filepath.Join(a.Store.Checks.Directory, "runs", report.RunID+".json"), report); err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(context.Background()); err == nil {
		t.Fatal("fixture should fail")
	}
	r, _ = a.runtime()
	*now = r.RetryAt
	before := runner.count("qq", "test.js")
	runAvailabilityBatch(t, a)
	if runner.count("qq", "test.js") == before {
		t.Fatal("advertised retry time passed but failed watch waits for 3h NextCheck")
	}
}

func TestPublicationConflictRequeuesScheduledCandidate(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(3 * time.Hour)
	dueQQ(t, a, *now)
	runner.set("qq", "test.js", "failed")
	runner.set("qq", "zz-unused.js", "failed")
	runAvailabilityBatch(t, a)
	*now = now.Add(time.Minute)
	runAvailabilityBatch(t, a)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for a.Controller.ActiveID() != "" {
		time.Sleep(time.Millisecond)
	}
	_, version, err := a.Store.Checks.Samples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = a.Store.Checks.ChangeSamples(context.Background(), version, func(set *sourcecheck.SampleSet) error { set.Samples[0].Title += " changed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(context.Background()); err == nil {
		t.Fatal("publication should reject changed samples")
	}
	r, _ := a.runtime()
	found := false
	for _, item := range r.Queue {
		if item.Platform == "qq" && item.Target.Path == "z-alternative.js" {
			found = true
		}
	}
	if !found {
		t.Fatal("passing candidate lost after publication conflict instead of being requeued")
	}
}
