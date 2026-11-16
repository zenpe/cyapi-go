package sourcecheck

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixtureJob(t *testing.T, body string) Job {
	t.Helper()
	set, _, e := DecodeSamples(strings.NewReader(string(defaultSamples)))
	if e != nil {
		t.Fatal(e)
	}
	return Job{Format: 1, ID: "test", ScriptPath: "fixture.js", Script: []byte(body), ScriptSHA256: digest([]byte(body)), Sample: set.Samples[0], Quality: "128k"}
}

func TestBlockedFallbackDoesNotOverwriteFinalScriptError(t *testing.T) {
	client := &http.Client{Transport: testRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("probe_policy: non-public destination (resolved address 198.18.0.1, category benchmark_or_fake_ip)")
	})}
	script := `lx.on(lx.EVENT_NAMES.request,async()=>{try{await lx.request('https://blocked.example',{json:true});}catch(e){}throw Error('QQ 无播放权限(104003)');});lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`
	r := executeJob(context.Background(), fixtureJob(t, script), client)
	if r.Outcome != OutcomeFailed || r.FailureStage != "resolution" || r.FailureOrigin != "source" || !strings.Contains(r.Reason, "104003") || strings.Contains(r.Reason, "probe_policy") || len(r.ProbeIssues) != 1 || r.Resource.Status != "not_run" {
		t.Fatalf("%+v", r)
	}
}
func TestJobEvidenceAndUnsupportedQuality(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(flacFixture(60)) }))
	defer server.Close()
	raw := `lx.on(lx.EVENT_NAMES.request,()=>` + jsJSON(server.URL) + `);lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`
	j := fixtureJob(t, raw)
	expected := int64(269000)
	j.Sample.ExpectedDurationMS = &expected
	j.Sample.DurationEvidence = "test fixture"
	r := executeJob(context.Background(), j, server.Client())
	if r.Resolution != "ok" || r.Resource.Assessment != "suspected_short_resource" {
		t.Fatalf("%+v", r)
	}
	r = executeJob(context.Background(), fixtureJob(t, strings.ReplaceAll(raw, "128k", "flac")), server.Client())
	if r.Initialization != "unsupported" || r.Resolution != "not_run" {
		t.Fatalf("quality silently changed: %+v", r)
	}
	r = executeJob(context.Background(), fixtureJob(t, `lx.on(lx.EVENT_NAMES.request,()=>{throw Error('service retired https://host/path?token=secret')});lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`), server.Client())
	if r.Resolution != "failed" || !strings.Contains(r.Reason, "service retired") || strings.Contains(r.Reason, "secret") {
		t.Fatalf("%+v", r)
	}
}
func TestSamplesAndDigestValidation(t *testing.T) {
	largeID := strings.ReplaceAll(string(defaultSamples), "186016", "9999999999")
	if _, _, e := DecodeSamples(strings.NewReader(largeID)); e != nil {
		t.Fatalf("large numeric song id lost precision or representation: %v", e)
	}
	_, _, e := DecodeSamples(strings.NewReader(strings.Replace(string(defaultSamples), `"songmid": "002X3Bu82FiBhb"`, `"songmid": "wrong"`, 1)))
	if e == nil {
		t.Fatal("mismatched sample accepted")
	}
	j := fixtureJob(t, "1")
	j.Script = []byte("2")
	if j.validate() == nil {
		t.Fatal("bad digest accepted")
	}
	c := newHTTPClient()
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := executeJob(ctx, fixtureJob(t, `setTimeout(()=>{},10000)`), c)
	if r.FailureOrigin == "" {
		t.Fatal("canceled worker not reported")
	}
}
