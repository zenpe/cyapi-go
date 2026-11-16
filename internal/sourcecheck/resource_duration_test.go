package sourcecheck

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sevenSecondMP3() []byte {
	frame := make([]byte, 576)
	copy(frame, []byte{0xff, 0xfb, 0xb4, 0}) // 48 kHz, 192 kbps, 1152 samples/frame
	return bytes.Repeat(frame, 297)
}
func TestCompleteHeaderlessMP3Duration(t *testing.T) {
	audio := sevenSecondMP3()
	for _, tc := range []struct {
		name, contentRange string
		status             int
		complete           bool
	}{
		{"full response", "", 200, true},
		{"full range", "bytes 0-171071/171072", 206, true},
		{"prefix of larger resource", "bytes 0-171071/6000000", 206, false},
		{"unknown total", "bytes 0-171071/*", 206, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				w.WriteHeader(tc.status)
				w.Write(audio)
			}))
			defer server.Close()
			expected := int64(212000)
			got := checkResource(context.Background(), server.Client(), server.URL, &expected)
			if got.Complete != tc.complete {
				t.Fatalf("%+v", got)
			}
			if tc.complete {
				if got.DurationMS == nil || *got.DurationMS != 7128 || got.DurationMethod != "mp3_complete_frames" || got.Assessment != "suspected_short_resource" || got.SHA256 != digest(audio) {
					t.Fatalf("%+v", got)
				}
			} else if got.DurationMS != nil || got.Assessment != "unknown" || got.SHA256 != "" {
				t.Fatalf("partial treated as full: %+v", got)
			}
		})
	}
}
func TestMP3ScanRequiresWholeFramesAndUnderstandsID3(t *testing.T) {
	audio := sevenSecondMP3()
	id3 := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 5}, make([]byte, 5)...)
	tagged := append(id3, audio...)
	tag := make([]byte, 128)
	copy(tag, "TAG")
	tagged = append(tagged, tag...)
	if ms, ok := completeMP3Duration(tagged); !ok || ms != 7128 {
		t.Fatalf("%d %v", ms, ok)
	}
	for _, b := range [][]byte{audio[:len(audio)-1], append(append([]byte{}, audio...), 1), audio[:100], []byte("ID3")} {
		if ms, ok := completeMP3Duration(b); ok {
			t.Fatalf("invalid tail/partial accepted: %d", ms)
		}
	}
	response := &http.Response{StatusCode: 200, ContentLength: int64(len(audio)), Header: make(http.Header)}
	if completeAudioResponse(response, len(audio), true) {
		t.Fatal("read budget treated as EOF")
	}
}
func TestWorkerDerivesIntervalForPreviouslySavedSamples(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(sevenSecondMP3()) }))
	defer server.Close()
	script := `lx.on(lx.EVENT_NAMES.request,({info})=>{if(info.musicInfo.interval!=='03:32')throw Error('missing interval');return ` + jsJSON(server.URL) + `;});lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`
	job := fixtureJob(t, script)
	duration := int64(212000)
	job.Sample.ExpectedDurationMS = &duration
	job.Sample.MusicInfo["interval"] = "00:07"
	got := executeJob(context.Background(), job, server.Client())
	if got.Resolution != "ok" || got.Outcome != OutcomeShort {
		t.Fatalf("%+v", got)
	}
	if job.Sample.MusicInfo["interval"] != "00:07" {
		t.Fatal("mutated persisted sample")
	}
	job.Sample.ExpectedDurationMS = nil
	if _, ok := job.Sample.lxMusicInfo()["interval"]; ok {
		t.Fatal("untrusted alias supplied without expected duration")
	}
}
