package sourcerecommendation

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sources"
)

func waitControllerIdle(t *testing.T, a *Automatic) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for a.Controller.ActiveID() != "" {
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestShutdownRestartsUnfinishedSearchInsteadOfDiscardingIt(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	entered := make(chan struct{})
	a.Store.Checks.Runner = cancellationRunner{entered}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	old, _ := a.runtime()
	if err := a.Controller.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	report, err := a.Store.Checks.Report(old.ActiveRunID)
	if err != nil || report.State != "interrupted" || report.CancellationReason != "shutdown" {
		t.Fatal(report.State, report.CancellationReason, err)
	}
	a.Store.Checks.Runner = runner
	a.Controller = sourcecheck.NewController(context.Background(), a.Store.Checks)
	t.Cleanup(func() { a.Controller.Close(context.Background()) })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		r, err := a.runtime()
		a.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if r.ActiveRunID != "" && r.ActiveRunID != old.ActiveRunID {
			if len(r.Queue) == 0 || r.Platforms["netease"].DecisionReason == "check_canceled" {
				t.Fatal("restart discarded pending search", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart did not resume search")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	<-done
	waitControllerIdle(t, a)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil || len(plan.Targets) != 2 {
		t.Fatal("restart could not finish initial selection", plan, err, *now)
	}
}

func TestMetadataIOLeavesAPIsResponsiveAndRejectsStaleResults(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(map[bool]string{false: "sample_edit_and_client_request", true: "pause"}[pause], func(t *testing.T) {
			a, _, _ := availabilityFixture(t)
			bootstrapAvailability(t, a)
			before, version, err := a.Store.Checks.Samples(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			a.Metadata = func(ctx context.Context, platform, id string) (sourcecheck.Metadata, error) {
				close(entered)
				select {
				case <-release:
					return sourcecheck.Metadata{TrackID: id, Title: "stale fetched title", Artist: "artist", DurationMS: 180000}, nil
				case <-ctx.Done():
					return sourcecheck.Metadata{}, ctx.Err()
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tickDone := make(chan error, 1)
			go func() { tickDone <- a.Tick(ctx) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("metadata did not start")
			}
			apiDone := make(chan error, 1)
			go func() {
				_, err := a.Status(ctx, true)
				if err == nil {
					_, err = a.RequestClientCheck(ctx)
				}
				apiDone <- err
			}()
			select {
			case err := <-apiDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("metadata network I/O blocked status or client request")
			}
			if pause {
				state, _, err := a.Store.AutomaticCurrent(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, err = a.Store.AutomaticControl(ctx, ControlInput{ExpectedPlanID: state.CurrentPlanID, ExpectedRevision: state.ControlRevision, Paused: true})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, _, err = a.Store.Checks.ChangeSamples(ctx, version, func(set *sourcecheck.SampleSet) error {
					set.Samples[0].Title = "administrator title"
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-tickDone; err != nil {
				t.Fatal(err)
			}
			after, _, err := a.Store.Checks.Samples(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := "administrator title"
			if pause {
				want = before.Samples[0].Title
			}
			if after.Samples[0].Title != want {
				t.Fatal("stale metadata overwrote intervening edit/control", after.Samples[0].Title)
			}
			r, _ := a.runtime()
			if !r.ClientCheckPending {
				t.Fatal("metadata completion lost admitted client request")
			}
		})
	}
}

func TestRollbackResumeImmediatelyChecksChangedTarget(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	bootstrapAvailability(t, a)
	first, plan, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := a.Store.Sources.ReadVerifiedScripts(context.Background(), "z-alternative.js", MaxScriptBytes, MaxScriptBytes)
	if err != nil {
		t.Fatal(err)
	}
	other := Target{Path: files[0].File.Path, SHA256: files[0].File.SHA256, Name: "z-alternative"}
	plan.Targets["netease"] = other
	second, err := a.Store.commitPlan(first, plan, Decision{At: *now, Actor: "system"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := a.runtime()
	r.Platforms["netease"].TargetSHA256 = other.SHA256
	r.Platforms["netease"].FailedChecks = 1
	r.Platforms["netease"].NextCheck = now.Add(3 * time.Hour)
	if err = a.save(r); err != nil {
		t.Fatal(err)
	}
	rolled, err := a.Store.AutomaticRollback(context.Background(), ControlInput{ExpectedPlanID: second.CurrentPlanID, ExpectedRevision: second.ControlRevision, FromPlanID: first.CurrentPlanID}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	if r.Platforms["netease"].Evidence != nil || r.Platforms["netease"].FailedChecks != 0 || r.Platforms["netease"].NextCheck.After(*now) {
		t.Fatal("rollback retained old target evidence or timer", r.Platforms["netease"])
	}
	_, err = a.Store.AutomaticControl(context.Background(), ControlInput{ExpectedPlanID: rolled.CurrentPlanID, ExpectedRevision: rolled.ControlRevision, Paused: false})
	if err != nil {
		t.Fatal(err)
	}
	runAvailabilityBatch(t, a)
	if runner.count("netease", "test.js") != 6 {
		t.Fatal("resumed rollback did not immediately check restored target")
	}
}

func publishChangedCatalog(t *testing.T, a *Automatic) {
	t.Helper()
	snapshot, err := a.Store.Sources.Current()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	manifest := snapshot.Manifest
	root := filepath.Dir(a.Store.ArchiveDirectory)
	manifest.Generation = "g-20261117000000000000-fedcba9876543210"
	dir := filepath.Join(root, "versions", manifest.Generation)
	if err = os.MkdirAll(filepath.Join(dir, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		body, err := os.ReadFile(filepath.Join(root, "versions", snapshot.Manifest.Generation, "files", file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "files", file.Path), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("/* new fixed script */")
	manifest.Files = append(manifest.Files, sources.File{Path: "zzz-fixed.js", SHA256: digest(body), Size: int64(len(body))})
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if err = os.WriteFile(filepath.Join(dir, "files", "zzz-fixed.js"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".read.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = saveJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if err = saveJSON(filepath.Join(root, "current.json"), map[string]any{"format": 1, "generation": manifest.Generation}); err != nil {
		t.Fatal(err)
	}
}

func TestNewCatalogWakesOnlyMissingPlatformAndOnlyUntriedScripts(t *testing.T) {
	a, now, runner := availabilityFixture(t)
	for _, path := range []string{"test.js", "z-alternative.js", "zz-unused.js"} {
		runner.set("qq", path, "failed")
	}
	for i := 0; i < 4; i++ {
		runAvailabilityBatch(t, a)
	}
	r, _ := a.runtime()
	if len(r.Queue) != 0 || !r.FullDue.After(*now) {
		t.Fatal("search did not exhaust")
	}
	publishChangedCatalog(t, a)
	runAvailabilityBatch(t, a)
	_, plan, err := a.Store.AutomaticCurrent(context.Background())
	if err != nil || plan.Targets["qq"].Path != "zzz-fixed.js" || plan.Targets["netease"].Path != "test.js" {
		t.Fatal("new catalog did not wake missing platform", plan, err)
	}
	if runner.count("qq", "test.js") != 3 || runner.count("netease", "zzz-fixed.js") != 0 || runner.count("qq", "zzz-fixed.js") != 3 {
		t.Fatal("catalog update rescanned tried scripts or healthy platform", runner.calls)
	}
}
