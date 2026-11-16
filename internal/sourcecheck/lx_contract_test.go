package sourcecheck

import (
	"compress/gzip"
	"context"
	"cyapi-go/internal/music"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProductionLXRawGzipMultipartContract(t *testing.T) {
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			z := gzip.NewWriter(w)
			_, _ = z.Write([]byte(`{"ok":true}`))
			_ = z.Close()
		case "/upload":
			if e := r.ParseMultipartForm(1024); e != nil {
				t.Error(e)
			}
			f, _, e := r.FormFile("file")
			if e != nil {
				t.Error(e)
				return
			}
			defer f.Close()
			b, _ := io.ReadAll(f)
			if string(b) != string([]byte{0, 255, 1, 128}) || r.FormValue("name") != "中文" {
				t.Error("multipart changed")
			}
			uploads++
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			_, _ = w.Write([]byte{0, 255, 1, 128})
		}
	}))
	defer server.Close()
	s := testSession(t)
	code := `lx.on(lx.EVENT_NAMES.request,async()=>{
 const {resp}=await lx.request(` + jsJSON(server.URL+"/binary") + `,{});
 if(resp.bytes!==4||resp.raw.toString('hex')!=='00ff0180'||resp.raw.slice(1,3).toString('hex')!=='ff01')throw Error('raw contract');
 const zipped=await lx.request(` + jsJSON(server.URL+"/gzip") + `,{});if(zipped.body.ok!==true||zipped.resp.raw.readUInt16BE(0)!==0x1f8b||zipped.resp.bytes!==zipped.resp.raw.length)throw Error('gzip contract');
 const result=await lx.request(` + jsJSON(server.URL+"/upload") + `,{method:'POST',formData:{name:'中文',file:{buffer:lx.utils.buffer.from('00ff0180','hex'),filename:'sample.bin'}}});if(!result.body.ok)throw Error('upload');return 'ok';});`
	if e := s.eval("contract", code); e != nil {
		t.Fatal(e)
	}
	if e := s.eval("request", `__lx_sendRequest__({requestKey:'test',source:'tx',action:'musicUrl',info:{musicInfo:{},type:'128k'}})`); e != nil {
		t.Fatal(e)
	}
	if e := s.pump(func() bool { return s.response != nil }); e != nil {
		t.Fatal(e)
	}
	if s.response["url"] != "ok" || uploads != 1 {
		t.Fatal(s.response, uploads)
	}
}
func TestSampleMetadataPreservesLXFields(t *testing.T) {
	s, e := SampleFromMetadata("qq", Metadata{TrackID: "MID1", Title: "Song", Artist: "Artist", DurationMS: 212000, LXMetadata: music.LXMetadata{SongID: "12345", MediaMID: "MEDIA1"}})
	if e != nil {
		t.Fatal(e)
	}
	info := s.lxMusicInfo()
	if info["songId"] != int64(12345) || info["strMediaMid"] != "MEDIA1" || info["interval"] != "03:32" {
		t.Fatal(info)
	}
}
func TestMultipartRejectsPathObjectsAndOversizedPayload(t *testing.T) {
	for _, v := range []any{map[string]any{"file": map[string]any{"file": "/etc/passwd"}}, map[string]any{"big": strings.Repeat("x", maxBytes+1)}} {
		out, _ := performHTTP(context.Background(), &http.Client{}, "https://example.invalid", map[string]any{"method": "POST", "formData": v})
		if out["success"] != false {
			t.Fatal(out)
		}
	}
}
