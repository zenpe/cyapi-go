package sourcecheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

type closeTrackingBody struct {
	*bytes.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error { b.closed = true; return nil }

type bodyTransport struct{ body *closeTrackingBody }

func (f bodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: f.body, ContentLength: int64(f.body.Len()), Request: r}, nil
}

func TestResourceEarlyStopClosesBodyWithoutCompleteHash(t *testing.T) {
	body := &closeTrackingBody{Reader: bytes.NewReader(append([]byte("<!doctype html><html>"), bytes.Repeat([]byte("x"), 2<<20)...))}
	client := &http.Client{Transport: bodyTransport{body}}
	r := checkResource(context.Background(), client, "https://fixture.example/error", nil)
	if !body.closed || body.Len() == 0 || r.Bytes != 64<<10 || r.ReadStop != "non_audio_response" || r.Status != "invalid" || r.Complete || r.SHA256 != "" || r.ReadLimit {
		t.Fatal(r, body.closed, body.Len())
	}
}

func TestStagedReadsPreserveEvidenceAndAccountActualBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		stop string
	}{
		{"short_mp3", sevenSecondMP3(), "eof"},
		{"normal_flac", append(flacFixture(269), make([]byte, 2<<20)...), "eof"},
		{"unknown", bytes.Repeat([]byte{0x7d}, 2<<20), "eof"},
		{"cap", make([]byte, maxBytes+1), "read_limit"},
		{"json", append([]byte(`{"error":true}`), bytes.Repeat([]byte(" "), 2<<20)...), "non_audio_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounted := 0
			data, stop, err := readResource(bytes.NewReader(tc.data), func(n int) error { accounted += n; return nil }, "application/json")
			if err != nil || stop != tc.stop || accounted != len(data) {
				t.Fatal(stop, err, accounted, len(data))
			}
			if tc.stop == "eof" && !bytes.Equal(data, tc.data) {
				t.Fatal("normal evidence truncated")
			}
			if tc.name == "cap" && len(data) != maxBytes {
				t.Fatal("cap changed")
			}
			if tc.name == "short_mp3" {
				r := checkResource(context.Background(), &http.Client{Transport: fixtureTransport{data}}, "https://fixture.example/audio", nil)
				if !r.Complete || r.SHA256 != digest(tc.data) || r.DurationMethod != "mp3_complete_frames" || *r.DurationMS != 7128 {
					t.Fatal(r)
				}
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestStagedReadFailureAndBudgetAreNotEOF(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, io.ErrClosedPipe} {
		_, stop, err := readResource(failingReader{failure}, func(int) error { return nil }, "")
		if stop != "read_error" || !errors.Is(err, failure) {
			t.Fatal(stop, err)
		}
	}
	accounted := 0
	data, stop, err := readResource(bytes.NewReader(make([]byte, 1<<20)), func(n int) error { accounted += n; return ErrLimit }, "")
	if !errors.Is(err, ErrLimit) || stop != "read_error" || len(data) != 64<<10 || accounted != len(data) {
		t.Fatal(stop, err, accounted)
	}
}
