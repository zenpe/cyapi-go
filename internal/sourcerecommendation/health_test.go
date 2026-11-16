package sourcerecommendation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
)

func TestRecommendationHealthUsesObservedOutcomes(t *testing.T) {
	s, g, hash := fixture(t)
	s.Checks = &sourcecheck.Service{Directory: t.TempDir()}
	_, version, err := s.Checks.Samples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := s.Publish(context.Background(), PublishInput{Generation: g, Path: "test.js", SHA256: hash, Note: "classification regression"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	report := sourcecheck.Report{Format: 1, RunID: "new-results", ProbeVersion: sourcecheck.ProbeVersion, SampleSetVersion: version, Results: []sourcecheck.Result{
		{SampleID: "blocked", Resolution: "failed", FailureOrigin: "probe"},
		{SampleID: "permission", Resolution: "failed", FailureOrigin: "source", ProbeIssues: []string{"probe_policy: blocked"}},
		{SampleID: "404", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "unreachable", HTTPStatus: 404}},
		{SampleID: "unknown", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "unverified", ReasonCode: "suspected_encrypted_resource"}},
		{SampleID: "m4a", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio", DuplicateSampleIDs: []string{"other"}}},
		{SampleID: "short", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio", Assessment: "suspected_short_resource"}},
		{SampleID: "canceled", Resolution: "canceled"},
	}}
	for i := range report.Results {
		report.Results[i].ScriptSHA256 = hash
		report.Results[i].Platform = "qq"
		report.Results[i].CheckedAt = time.Now().UTC()
	}
	b, _ := json.Marshal(report)
	if err = atomic(filepath.Join(s.Checks.Directory, "runs", report.RunID+".json"), b); err != nil {
		t.Fatal(err)
	}
	h := s.Describe(current).Health["qq"]
	if h == nil || h.Failed != 3 || h.Limited != 2 || h.Unverified != 1 || h.Recognized != 1 || h.Short != 1 || h.Canceled != 1 || h.Duplicate != 1 || h.SuspectedEncrypted != 1 {
		t.Fatalf("%+v", h)
	}
	// Old detector records remain readable but cannot contaminate current health.
	old := report
	old.RunID = "old-results"
	old.ProbeVersion = "goja-probe-v1"
	old.Results = []sourcecheck.Result{{ScriptSHA256: hash, SampleID: "old-only", Platform: "netease", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio"}}}
	b, _ = json.Marshal(old)
	if err = atomic(filepath.Join(s.Checks.Directory, "runs", old.RunID+".json"), b); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Describe(current).Health["netease"]; ok {
		t.Fatal("old detector evidence included in current recommendation")
	}
}
