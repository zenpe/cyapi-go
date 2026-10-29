// Package platformhttp bounds platform requests and keeps vendor response parsing out of business modules.
package platformhttp

import (
	"context"
	"crypto/md5"
	"cyapi-go/internal/music"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Options struct {
	Cookie        string
	MaxConcurrent int
	Timeout       time.Duration
	Client        *http.Client
}
type Client struct {
	http    *http.Client
	cookie  string
	slots   chan struct{}
	timeout time.Duration
	hosts   map[string]bool
}

func New(o Options, hosts ...string) (*Client, error) {
	if o.MaxConcurrent < 1 || o.MaxConcurrent > 16 || o.Timeout <= 0 || o.Timeout > 20*time.Second || len(o.Cookie) > 16384 || strings.ContainsAny(o.Cookie, "\r\n") {
		return nil, errors.New("invalid platform settings")
	}
	c := &http.Client{}
	if o.Client != nil {
		*c = *o.Client
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxConnsPerHost = o.MaxConcurrent
		t.MaxIdleConnsPerHost = o.MaxConcurrent
		t.TLSHandshakeTimeout = 5 * time.Second
		t.IdleConnTimeout = time.Minute
		c.Transport = t
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[h] = true
	}
	return &Client{c, o.Cookie, make(chan struct{}, o.MaxConcurrent), o.Timeout, allowed}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) Do(ctx context.Context, address string, payload any) (map[string]any, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || !c.hosts[u.Host] || u.User != nil {
		return nil, music.BadUpstream()
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, music.ErrTimeout
	}
	method, content := "GET", ""
	var body io.Reader
	if form, ok := payload.(url.Values); ok {
		method = "POST"
		body = strings.NewReader(form.Encode())
		content = "application/x-www-form-urlencoded"
	} else if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, music.BadUpstream()
		}
		method = "POST"
		body = strings.NewReader(string(data))
		content = "application/json"
	}
	req, e := http.NewRequestWithContext(ctx, method, address, body)
	if e != nil {
		return nil, music.BadUpstream()
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://"+u.Host+"/")
	req.Header.Set("Accept", "application/json")
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	if content != "" {
		req.Header.Set("Content-Type", content)
	}
	res, e := c.http.Do(req)
	if e != nil {
		var n net.Error
		if ctx.Err() != nil || errors.As(e, &n) && n.Timeout() {
			return nil, music.ErrTimeout
		}
		return nil, &music.UpstreamError{Status: 502, Retryable: true}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		status := 502
		if res.StatusCode == 429 {
			status = 429
		}
		after := res.Header.Get("Retry-After")
		if len(after) > 64 || strings.ContainsAny(after, "\r\n") {
			after = ""
		}
		return nil, &music.UpstreamError{Status: status, Retryable: status == 429 || res.StatusCode >= 500, RetryAfter: after}
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if e != nil {
		if ctx.Err() != nil {
			return nil, music.ErrTimeout
		}
		return nil, music.BadUpstream()
	}
	if len(raw) > 8<<20 {
		return nil, music.BadUpstream()
	}
	var out map[string]any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&out) != nil || out == nil || d.Decode(new(any)) != io.EOF {
		return nil, music.BadUpstream()
	}
	return out, nil
}
func Obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func Arr(v any) []any { a, _ := v.([]any); return a }
func Text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return ""
}
func Int(v any) int64          { n, _ := strconv.ParseInt(Text(v), 10, 64); return n }
func Code(v any, n int64) bool { _, ok := v.(json.Number); return ok && Int(v) == n }
func Hash(data []byte) string  { d := md5.Sum(data); return hex.EncodeToString(d[:]) }
func Cookie(value string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(value, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok {
			out[k] = v
		}
	}
	return out
}
func SecureMedia(value string, suffixes ...string) (string, bool) {
	u, e := url.Parse(value)
	if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" {
		return "", false
	}
	h := strings.ToLower(u.Hostname())
	for _, suffix := range suffixes {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			u.Scheme = "https"
			return u.String(), true
		}
	}
	return "", false
}
func Page(songs []any, ids []any, page, limit int, total any) map[string]any {
	var fp *string
	if len(ids) > 0 {
		b, _ := json.Marshal(ids)
		s := Hash(b)
		fp = &s
	}
	more := len(ids) > 0 && len(ids) >= limit
	if n, ok := total.(json.Number); ok && Int(n) >= 0 {
		more = len(ids) > 0 && int64(page*limit) < Int(n)
	}
	return map[string]any{"status": 200, "result": songs, "page": page, "resultKind": "full", "hasMore": more, "pageFingerprint": fp}
}
