package sourcecheck

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

func flacFixture(seconds int64) []byte {
	b := make([]byte, 60)
	copy(b, "fLaC")
	b[4] = 128
	b[7] = 34
	binary.BigEndian.PutUint64(b[18:26], uint64(44100)<<44|uint64(seconds*44100))
	return b
}
func TestResourceEvidence(t *testing.T) {
	expected := int64(269000)
	cases := []struct {
		name                       string
		body                       []byte
		status, format, assessment string
	}{
		{"business error", []byte(`{"code":403,"message":"login required"}`), "invalid", "", "unknown"},
		{"html", []byte("<!DOCTYPE html><html>denied</html>"), "invalid", "", "unknown"},
		{"unknown", []byte{0, 1, 2, 3, 4}, "unverified", "", "unknown"},
		{"short", flacFixture(60), "recognized_audio", "flac", "suspected_short_resource"},
		{"duration", flacFixture(269), "recognized_audio", "flac", "no_short_duration_detected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Write(tc.body)
			}))
			defer srv.Close()
			got := checkResource(context.Background(), srv.Client(), srv.URL, &expected)
			if got.Status != tc.status || got.Format != tc.format || got.Assessment != tc.assessment {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestMP3RequiresFramesAndDoesNotInventDuration(t *testing.T) {
	b := make([]byte, 834)
	copy(b, []byte{255, 251, 144, 0})
	copy(b[417:], b[:4])
	got := ResourceResult{Status: "unverified", Assessment: "unknown"}
	identifyAudio(b, &got)
	if got.Status != "recognized_audio" || got.DurationMS != nil {
		t.Fatalf("%+v", got)
	}
	copy(b[36:], "Xing")
	binary.BigEndian.PutUint32(b[40:44], 1)
	binary.BigEndian.PutUint32(b[44:48], 100)
	got = ResourceResult{Status: "unverified"}
	identifyAudio(b, &got)
	if got.DurationMS == nil || *got.DurationMS != 2612 {
		t.Fatalf("%+v", got)
	}
	got = ResourceResult{Status: "unverified"}
	identifyAudio(b[:100], &got)
	if got.Status == "recognized_audio" {
		t.Fatal("single partial frame accepted")
	}
	// ID3 payload is skipped, not mistaken for MPEG frames.
	id3 := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 5}, []byte{0, 0, 0, 0, 0}...)
	id3 = append(id3, b...)
	got = ResourceResult{Status: "unverified"}
	identifyAudio(id3, &got)
	if got.Format != "mp3" {
		t.Fatalf("ID3 skip failed: %+v", got)
	}
}
