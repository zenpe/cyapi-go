package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sources"
)

func TestOverviewCountsFailuresAndSeparatesOldReports(t *testing.T) {
	root := t.TempDir()
	generation := "g-20261113000000000000-0123456789abcdef"
	dir := filepath.Join(root, "versions", generation)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("fixture")
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	write := func(path string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := sources.Manifest{Format: 1, Generation: generation, Repo: "guoyue2010/lxmusic-", ArchiveSHA256: hash, ConfigSHA256: hash, Files: []sources.File{{Path: "test.js", Size: int64(len(data)), SHA256: hash}}}
	write(filepath.Join(dir, "manifest.json"), manifest)
	write(filepath.Join(root, "current.json"), map[string]any{"format": 1, "generation": generation})
	if err := os.WriteFile(filepath.Join(dir, ".read.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "test.js"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := sources.New(sources.DefaultOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	svc := &sourcecheck.Service{Directory: t.TempDir()}
	_, version, err := svc.Samples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(svc.Directory, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	report := sourcecheck.Report{Format: 1, RunID: "new-results", ProbeVersion: sourcecheck.ProbeVersion, SampleSetVersion: version, Results: []sourcecheck.Result{
		{SampleID: "policy", Resolution: "failed", FailureOrigin: "probe"},
		{SampleID: "permission", Resolution: "failed", FailureOrigin: "source", ProbeIssues: []string{"probe_policy: blocked"}},
		{SampleID: "404", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "unreachable"}},
		{SampleID: "unknown", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "unverified", ReasonCode: "suspected_encrypted_resource"}},
		{SampleID: "m4a", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio", DuplicateSampleIDs: []string{"other"}}},
		{SampleID: "short", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio", Assessment: "suspected_short_resource"}},
		{SampleID: "canceled", Resolution: "canceled"},
	}}
	for i := range report.Results {
		report.Results[i].ScriptPath = "test.js"
		report.Results[i].ScriptSHA256 = hash
		report.Results[i].Platform = "qq"
		report.Results[i].CheckedAt = time.Now().UTC()
	}
	write(filepath.Join(svc.Directory, "runs", report.RunID+".json"), report)
	old := report
	old.RunID = "old-results"
	old.ProbeVersion = "goja-probe-v1"
	old.Results = []sourcecheck.Result{{ScriptPath: "test.js", ScriptSHA256: hash, SampleID: "old-only", Platform: "netease", Resolution: "ok", Resource: sourcecheck.ResourceResult{Status: "recognized_audio"}}}
	write(filepath.Join(svc.Directory, "runs", old.RunID+".json"), old)
	h := &Handler{service: svc, sources: manager, controller: sourcecheck.NewController(context.Background(), svc)}
	w := httptest.NewRecorder()
	h.overview(w, httptest.NewRequest("GET", "/admin/source-checks/overview", nil))
	var overview struct {
		Sources []sourceRow              `json:"sources"`
		Runs    []sourcecheck.RunSummary `json:"runs"`
	}
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Sources) != 1 || len(overview.Runs) != 2 {
		t.Fatalf("%+v", overview)
	}
	health := overview.Sources[0].Platforms["qq"]
	if health == nil || health.Failed != 3 || health.Limited != 2 || health.Unverified != 1 || health.Recognized != 1 || health.Short != 1 || health.Canceled != 1 || health.Duplicate != 1 || health.SuspectedEncrypted != 1 {
		t.Fatalf("%+v", health)
	}
	if _, ok := overview.Sources[0].Platforms["netease"]; ok {
		t.Fatal("old report contaminated current overview")
	}
	r := httptest.NewRequest("GET", "/admin/source-checks/runs/old-results", nil)
	r.SetPathValue("id", "old-results")
	w = httptest.NewRecorder()
	h.report(w, r)
	var response struct {
		Report              sourcecheck.Report `json:"report"`
		CurrentProbeVersion string             `json:"currentProbeVersion"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Report.ProbeVersion != "goja-probe-v1" || response.CurrentProbeVersion != sourcecheck.ProbeVersion {
		t.Fatalf("%+v", response)
	}
}
