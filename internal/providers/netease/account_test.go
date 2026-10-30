package netease

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type accountTransport func(*http.Request) (*http.Response, error)

func (f accountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAccountCookieIsRequestLocal(t *testing.T) {
	client, e := New(Options{Cookie: "MUSIC_U=global", MaxConcurrent: 2, RequestTimeout: time.Second, Client: &http.Client{Transport: accountTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "music.163.com" || r.URL.Path != "/weapi/w/nuser/account/get" {
			t.Fatal("unexpected endpoint")
		}
		if r.Header.Get("Cookie") != "MUSIC_U=personal; __csrf=csrf" {
			t.Fatal("global cookie leaked into account request")
		}
		if e := r.ParseForm(); e != nil {
			t.Fatal(e)
		}
		if r.Form.Get("params") == "" || len(r.Form.Get("encSecKey")) != 256 || r.URL.Query().Get("csrf_token") != "csrf" {
			t.Fatal("invalid weapi envelope")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"MUSIC_U=new; HttpOnly; Secure; Path=/"}}, Body: io.NopCloser(strings.NewReader(`{"code":200,"profile":{"userId":123}}`))}, nil
	})}})
	if e != nil {
		t.Fatal(e)
	}
	_, cookie, e := client.AccountRequest(context.Background(), "MUSIC_U=personal; __csrf=csrf", "/w/nuser/account/get", map[string]any{})
	if e != nil || cookie != "MUSIC_U=new" {
		t.Fatal(e)
	}
	if client.cookie != "MUSIC_U=global" {
		t.Fatal("shared cookie mutated")
	}
	if _, _, e = client.AccountRequest(context.Background(), "private", "//evil.test/?secret", nil); e == nil {
		t.Fatal("arbitrary URL allowed")
	}
}
func TestOfficialQRConnectivity(t *testing.T) {
	if os.Getenv("CYAPI_NETEASE_LIVE") != "1" {
		t.Skip("explicit live probe only; no account credentials")
	}
	client, e := New(Options{MaxConcurrent: 1, RequestTimeout: 15 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	out, _, e := client.AccountRequest(context.Background(), "", "/login/qrcode/unikey", map[string]any{"type": 1})
	if e != nil {
		t.Fatal("official QR request unavailable")
	}
	data, _ := out["data"].(map[string]any)
	key, _ := data["unikey"].(string)
	if key == "" {
		key, _ = out["unikey"].(string)
	}
	if out["code"] != float64(200) || key == "" {
		t.Fatalf("QR unavailable, official code=%v", out["code"])
	}
	state, _, e := client.AccountRequest(context.Background(), "", "/login/qrcode/client/login", map[string]any{"key": key, "type": 1})
	if e != nil || state["code"] != float64(801) {
		t.Fatalf("official QR waiting-state unavailable (code=%v)", state["code"])
	}
	t.Log("official QR creation and waiting-state available; account confirmation not tested")
}
