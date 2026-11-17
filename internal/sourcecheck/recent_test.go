package sourcecheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecentReportsBoundedNewestFirst(t *testing.T) {
	s := &Service{Directory: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.Directory, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("run-%02d", i)
		if err := s.save(Report{Format: 1, RunID: id}); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	if err := s.VisitRecentReports(context.Background(), func(r Report) bool {
		if r.RunID != fmt.Sprintf("run-%02d", 19-count) {
			t.Fatal("wrong order", r.RunID)
		}
		count++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if count != 16 {
		t.Fatal("unbounded file lookup", count)
	}
	// Sparse oversized reports must not be decoded or skipped to old evidence.
	f, err := os.Create(filepath.Join(s.Directory, "runs", "zzz.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(17 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	count = 0
	if err = s.VisitRecentReports(context.Background(), func(Report) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("oversized report bypassed budget")
	}
}

func TestRecentReportsCumulativeReadBudget(t *testing.T) {
	s := &Service{Directory: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.Directory, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"run-01", "run-02"} {
		if err := s.save(Report{Format: 1, RunID: id, Error: strings.Repeat("x", 9<<20)}); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	if err := s.VisitRecentReports(context.Background(), func(Report) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("cumulative report read budget exceeded", count)
	}
}
