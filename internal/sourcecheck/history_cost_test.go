package sourcecheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareWithReportHistory(t *testing.T) {
	manager, _ := sourceFixture(t)
	for _, count := range []int{0, 20, 100} {
		s := Service{Directory: t.TempDir(), Sources: manager, Runner: runnerFunc(func(context.Context, Job) Result { return Result{} })}
		if err := os.MkdirAll(filepath.Join(s.Directory, "runs"), 0700); err != nil {
			t.Fatal(err)
		}
		var bytes int64
		for i := 0; i < count; i++ {
			r := Report{Format: 1, RunID: fmt.Sprintf("history-%03d", i), State: "completed", Results: make([]Result, 168)}
			if err := s.save(r); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(s.Directory, "runs", r.RunID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			bytes += info.Size()
		}
		for _, pass := range []string{"first", "repeat"} {
			start := time.Now()
			p, err := s.Prepare(context.Background(), RunOptions{Platform: "qq", Path: "test.js"})
			if err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			p.release()
			if err := os.Remove(filepath.Join(s.Directory, "runs", p.ID()+".json")); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s %d reports %d bytes Prepare %v", pass, count, bytes, elapsed)
		}
	}
}
