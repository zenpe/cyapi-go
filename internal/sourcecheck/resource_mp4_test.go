package sourcecheck

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func observedM4A(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/fish-qq-m4a-metadata.bin")
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestObservedM4AMetadata(t *testing.T) {
	b := observedM4A(t)
	r := ResourceResult{Status: "unverified", Assessment: "unknown"}
	identifyAudio(b, &r)
	if r.Status != "recognized_audio" || r.Format != "m4a" || r.Codec != "aac" || r.DurationMS == nil || *r.DurationMS < 217000 || *r.DurationMS > 218000 || r.DurationMethod != "mp4_mdhd" {
		t.Fatalf("%+v", r)
	}
	// A video-only track and a mere ftyp header must not become audio evidence.
	for _, invalid := range [][]byte{bytes.Replace(b, []byte("soun"), []byte("vide"), 1), b[:32], b[:80]} {
		r = ResourceResult{Status: "unverified"}
		identifyAudio(invalid, &r)
		if r.Status == "recognized_audio" {
			t.Fatalf("unsupported/truncated container accepted: %+v", r)
		}
	}
	// Unknown and all-ones media duration stays unknown; do not invent duration.
	mdhd := bytes.Index(b, []byte("mdhd"))
	for _, duration := range []uint32{0, 0xffffffff} {
		unknown := bytes.Clone(b)
		binary.BigEndian.PutUint32(unknown[mdhd+20:mdhd+24], duration)
		r = ResourceResult{Status: "unverified"}
		identifyAudio(unknown, &r)
		if r.Status != "recognized_audio" || r.DurationMS != nil {
			t.Fatalf("unknown duration: %+v", r)
		}
	}
	// Oversized metadata is bounded by the read window, including extended sizes.
	malformed := bytes.Clone(b)
	binary.BigEndian.PutUint32(malformed[32:36], 0xffffffff)
	r = ResourceResult{Status: "unverified"}
	identifyAudio(malformed, &r)
	if r.Status == "recognized_audio" {
		t.Fatal("oversized metadata accepted")
	}
}

func TestResourceResponseEvidenceAndShortM4A(t *testing.T) {
	b := observedM4A(t)
	mdhd := bytes.Index(b, []byte("mdhd"))
	timescale := binary.BigEndian.Uint32(b[mdhd+16 : mdhd+20])
	binary.BigEndian.PutUint32(b[mdhd+20:mdhd+24], 60*timescale)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mp4")
		w.Header().Set("Content-Range", "bytes 0-999/1000")
		w.WriteHeader(206)
		w.Write(b)
	}))
	defer srv.Close()
	expected := int64(217000)
	r := checkResource(context.Background(), srv.Client(), srv.URL, &expected)
	if r.Status != "recognized_audio" || r.Assessment != "suspected_short_resource" || r.HTTPStatus != 206 || r.ContentType != "audio/mp4" || r.Host == "" || r.FinalHost == "" || r.Bytes != len(b) {
		t.Fatalf("%+v", r)
	}
	bad := &http.Client{Transport: testRoundTrip(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
	})}
	r = checkResource(context.Background(), bad, "https://resource.example/audio", nil)
	if r.Status != "unreachable" || r.HTTPStatus != 404 || r.ReasonCode != "resource_http_error" {
		t.Fatalf("%+v", r)
	}
}

type testRoundTrip func(*http.Request) (*http.Response, error)

func (f testRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
