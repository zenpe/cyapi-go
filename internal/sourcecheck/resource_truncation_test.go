package sourcecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTruncatedHTTPBodyCannotBecomePositiveEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(flacFixture(269))
	}))
	defer srv.Close()
	expected := int64(269000)
	got := checkResource(context.Background(), srv.Client(), srv.URL, &expected)
	classification := ClassifyResult(Result{Initialization: "ok", Resolution: "ok", Resource: got})
	t.Logf("status=%s reason=%s stop=%s bytes=%d declared=%d complete=%v outcome=%s", got.Status, got.ReasonCode, got.ReadStop, got.Bytes, got.ContentLength, got.Complete, classification)
	if got.Status != "unreachable" || got.ReasonCode != "resource_read_failed" || classification == OutcomeRecognized {
		t.Fatal("server truncated HTTP Content-Length, but reader accepted prefix as positive audio evidence")
	}
}
