package sourcecheck

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddresses(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.2", "169.254.169.254", "100.100.100.200", "198.18.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Fatal("accepted", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(s)) {
			t.Fatal("rejected", s)
		}
	}
	c := newHTTPClient()
	defer c.CloseIdleConnections()
	_, e := c.Get("http://127.0.0.1:1/")
	if e == nil || !strings.Contains(e.Error(), "non-public") {
		t.Fatal(e)
	}
	if !strings.Contains(e.Error(), "127.0.0.1") || !strings.Contains(e.Error(), "loopback") {
		t.Fatalf("missing policy evidence: %v", e)
	}
	_, e = c.Get("file:///etc/passwd")
	if e == nil {
		t.Fatal("accepted file URL")
	}
}
func TestRequestBudget(t *testing.T) {
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, context.Canceled })
	c := &http.Client{Transport: &budgetTransport{base: base, slots: make(chan struct{}, 4)}}
	for i := 0; i < 64; i++ {
		c.Get("https://example.com")
	}
	_, e := c.Get("https://example.com")
	if e == nil || !strings.Contains(e.Error(), "probe_limit") {
		t.Fatal(e)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
