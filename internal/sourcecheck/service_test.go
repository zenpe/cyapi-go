package sourcecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyapi-go/internal/sources"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_source-probe-worker" {
		allow := len(os.Args) > 2 && os.Args[2] == "--allow-unconfined-worker"
		if e := WorkerMain(context.Background(), os.Stdin, os.Stdout, allow); e != nil {
			WorkerError(os.Stderr, e)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type runnerFunc func(context.Context, Job) Result

func (f runnerFunc) Run(c context.Context, j Job) Result { return f(c, j) }

func sourceFixture(t *testing.T) (*sources.Manager, string) {
	t.Helper()
	root := t.TempDir()
	generation := "g-20261113000000000000-0123456789abcdef"
	dir := filepath.Join(root, "versions", generation)
	if e := os.MkdirAll(filepath.Join(dir, "files"), 0700); e != nil {
		t.Fatal(e)
	}
	script := []byte("fixture-script")
	manifest := sources.Manifest{Format: 1, Generation: generation, Repo: "guoyue2010/lxmusic-", ArchiveSHA256: digest([]byte("archive")), ConfigSHA256: digest([]byte("config")), Files: []sources.File{{Path: "test.js", Size: int64(len(script)), SHA256: digest(script)}}}
	b, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600)
	os.WriteFile(filepath.Join(dir, ".read.lock"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "files", "test.js"), script, 0600)
	b, _ = json.Marshal(map[string]any{"format": 1, "generation": generation})
	os.WriteFile(filepath.Join(root, "current.json"), b, 0600)
	manager, e := sources.New(sources.DefaultOptions(root))
	if e != nil {
		t.Fatal(e)
	}
	return manager, filepath.Join(dir, "files", "test.js")
}
func TestBatchPersistenceLockAndSnapshot(t *testing.T) {
	manager, scriptPath := sourceFixture(t)
	dir := t.TempDir()
	calls := 0
	svc := &Service{Directory: dir, Sources: manager}
	svc.Runner = runnerFunc(func(ctx context.Context, j Job) Result {
		calls++
		if _, e := acquire(dir); !errors.Is(e, ErrBusy) {
			t.Fatalf("lock not held: %v", e)
		}
		if string(j.Script) != "fixture-script" {
			t.Fatal("script changed during batch")
		}
		// Modifying the origin after the snapshot must not change subsequent jobs.
		os.WriteFile(scriptPath, []byte("changed-script"), 0600)
		r := resultFor(j)
		r.Initialization = "ok"
		r.Resolution = "failed"
		r.FailureOrigin = "source"
		r.Reason = "provider unavailable"
		return r
	})
	r, e := svc.Run(context.Background(), RunOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if calls != 6 || r.State != "completed" || r.Completed != 6 || r.Planned != 6 {
		t.Fatalf("%+v", r)
	}
	saved, e := LatestReport(dir)
	if e != nil || saved.RunID != r.RunID || len(saved.Samples) != 6 {
		t.Fatalf("%+v %v", saved, e)
	}
	_, e = svc.Run(context.Background(), RunOptions{})
	if e == nil {
		t.Fatal("corrupt origin accepted")
	}
}
func TestBatchCancellationAndInterruptedRecovery(t *testing.T) {
	manager, _ := sourceFixture(t)
	dir := t.TempDir()
	svc := &Service{Directory: dir, Sources: manager}
	release, e := acquire(dir)
	if e != nil {
		t.Fatal(e)
	}
	old := Report{Format: 1, RunID: "old-run", State: "running", StartedAt: time.Now().UTC()}
	if e = svc.save(old); e != nil {
		t.Fatal(e)
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Runner = runnerFunc(func(ctx context.Context, j Job) Result { cancel(); return resultFor(j) })
	report, e := svc.Run(ctx, RunOptions{})
	if !errors.Is(e, context.Canceled) || report.State != "canceled" || report.Completed != 1 {
		t.Fatalf("%+v %v", report, e)
	}
	b, e := os.ReadFile(filepath.Join(dir, "runs", "old-run.json"))
	if e != nil {
		t.Fatal(e)
	}
	var recovered Report
	if e = json.Unmarshal(b, &recovered); e != nil || recovered.State != "interrupted" {
		t.Fatal(recovered, e)
	}
}
func TestRealSubprocessProtocolAndCancellation(t *testing.T) {
	j := fixtureJob(t, `lx.send(lx.EVENT_NAMES.inited,{status:false,message:'retired'});`)
	r := (Executor{AllowUnconfined: true}).Run(context.Background(), j)
	if r.Initialization != "failed" || r.FailureOrigin != "source" || !strings.Contains(r.Reason, "retired") || r.Isolation == "" {
		t.Fatalf("%+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = (Executor{AllowUnconfined: true}).Run(ctx, j)
	if r.FailureOrigin != "probe" || !strings.Contains(r.Reason, "canceled") {
		t.Fatalf("%+v", r)
	}
}
func TestProtocolRejectsInvalidInputAndOutputLimit(t *testing.T) {
	var out bytes.Buffer
	if e := WorkerMain(context.Background(), strings.NewReader(`{"format":1,"surprise":true}`), &out, true); e == nil {
		t.Fatal("unknown worker fields accepted")
	}
	canceled := false
	w := &cappedWriter{limit: 4, cancel: func() { canceled = true }}
	if _, e := w.Write([]byte("12345")); !errors.Is(e, ErrLimit) || !canceled || w.Len() != 0 {
		t.Fatal("unbounded output")
	}
}
