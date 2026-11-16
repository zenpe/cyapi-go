package sourcecheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedBatchSnapshotCancelAndBackup(t *testing.T) {
	manager, _ := sourceFixture(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	svc := &Service{Directory: t.TempDir(), Sources: manager, Runner: runnerFunc(func(ctx context.Context, j Job) Result {
		close(started)
		<-ctx.Done()
		close(finished)
		return resultFor(j)
	})}
	c := NewController(context.Background(), svc)
	defer c.Close(context.Background())
	request, cancel := context.WithCancel(context.Background())
	id, reused, e := c.Start(request, RunOptions{Platform: "qq"})
	if e != nil || reused {
		t.Fatalf("start: %v", e)
	}
	<-started
	cancel()
	select {
	case <-finished:
		t.Fatal("HTTP cancellation stopped batch")
	default:
	}
	again, reused, e := c.Start(context.Background(), RunOptions{Platform: "qq"})
	if e != nil || !reused || again != id {
		t.Fatal("duplicate not reused", e)
	}
	if _, _, e = c.Start(context.Background(), RunOptions{Platform: "netease"}); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	if _, e = svc.Run(context.Background(), RunOptions{}); !errors.Is(e, ErrBusy) {
		t.Fatal("CLI lock", e)
	}
	_, version, e := svc.Samples(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = svc.ChangeSamples(context.Background(), version, func(s *SampleSet) error { s.Samples = []Sample{}; return nil })
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = svc.ChangeSamples(context.Background(), version, func(s *SampleSet) error { return nil }); !errors.Is(e, ErrConflict) {
		t.Fatal("CAS", e)
	}
	live, e := svc.Report(id)
	if e != nil || live.Planned != 3 || len(live.Samples) != 6 {
		t.Fatal("snapshot changed", e)
	}
	dest := filepath.Join(t.TempDir(), "source-checks")
	hashes, e := BackupSnapshot(context.Background(), svc.Directory, dest)
	if e != nil {
		t.Fatal(e)
	}
	for name, hash := range hashes {
		b, e := os.ReadFile(filepath.Join(dest, strings.TrimPrefix(name, "source-checks/")))
		if e != nil || digest(b) != hash {
			t.Fatal("backup hash", e)
		}
	}
	restored := &Service{Directory: dest}
	r, e := restored.Report(id)
	if e != nil || r.State != "interrupted" {
		t.Fatal("backup state", e)
	}
	live, _ = svc.Report(id)
	if live.State != "running" {
		t.Fatal("backup mutated live report")
	}
	if e = c.Cancel(id); e != nil {
		t.Fatal(e)
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if e = c.Close(stop); e != nil {
		t.Fatal(e)
	}
	live, _ = svc.Report(id)
	if live.State != "canceled" {
		t.Fatalf("final state %s", live.State)
	}
	if _, e = svc.Prepare(context.Background(), RunOptions{}); !errors.Is(e, ErrInvalid) {
		t.Fatal("empty sample set", e)
	}
}
