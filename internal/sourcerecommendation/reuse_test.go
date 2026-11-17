package sourcerecommendation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
)

func manualEvidence(t *testing.T, a *Automatic, platform string) sourcecheck.Report {
	t.Helper()
	r, err := a.Store.Checks.Run(context.Background(), sourcecheck.RunOptions{Platform: platform, Path: "test.js"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestInitialReuseConsumesOnlyScheduledCandidateFromFullReport(t *testing.T) {
	a, _, runner := availabilityFixture(t)
	runner.set("netease", "test.js", "failed")
	report, err := a.Store.Checks.Run(context.Background(), sourcecheck.RunOptions{Platform: "netease"})
	if err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	st, _, err := a.Store.AutomaticCurrent(context.Background())
	if !errors.Is(err, ErrUnpublished) || st.CurrentPlanID != "" {
		t.Fatal("published unscheduled candidate from full report", st, err)
	}
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	r, _ := a.runtime()
	if err != nil || plan.Targets["netease"].Path != "z-alternative.js" || r.Platforms["netease"].Evidence.RunID != report.RunID || r.CompletedTasks != 0 || r.ResourceBytes != 0 || runner.count("netease", "z-alternative.js") != 3 {
		t.Fatal("initial evidence reuse changed discovery or counters", plan, r, err)
	}
}

func TestReuseManualEvidencePreservesIdentityAgeAndCounters(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	report := manualEvidence(t, a, "qq")
	*now = now.Add(5 * time.Minute)
	dueQQ(t, a, *now)
	before, _ := a.runtime()
	runAvailabilityBatch(t, a)
	after, _ := a.runtime()
	status := after.Platforms["qq"]
	if runner.count("qq", "test.js") != 6 || status.Evidence.RunID != report.RunID || !status.Evidence.CheckedAt.Equal(*report.FinishedAt) || !status.NextCheck.Equal(report.Results[0].CheckedAt.Add(a.Options.WatchInterval)) {
		t.Fatal("reuse lost original evidence or started worker", status)
	}
	if before.ResourceBytes != after.ResourceBytes || before.CompletedTasks != after.CompletedTasks || !before.LastCompleted.Equal(after.LastCompleted) {
		t.Fatal("reuse counted original work again", before, after)
	}
}

func TestReuseNewestFailureCannotResurrectOlderSuccessOrConfirmItself(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	manualEvidence(t, a, "qq")
	*now = now.Add(time.Second)
	runner.set("qq", "test.js", "failed")
	report := manualEvidence(t, a, "qq")
	dueQQ(t, a, *now)
	runAvailabilityBatch(t, a)
	r, _ := a.runtime()
	if r.Platforms["qq"].Evidence.RunID != report.RunID || r.Platforms["qq"].FailedChecks != 1 {
		t.Fatal(r)
	}
	*now = now.Add(time.Minute)
	runAvailabilityBatch(t, a)
	r, _ = a.runtime()
	if runner.count("qq", "test.js") != 12 || r.Platforms["qq"].FailedChecks != 2 || r.Platforms["qq"].Evidence.RunID == report.RunID {
		t.Fatal("failure recheck reused its previous evidence", r)
	}
}

func TestReuseRejectsOldIndividualObservationAndIncompatibleEvidence(t *testing.T) {
	a, now, _ := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	report := manualEvidence(t, a, "qq")
	_, plan, _ := a.Store.AutomaticCurrent(context.Background())
	target := plan.Targets["qq"]
	if _, ok := a.reuseTime(report, target, "qq", report.SampleSetVersion, *now); !ok {
		t.Fatal("valid fixture rejected")
	}
	for _, kind := range []string{"old", "missing", "future", "partial", "unconfined", "version", "quality", "duplicate", "sample_version"} {
		t.Run(kind, func(t *testing.T) {
			r := report
			r.Results = append([]sourcecheck.Result(nil), report.Results...)
			version := report.SampleSetVersion
			switch kind {
			case "old":
				r.StartedAt = now.Add(-time.Hour)
				r.Results[0].CheckedAt = r.StartedAt
			case "missing":
				r.Results[0].CheckedAt = time.Time{}
			case "future":
				r.Results[0].CheckedAt = now.Add(time.Minute)
			case "partial":
				r.Results = r.Results[:2]
				r.Completed = 2
			case "unconfined":
				r.Results[0].Isolation = "unconfined"
			case "version":
				r.ProbeVersion = "old"
			case "quality":
				r.Results[0].Quality = "320k"
			case "duplicate":
				r.Results[1].SampleID = r.Results[0].SampleID
			case "sample_version":
				version = "changed"
			}
			if _, ok := a.reuseTime(r, target, "qq", version, *now); ok {
				t.Fatal("unsafe evidence reused")
			}
		})
	}
}

func TestAdminRequestForcesBothPlatformsAcrossRestart(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	manualEvidence(t, a, "qq")
	manualEvidence(t, a, "netease")
	st, _, _ := a.Store.AutomaticCurrent(context.Background())
	if _, err := a.Request(context.Background(), ControlInput{ExpectedPlanID: st.CurrentPlanID, ExpectedRevision: st.ControlRevision}); err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	r, _ := a.runtime()
	if r.RequestedID == "" || !r.ForceFresh["netease"] || r.ForceFresh["qq"] {
		t.Fatal("request completed after first platform", r)
	}
	// Reconstruct the scheduler from durable state, as after a service restart.
	restarted := &Automatic{Store: a.Store, Controller: a.Controller, Options: a.Options, Now: a.Now}
	runAvailabilityBatch(t, restarted)
	r, _ = restarted.runtime()
	if runner.count("qq", "test.js") != 9 || runner.count("netease", "test.js") != 9 || r.RequestedID != "" || len(r.ForceFresh) != 0 {
		t.Fatal("forced real checks were not preserved", r)
	}
}

func TestReusePendingReportExpiresBeforeConsumption(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	manualEvidence(t, a, "qq")
	dueQQ(t, a, *now)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := a.runtime()
	if !r.ActiveReused {
		t.Fatal("did not find report")
	}
	*now = now.Add(16 * time.Minute)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	if runner.count("qq", "test.js") != 9 {
		t.Fatal("expired lookup did not fall back")
	}
}

func TestNewestIncompleteGroupPreventsFallbackToOlderSuccess(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	*now = now.Add(time.Hour)
	manualEvidence(t, a, "qq")
	*now = now.Add(time.Second)
	report := manualEvidence(t, a, "qq")
	report.Results = report.Results[:2]
	if err := saveJSON(filepath.Join(a.Store.Checks.Directory, "runs", report.RunID+".json"), report); err != nil {
		t.Fatal(err)
	}
	dueQQ(t, a, *now)
	runAvailabilityBatch(t, a)
	if runner.count("qq", "test.js") != 12 {
		t.Fatal("incomplete newest report was skipped")
	}
}
