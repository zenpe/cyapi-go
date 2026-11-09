package sources

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSourceTransportTimeoutsAndDedicatedProxy(t *testing.T) {
	o := DefaultOptions(t.TempDir())
	o.ProxyURL = "socks5://user:private-password@127.0.0.1:1080"
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	transport := m.client.Transport.(*http.Transport)
	proxy, err := transport.Proxy(&http.Request{})
	if err != nil || proxy.String() != o.ProxyURL {
		t.Fatal("dedicated proxy not applied")
	}
	if transport == http.DefaultTransport || m.client.Timeout != 0 || transport.TLSHandshakeTimeout != 15*time.Second || transport.ResponseHeaderTimeout != 30*time.Second {
		t.Fatal("unexpected shared transport mutation or total download timeout")
	}
	for _, value := range []string{"file:///private-password", "https://user:private-password@", "http://user:private-password@localhost:8080/path", "http://localhost:8080?token=private-password"} {
		o.ProxyURL = value
		_, err = New(o)
		if err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatal("invalid proxy accepted or exposed")
		}
	}
}

func TestSourceWithoutProxyIgnoresEnvironment(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "http://127.0.0.1:7890")
	}
	m, err := New(DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if m.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("empty proxy_url must use a direct connection")
	}
}

func TestSourceNetworkFailureIsDiagnosableAndNotRetried(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, item := range []struct {
		cause  error
		reason string
	}{
		{&net.DNSError{Err: "private-password signed-query", Name: "private-host"}, "dns_failure"},
		{&net.DNSError{Err: "private-password", IsTimeout: true}, "timeout"},
		{&tls.CertificateVerificationError{Err: errors.New("private-password")}, "tls_certificate"},
	} {
		calls := 0
		o := DefaultOptions(t.TempDir())
		o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, item.cause })}
		m, _ := New(o)
		_, err := m.get(context.Background(), "https://github.com/download?token=signed-query", false)
		if err == nil || calls != 1 || !strings.Contains(err.Error(), "reason="+item.reason) || !strings.Contains(err.Error(), "zip_download") {
			t.Fatal(calls, err)
		}
		if strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), "signed-query") {
			t.Fatal("URL or underlying error leaked")
		}
	}
	if strings.Contains(logs.String(), "private-password") || strings.Contains(logs.String(), "signed-query") {
		t.Fatal("secret leaked into diagnostics")
	}
}

type brokenNetworkBody struct{}

func (brokenNetworkBody) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
}
func (brokenNetworkBody) Close() error { return nil }

func TestSourceBodyFailureRetainsOperationAndStage(t *testing.T) {
	for _, api := range []bool{false, true} {
		calls := 0
		o := DefaultOptions(t.TempDir())
		o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			resp := reply(r, 200, nil)
			resp.Body = brokenNetworkBody{}
			return resp, nil
		})}
		m, _ := New(o)
		m.free = func(string) (uint64, error) { return 1 << 40, nil }
		var err error
		if api {
			_, _, err = m.latest(context.Background())
		} else {
			file, e := os.CreateTemp(t.TempDir(), "download")
			if e != nil {
				t.Fatal(e)
			}
			_, err = m.download(context.Background(), asset{URL: "https://github.com/file.zip"}, file)
			file.Close()
		}
		if err == nil || calls != 1 || !strings.Contains(err.Error(), "stage=body") || !strings.Contains(err.Error(), requestOperation(api)) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(calls, err)
		}
	}
}

func TestSourceMetadataDeadlineLivesUntilBodyClose(t *testing.T) {
	var requestContext context.Context
	o := DefaultOptions(t.TempDir())
	o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		requestContext = r.Context()
		return reply(r, 200, []byte("[]")), nil
	})}
	m, _ := New(o)
	response, err := m.get(context.Background(), "https://api.github.com/releases", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline, ok := requestContext.Deadline()
	if !ok || time.Until(deadline) > 45*time.Second || requestContext.Err() != nil {
		t.Fatal("invalid metadata deadline")
	}
	if _, err = io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requestContext.Err() == nil {
		t.Fatal("metadata deadline was not released")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	o.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	m, _ = New(o)
	_, err = m.get(ctx, "https://api.github.com/releases", true)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "sync_timeout") {
		t.Fatal(err)
	}
}
