package sourcecheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("64:ff9b::/96"),
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range deniedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil {
		return nil, e
	}
	if len(ips) == 0 {
		return nil, errors.New("no addresses")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("probe_policy: non-public destination")
		}
	}
	var last error
	for _, ip := range ips {
		c, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	return nil, last
}

type budgetTransport struct {
	base     http.RoundTripper
	requests atomic.Int32
	bytes    atomic.Int64
	decoded  atomic.Int64
	slots    chan struct{}
}

func accountDecoded(client *http.Client, n int) error {
	if t, ok := client.Transport.(*budgetTransport); ok && t.decoded.Add(int64(n)) > 32<<20 {
		return fmt.Errorf("%w: decoded transfer bytes", ErrLimit)
	}
	return nil
}

func (t *budgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" && r.URL.Scheme != "https" || r.URL.Hostname() == "" || r.URL.User != nil {
		return nil, errors.New("probe_policy: invalid HTTP destination")
	}
	if t.requests.Add(1) > 64 {
		return nil, fmt.Errorf("%w: request count", ErrLimit)
	}
	select {
	case t.slots <- struct{}{}:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	resp, e := t.base.RoundTrip(r)
	if e != nil {
		<-t.slots
		return nil, e
	}
	resp.Body = &budgetBody{ReadCloser: resp.Body, owner: t}
	return resp, nil
}

type budgetBody struct {
	io.ReadCloser
	owner  *budgetTransport
	closed bool
}

func (b *budgetBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if b.owner.bytes.Add(int64(n)) > 32<<20 {
		return n, fmt.Errorf("%w: transfer bytes", ErrLimit)
	}
	return n, e
}
func (b *budgetBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	e := b.ReadCloser.Close()
	<-b.owner.slots
	return e
}
func (t *budgetTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = publicDial
	tr.TLSHandshakeTimeout = 5 * time.Second
	tr.ResponseHeaderTimeout = 8 * time.Second
	tr.DisableCompression = true
	tr.MaxConnsPerHost = 4
	return &http.Client{Transport: &budgetTransport{base: tr, slots: make(chan struct{}, 4)}, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("probe_policy: redirect limit")
		}
		if len(via) > 0 && !strings.EqualFold(r.URL.Host, via[len(via)-1].URL.Host) {
			r.Header.Del("Authorization")
			r.Header.Del("Cookie")
			r.Header.Del("Proxy-Authorization")
		}
		return nil
	}}
}
