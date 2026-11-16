package sourcecheck

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAudioHeadersFollowHost(t *testing.T) {
	for host, referer := range map[string]string{
		"isure.stream.qqmusic.qq.com": "https://y.qq.com/",
		"dl.stream.music.tc.qq.com":   "https://y.qq.com/",
		"fs.kugou.com":                "https://www.kugou.com/",
		"cdn-img.gitcode.com":         "",
		"qqmusic.qq.com.evil.test":    "",
		"fakeqqmusic.qq.com":          "",
	} {
		req, _ := http.NewRequest("GET", "https://"+host+"/audio", nil)
		req.Header.Set("Referer", "stale")
		req.Header.Set("Origin", "stale")
		applyAudioHeaders(req)
		if req.Header.Get("Referer") != referer {
			t.Fatalf("%s: %v", host, req.Header)
		}
		if referer == "" && req.Header.Get("Origin") != "" {
			t.Fatal(req.Header)
		}
	}
}

type audioRoundTrip func(*http.Request) (*http.Response, error)

func (f audioRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAudioResourceRedirectRecomputesHeaders(t *testing.T) {
	guardCalls := 0
	calls := 0
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error { guardCalls++; return nil },
		Transport: audioRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Range") != "bytes=0-8388607" {
				t.Fatal(req.Header)
			}
			if calls == 1 {
				if req.Header.Get("Referer") != "https://y.qq.com/" {
					t.Fatal(req.Header)
				}
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://cdn-img.gitcode.com/audio"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			}
			if req.Header.Get("Referer") != "" || req.Header.Get("Origin") != "" {
				t.Fatal(req.Header)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/flac"}}, Body: io.NopCloser(strings.NewReader(string(flacFixture(212)))), Request: req}, nil
		}),
	}
	result := checkResource(context.Background(), client, "https://isure.stream.qqmusic.qq.com/audio", nil)
	if result.Status != "recognized_audio" || calls != 2 || guardCalls != 1 {
		t.Fatalf("%+v, calls=%d guards=%d", result, calls, guardCalls)
	}
}

func TestBinaryMediaReadLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte{255, 251, 144, 0, 128})
	}))
	defer srv.Close()
	out, _ := performHTTP(context.Background(), srv.Client(), srv.URL, map[string]any{"encoding": nil, "maxResponseBytes": 5})
	if out["success"] != true {
		t.Fatal(out)
	}
	body, ok := out["body"].([]int)
	if !ok || len(body) != 5 || body[0] != 255 {
		t.Fatal(out)
	}
	out, _ = performHTTP(context.Background(), srv.Client(), srv.URL, map[string]any{"encoding": nil, "maxResponseBytes": 4})
	if out["success"] != false {
		t.Fatal("expected read budget failure", out)
	}
}

func TestWorkerBinaryAudioProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" || r.Header.Get("Origin") != "" {
			t.Error("unexpected platform headers", r.Header)
		}
		w.Header().Set("Content-Type", "audio/flac")
		w.Write(flacFixture(269))
	}))
	defer server.Close()
	script := `lx.on(lx.EVENT_NAMES.request,()=>new Promise((resolve,reject)=>{lx.request(` + jsJSON(server.URL) + `,{encoding:null,audioResource:true,maxResponseBytes:4096,headers:{Range:'bytes=0-4095',Referer:'https://y.qq.com/'}},(error,response)=>{if(error)return reject(error);if(!Array.isArray(response.body)||response.body[0]!==102)return reject(Error('binary mismatch'));resolve(` + jsJSON(server.URL) + `);});}));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`
	result := executeJob(context.Background(), fixtureJob(t, script), server.Client())
	if result.Resolution != "ok" || result.Resource.Status != "recognized_audio" {
		t.Fatalf("%+v", result)
	}
}
