package sources

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// Never include raw transport errors: redirected asset URLs contain signatures,
// and proxy errors can include credentials. Preserve the cause for errors.Is.
type requestFailure struct {
	operation, host, stage, reason string
	attempt                        int
	elapsed                        time.Duration
	cause                          error
}

func (e *requestFailure) Error() string {
	return fmt.Sprintf("GitHub %s failed: host=%s stage=%s reason=%s attempt=%d elapsed_ms=%d", e.operation, e.host, e.stage, e.reason, e.attempt, e.elapsed.Milliseconds())
}
func (e *requestFailure) Unwrap() error { return e.cause }

func requestOperation(api bool) string {
	if api {
		return "release_metadata"
	}
	return "zip_download"
}

type requestTrace struct {
	mu          sync.Mutex
	host, stage string
}

func newRequestTrace(host string) *requestTrace { return &requestTrace{host: host, stage: "connect"} }
func (t *requestTrace) set(stage, host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stage = stage
	if host != "" {
		t.host = host
	}
}
func (t *requestTrace) hostName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.host
}
func (t *requestTrace) context(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { t.set("dns", "") },
		ConnectStart:      func(_, _ string) { t.set("connect", "") },
		TLSHandshakeStart: func() { t.set("tls", "") },
		GotConn:           func(httptrace.GotConnInfo) { t.set("response_headers", "") },
	})
}

func sourceRequestError(ctx context.Context, trace *requestTrace, api bool, attempt int, started time.Time, err error) error {
	reason := "transport_failure"
	var network net.Error
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		reason = "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		reason = "sync_timeout"
	case errors.As(err, &certificate):
		reason = "tls_certificate"
	case errors.Is(err, errSourceRedirect):
		reason = "redirect_rejected"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	case errors.As(err, &network) && network.Timeout():
		reason = "timeout"
	case errors.As(err, &dns):
		reason = "dns_failure"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		reason = "connection_closed"
	default:
		var op *net.OpError
		if errors.As(err, &op) {
			switch op.Op {
			case "dial":
				reason = "connection_failed"
			case "proxyconnect":
				reason = "proxy_connection_failed"
			case "read":
				reason = "connection_read_failed"
			case "write":
				reason = "connection_write_failed"
			}
		}
	}
	trace.mu.Lock()
	host := trace.host
	var upstream *url.Error
	if errors.As(err, &upstream) {
		if target, parseErr := url.Parse(upstream.URL); parseErr == nil && allowedURL(target.Scheme, target.Host, target.User != nil) {
			host = target.Host
		}
	}
	failure := &requestFailure{operation: requestOperation(api), host: host, stage: trace.stage, reason: reason, attempt: attempt, elapsed: time.Since(started), cause: err}
	trace.mu.Unlock()
	slog.Warn("source network request failed", "operation", failure.operation, "host", failure.host, "stage", failure.stage, "reason", failure.reason, "attempt", failure.attempt, "elapsed_ms", failure.elapsed.Milliseconds())
	return failure
}

type sourceResponseBody struct {
	io.ReadCloser
	cancel  context.CancelFunc
	ctx     context.Context
	trace   *requestTrace
	api     bool
	attempt int
	started time.Time
	failure error
}

func (b *sourceResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		if b.failure == nil {
			b.failure = sourceRequestError(b.ctx, b.trace, b.api, b.attempt, b.started, err)
		}
		return n, b.failure
	}
	return n, err
}
func (b *sourceResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
