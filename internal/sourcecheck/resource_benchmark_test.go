package sourcecheck

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fixtureTransport struct{ body []byte }

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(f.body)), ContentLength: int64(len(f.body)), Request: r}, nil
}

func BenchmarkIsolatedWorkerLifecycle(b *testing.B) {
	executor := Executor{}
	if err := executor.Preflight(context.Background()); err != nil {
		b.Skip(err)
	}
	set, _, err := DecodeSamples(strings.NewReader(string(defaultSamples)))
	if err != nil {
		b.Fatal(err)
	}
	script := []byte(`lx.send(lx.EVENT_NAMES.inited,{status:false,message:'baseline-retired'});`)
	job := Job{Format: ProtocolVersion, ID: "baseline", ScriptPath: "baseline.js", ScriptSHA256: digest(script), Script: script, Sample: set.Samples[0], Quality: "128k"}
	var workerMS int64
	b.ReportAllocs()
	for b.Loop() {
		r := executor.Run(context.Background(), job)
		if r.Initialization != "failed" || r.FailureOrigin != "source" || !strings.Contains(r.Reason, "baseline-retired") {
			b.Fatalf("worker failed: %+v", r)
		}
		workerMS += r.ElapsedMS
	}
	b.ReportMetric(float64(workerMS)/float64(b.N), "worker-ms/op")
}

// No external network or codec dependency: repeatable reader/parser/hash costs.
// B/op is allocated bytes per operation, not peak RSS or worker startup cost.
func BenchmarkResourceFixtures(b *testing.B) {
	for name, body := range map[string][]byte{
		"normal_flac_2MiB": append(flacFixture(269), make([]byte, (2<<20)-60)...),
		"short_mp3_171072": sevenSecondMP3(),
		"encrypted_2MiB":   bytes.Repeat([]byte{0x7d, 0x35, 0x28, 0x01}, (2<<20)/4),
		"html_error_2MiB":  append([]byte("<!DOCTYPE html><html>"), bytes.Repeat([]byte("x"), (2<<20)-21)...),
	} {
		b.Run(name, func(b *testing.B) {
			client := &http.Client{Transport: fixtureTransport{body}}
			expected := int64(269000)
			b.ReportAllocs()
			for b.Loop() {
				got := checkResource(context.Background(), client, "https://fixture.example/audio", &expected)
				b.ReportMetric(float64(got.Bytes), "resource-bytes/op")
			}
		})
	}
}
