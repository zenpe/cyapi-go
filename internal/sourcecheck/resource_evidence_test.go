package sourcecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownResourceEvidenceDoesNotInventEncryptionOrPlayback(t *testing.T) {
	for _, tc := range []struct {
		name, path, contentRange, reason, status string
		body                                     []byte
	}{
		{"encrypted clue", "/O4M-test.mgg?token=private", "", "suspected_encrypted_resource", "unverified", []byte{0x7d, 0x35, 0x28, 1, 0x65}},
		{"unknown complete", "/stream.mp3", "", "unsupported_or_unknown_format", "unverified", []byte{1, 2, 3, 4}},
		{"partial unknown", "/stream", "bytes 0-3/1000", "format_evidence_incomplete", "unverified", []byte{1, 2, 3, 4}},
		{"html despite suffix", "/stream.mflac", "", "non_audio_response", "invalid", []byte("<html>denied</html>")},
		{"valid audio despite suffix", "/stream.mgg", "", "", "recognized_audio", sevenSecondMP3()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
					w.WriteHeader(206)
				}
				w.Write(tc.body)
			}))
			defer server.Close()
			got := checkResource(context.Background(), server.Client(), server.URL+tc.path, nil)
			if got.Status != tc.status || got.ReasonCode != tc.reason || got.HeaderHex == "" || len(got.HeaderHex) > 32 {
				t.Fatalf("%+v", got)
			}
			if strings.Contains(jsJSON(got), "private") {
				t.Fatal("signed URL leaked into evidence")
			}
		})
	}
}

func TestDuplicateResourcesRequireCompleteBytesAndDistinctSongs(t *testing.T) {
	samples := []Sample{
		{ID: "a", TrackID: "1", Title: "Song A", MusicInfo: map[string]any{"singer": "Singer"}},
		{ID: "b", TrackID: "2", Title: "Song B", MusicInfo: map[string]any{"singer": "Singer"}},
		{ID: "same-id", TrackID: "1", Title: "Other metadata"},
		{ID: "same-song", TrackID: "3", Title: " Song A ", MusicInfo: map[string]any{"artist": "singer"}},
	}
	result := func(id string) Result {
		return Result{SampleID: id, ScriptSHA256: "script", Platform: "qq", Quality: "128k", Resolution: "ok", Resource: ResourceResult{Complete: true, SHA256: "full-hash", Status: "recognized_audio", Assessment: "suspected_short_resource"}}
	}
	results := []Result{result("a"), result("b"), result("same-id"), result("same-song")}
	d := newDuplicateResources(samples)
	for i := range results {
		d.add(results, i)
	}
	if strings.Join(results[0].Resource.DuplicateSampleIDs, ",") != "b" {
		t.Fatalf("%+v", results[0])
	}
	if len(results[1].Resource.DuplicateSampleIDs) != 3 || ClassifyResult(results[0]) != OutcomeShort {
		t.Fatalf("%+v", results)
	}
	for _, change := range []func(*Result){
		func(r *Result) { r.Resource.Complete = false },
		func(r *Result) { r.Resource.SHA256 = "different" },
		func(r *Result) { r.ScriptSHA256 = "other-script" },
		func(r *Result) { r.ScriptPath = "other-path.js" },
		func(r *Result) { r.Platform = "netease" },
		func(r *Result) { r.Quality = "320k" },
		func(r *Result) { r.Resolution = "failed" },
	} {
		pair := []Result{result("a"), result("b")}
		change(&pair[1])
		d := newDuplicateResources(samples)
		d.add(pair, 0)
		d.add(pair, 1)
		if len(pair[0].Resource.DuplicateSampleIDs) != 0 || len(pair[1].Resource.DuplicateSampleIDs) != 0 {
			t.Fatal("unrelated or incomplete resources grouped")
		}
	}
}

func TestBatchPersistsDuplicateWarningsForEarlierResults(t *testing.T) {
	manager, _ := sourceFixture(t)
	svc := &Service{Directory: t.TempDir(), Sources: manager, Runner: runnerFunc(func(ctx context.Context, j Job) Result {
		r := resultFor(j)
		r.Initialization = "ok"
		r.Resolution = "ok"
		r.Resource = ResourceResult{Status: "recognized_audio", Complete: true, SHA256: "identical-full-file", Assessment: "suspected_short_resource"}
		finalizeResult(&r)
		return r
	})}
	report, err := svc.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := LatestReport(svc.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Results) != 6 || len(report.Results) != 6 {
		t.Fatalf("%+v", saved)
	}
	for _, r := range saved.Results {
		if len(r.Resource.DuplicateSampleIDs) != 2 || r.Outcome != OutcomeShort {
			t.Fatalf("earlier result not updated: %+v", r)
		}
	}
}
