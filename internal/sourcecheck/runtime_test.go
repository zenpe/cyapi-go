package sourcecheck

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testSession(t *testing.T) *session {
	t.Helper()
	s, e := newSession(context.Background(), &http.Client{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.close)
	return s
}
func TestAsyncHTTPAndBrowserFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "lx-music-request" {
			t.Error("UA mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":2,"message":"business failure","ok":false}`))
	}))
	defer server.Close()
	for _, browser := range []bool{false, true} {
		s := testSession(t)
		s.deadline = time.Now().Add(2 * time.Second)
		code := `lx.on(lx.EVENT_NAMES.request,async function(){await new Promise(r=>setTimeout(r,10));const result=await lx.request(` + jsJSON(server.URL) + `,{json:true});if(result.body.ok!==false||result.resp.statusCode!==200)throw Error('shape mismatch');return result.body.message;});lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`
		if browser {
			code = `lx.on(lx.EVENT_NAMES.request,async function(){const response=await fetch(` + jsJSON(server.URL) + `);const body=await response.json();if(body.ok!==false)throw Error('json false lost');return body.message;});lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{qualitys:['128k']}}});`
		}
		if e := s.eval("fixture", code); e != nil {
			t.Fatal(e)
		}
		if s.ready == nil {
			t.Fatal("not initialized")
		}
		if e := s.eval("request", `__lx_sendRequest__({requestKey:'test',source:'tx',action:'musicUrl',info:{musicInfo:{},type:'128k'}})`); e != nil {
			t.Fatal(e)
		}
		if e := s.pump(func() bool { return s.response != nil }); e != nil {
			t.Fatal(e)
		}
		if s.response["url"] != "business failure" {
			t.Fatal(s.response)
		}
		if len(s.events) != 1 || (!browser && s.events[0]["businessCode"] != "2") {
			t.Fatal(s.events)
		}
	}
}
func TestUtilityContract(t *testing.T) {
	s := testSession(t)
	k, e := rsa.GenerateKey(rand.Reader, 1024)
	if e != nil {
		t.Fatal(e)
	}
	public, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))
	s.vm.Set("testPublicKey", key)
	digest := md5.Sum([]byte("音源测试"))
	s.vm.Set("expectedMD5", hex.EncodeToString(digest[:]))
	code := `(async function(){const u=lx.utils,b=u.buffer,c=u.crypto;if(c.md5('音源测试')!==expectedMD5)throw Error('md5');const bytes=b.from('音源测试');const compressed=await u.zlib.deflate(bytes);const restored=await u.zlib.inflate(compressed);if(b.bufToString(restored)!=='音源测试')throw Error('zlib');if(b.bufToString(new Uint8Array([0,255,16]),'hex')!=='00ff10')throw Error('bytes');const encrypted=c.aesEncrypt(b.from('6bc1bee22e409f96e93d7e117393172a','hex'),'aes-128-cbc',b.from('2b7e151628aed2a6abf7158809cf4f3c','hex'),b.from('000102030405060708090a0b0c0d0e0f','hex'));if(b.bufToString(encrypted,'hex').slice(0,32)!=='7649abac8119b246cee98e9b12e9197d')throw Error('aes-vector:'+b.bufToString(encrypted,'hex'));if(c.rsaEncrypt(new Uint8Array([1]),testPublicKey).length!==128)throw Error('rsa');if(c.randomBytes(16).length!==16)throw Error('random');globalThis.utilityDone=true;})().catch(e=>globalThis.utilityError=String(e));`
	if e = s.eval("utilities", code); e != nil {
		t.Fatal(e)
	}
	v := s.vm.Get("utilityError")
	if v != nil && !strings.Contains(v.String(), "undefined") {
		t.Fatal(v)
	}
	if v = s.vm.Get("utilityDone"); v == nil || !v.ToBoolean() {
		t.Fatal("utility promise did not settle")
	}
}
func TestTimeoutInterruptAndAbort(t *testing.T) {
	s := testSession(t)
	s.cpu = 30 * time.Millisecond
	started := time.Now()
	if e := s.eval("infinite", `while(true){}`); e == nil {
		t.Fatal("infinite loop was not interrupted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("interrupt too slow")
	}
	if e := s.eval("after-interrupt", `globalThis.recovered=1`); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	s.deadline = time.Now().Add(time.Second)
	if e := s.eval("abort", `var abort=lx.request(`+jsJSON(server.URL)+`,{},()=>{throw Error('cancelled callback ran')});setTimeout(abort,20)`); e != nil {
		t.Fatal(e)
	}
	if e := s.pump(func() bool { return len(s.events) > 0 }); e != nil {
		t.Fatal(e)
	}
	if len(s.requests) != 0 {
		t.Fatal("request retained after abort")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, _ := performHTTP(ctx, s.client, server.URL, map[string]any{})
	if result["success"] != false {
		t.Fatal(result)
	}
}
