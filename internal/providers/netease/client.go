package netease

import (
	"context"
	"cyapi-go/internal/music"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Safari/537.36 Chrome/91.0.4472.164 NeteaseMusicDesktop/2.10.2.200154"

type Options struct {
	Cookie         string
	MaxConcurrent  int
	RequestTimeout time.Duration
	Client         *http.Client
}
type Client struct {
	cookie  string
	http    *http.Client
	slots   chan struct{}
	timeout time.Duration
}

func New(o Options) (*Client, error) {
	if o.MaxConcurrent < 1 || o.MaxConcurrent > 16 || o.RequestTimeout <= 0 || o.RequestTimeout > 20*time.Second || len(o.Cookie) > 16384 || strings.ContainsAny(o.Cookie, "\r\n") {
		return nil, errors.New("invalid Netease request settings")
	}
	client := &http.Client{}
	if o.Client != nil {
		*client = *o.Client
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = o.MaxConcurrent
		transport.MaxConnsPerHost = o.MaxConcurrent
		transport.IdleConnTimeout = time.Minute
		transport.TLSHandshakeTimeout = 5 * time.Second
		transport.ResponseHeaderTimeout = o.RequestTimeout
		client.Transport = transport
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{o.Cookie, client, make(chan struct{}, o.MaxConcurrent), o.RequestTimeout}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) request(ctx context.Context, address string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return music.ErrTimeout
	}
	method := http.MethodGet
	var reader io.Reader
	if form != nil {
		method = http.MethodPost
		reader = strings.NewReader(form.Encode())
	}
	req, e := http.NewRequestWithContext(ctx, method, address, reader)
	if e != nil {
		return music.BadUpstream()
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	response, e := c.http.Do(req)
	if e != nil {
		var timeout net.Error
		if ctx.Err() != nil || errors.As(e, &timeout) && timeout.Timeout() {
			return music.ErrTimeout
		}
		return &music.UpstreamError{Status: 502, Retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := 502
		if response.StatusCode == 429 {
			status = 429
		}
		retry := response.StatusCode >= 500
		after := response.Header.Get("Retry-After")
		if len(after) > 64 || strings.ContainsAny(after, "\r\n") {
			after = ""
		}
		return &music.UpstreamError{Status: status, Retryable: retry, RetryAfter: after}
	}
	body, e := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if e != nil {
		if ctx.Err() != nil {
			return music.ErrTimeout
		}
		return &music.UpstreamError{Status: 502, Retryable: true}
	}
	if len(body) > 8<<20 {
		return music.BadUpstream()
	}
	if e = json.Unmarshal(body, out); e != nil {
		return music.BadUpstream()
	}
	return nil
}

var _ music.Provider = (*Client)(nil)
