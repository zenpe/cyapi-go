package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type release struct {
	ID                int64  `json:"id"`
	Tag               string `json:"tag_name"`
	Name              string `json:"name"`
	Draft, Prerelease bool
	Assets            []asset
}
type asset struct {
	ID        int64
	Name      string
	Size      int64
	URL       string `json:"browser_download_url"`
	UpdatedAt string `json:"updated_at"`
	Digest    string
}

func allowedURL(scheme, host string, user bool) bool {
	if scheme != "https" || user {
		return false
	}
	switch host {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	}
	return false
}
func (m *Manager) get(ctx context.Context, address string, api bool) (*http.Response, error) {
	u, e := url.Parse(address)
	if e != nil || !allowedURL(u.Scheme, u.Host, u.User != nil) {
		return nil, errors.New("source URL rejected")
	}
	for attempt := 0; attempt < 3; attempt++ {
		work := ctx
		cancel := func() {}
		if api {
			work, cancel = context.WithTimeout(ctx, 45*time.Second)
		}
		req, e := http.NewRequestWithContext(work, http.MethodGet, address, nil)
		if e != nil {
			cancel()
			return nil, errors.New("invalid source request")
		}
		req.Header.Set("User-Agent", "cyapi-go-source-sync/1")
		req.Header.Set("Accept", "application/octet-stream")
		if api {
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			if m.o.Token != "" {
				req.Header.Set("Authorization", "Bearer "+m.o.Token)
			}
		}
		started := time.Now()
		trace := newRequestTrace(u.Host)
		req = req.WithContext(trace.context(req.Context()))
		response, e := m.client.Do(req)
		if e != nil {
			cancel()
			return nil, sourceRequestError(ctx, trace, api, attempt+1, started, e)
		}
		if response.StatusCode == 200 {
			trace.set("body", response.Request.URL.Host)
			response.Body = &sourceResponseBody{ReadCloser: response.Body, cancel: cancel, ctx: ctx, trace: trace, api: api, attempt: attempt + 1, started: started}
			return response, nil
		}
		status := response.StatusCode
		response.Body.Close()
		cancel()
		slog.Warn("source request rejected", "operation", requestOperation(api), "host", trace.hostName(), "status", status, "attempt", attempt+1, "elapsed_ms", time.Since(started).Milliseconds())
		if (status != 429 && status < 500) || attempt == 2 {
			return nil, fmt.Errorf("GitHub returned HTTP %d", status)
		}
		delay := time.Duration(1<<attempt) * time.Second
		if seconds, e := strconv.Atoi(response.Header.Get("Retry-After")); e == nil && seconds >= 0 {
			delay = time.Duration(seconds) * time.Second
		}
		if when, e := http.ParseTime(response.Header.Get("Retry-After")); e == nil {
			delay = time.Until(when)
		}
		if delay < 0 {
			delay = 0
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("GitHub retry limit reached")
}
func (m *Manager) latest(ctx context.Context) (release, asset, error) {
	response, e := m.get(ctx, "https://api.github.com/repos/"+m.o.Repo+"/releases?per_page=20", true)
	if e != nil {
		return release{}, asset{}, e
	}
	defer response.Body.Close()
	var releases []release
	if e = readJSON(response.Body, &releases, 4<<20); e != nil {
		var failure *requestFailure
		if body, ok := response.Body.(*sourceResponseBody); ok && errors.As(body.failure, &failure) {
			return release{}, asset{}, failure
		}
		return release{}, asset{}, errors.New("invalid or oversized GitHub release response")
	}
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		if r.Tag == "" {
			r.Tag = r.Name
		}
		if r.Tag == "" {
			r.Tag = strconv.FormatInt(r.ID, 10)
		}
		for _, a := range r.Assets {
			if strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
				if r.ID <= 0 || a.ID <= 0 || a.Size < 0 || a.Size > m.o.MaxZipBytes {
					return r, a, errors.New("invalid release asset or ZIP exceeds limit")
				}
				u, e := url.Parse(a.URL)
				if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || !strings.HasPrefix(u.Path, "/"+m.o.Repo+"/releases/download/") {
					return r, a, errors.New("release asset URL does not match approved repository")
				}
				return r, a, nil
			}
		}
		return r, asset{}, errors.New("latest stable release has no ZIP asset")
	}
	return release{}, asset{}, errors.New("no stable release found in latest 20 releases")
}

// CheckGitHub uses the same proxy, token and transport policy as synchronization.
func (m *Manager) CheckGitHub(ctx context.Context) error {
	response, err := m.get(ctx, "https://api.github.com/repos/"+m.o.Repo+"/releases?per_page=1", true)
	if err != nil {
		return err
	}
	return response.Body.Close()
}
func (m *Manager) download(ctx context.Context, a asset, target *os.File) (int64, error) {
	response, e := m.get(ctx, a.URL, false)
	if e != nil {
		return 0, e
	}
	defer response.Body.Close()
	if response.ContentLength > m.o.MaxZipBytes {
		return 0, errors.New("ZIP exceeds download limit")
	}
	n, e := io.Copy(&diskWriter{m: m, w: target, left: m.o.MaxZipBytes}, io.LimitReader(contextReader{ctx, response.Body}, m.o.MaxZipBytes+1))
	if e != nil {
		var failure *requestFailure
		if errors.As(e, &failure) {
			return n, failure
		}
		return n, errors.New("ZIP download interrupted")
	}
	if n > m.o.MaxZipBytes || a.Size > 0 && n != a.Size {
		return n, errors.New("ZIP size does not match release metadata or limit")
	}
	return n, target.Sync()
}
