package sourcecheck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// WorkerMain consumes one bounded job. It is dispatched before service/config initialization.
func WorkerMain(ctx context.Context, input io.Reader, output io.Writer, allowUnconfined bool) error {
	var j Job
	if e := decodeLimited(input, &j, MaxJobBytes); e != nil {
		return fmt.Errorf("worker input: %w", e)
	}
	if e := j.validate(); e != nil {
		return e
	}
	isolation, cleanup, e := isolateWorker(allowUnconfined)
	if e != nil {
		return e
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, TaskTimeout)
	defer cancel()
	result := executeJob(ctx, j, newHTTPClient())
	result.Isolation = isolation
	return json.NewEncoder(output).Encode(result)
}
func executeJob(ctx context.Context, j Job, client *http.Client) (r Result) {
	r = resultFor(j)
	defer func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			r.Outcome = OutcomeCanceled
		}
		finalizeResult(&r)
	}()
	started := time.Now()
	defer func() {
		r.ElapsedMS = time.Since(started).Milliseconds()
		if p := recover(); p != nil {
			r.FailureOrigin = "probe"
			r.Reason = "worker panic: " + safe(p)
		}
	}()
	if err := j.validate(); err != nil {
		r.FailureOrigin = "sample"
		r.Reason = safe(err)
		return
	}
	if !utf8.Valid(j.Script) {
		r.FailureOrigin = "probe"
		r.Reason = "unsupported script encoding; UTF-8 required"
		return
	}
	s, e := newSession(ctx, client)
	if e != nil {
		r.FailureOrigin = "probe"
		r.Reason = safe(e)
		return
	}
	defer s.close()
	defer func() {
		r.HTTP = s.events
		seen := map[string]bool{}
		for _, event := range s.events {
			msg := str(event["error"])
			if (strings.Contains(msg, "probe_policy") || strings.Contains(msg, "probe_limit")) && !seen[msg] {
				r.ProbeIssues = append(r.ProbeIssues, safe(msg))
				seen[msg] = true
			}
		}
	}()
	fail := func(e error, origin string) {
		r.Reason = safe(e)
		r.FailureOrigin = origin
		if errors.Is(e, context.Canceled) {
			r.FailureOrigin = "probe"
			r.Outcome = OutcomeCanceled
		}
		if strings.Contains(r.Reason, "probe_limit") || strings.Contains(r.Reason, "probe_policy") || strings.Contains(r.Reason, "JS execution budget") {
			r.FailureOrigin = "probe"
		}
	}
	info := map[string]any{"name": annotation(string(j.Script), "name"), "version": annotation(string(j.Script), "version"), "author": annotation(string(j.Script), "author"), "description": annotation(string(j.Script), "description"), "scriptBase64": base64.StdEncoding.EncodeToString(j.Script)}
	r.Initialization = "failed"
	if e = s.eval("script-info", "__lx_setScriptInfo__("+jsJSON(info)+")"); e != nil {
		fail(e, "probe")
		return
	}
	e = s.eval(j.ScriptPath, "(function(){\n"+string(j.Script)+"\n})();")
	if e == nil {
		e = s.pump(func() bool { return s.ready != nil })
	}
	r.LoadMS = time.Since(started).Milliseconds()
	if e != nil {
		fail(e, "source")
		return
	}
	if s.ready["status"] == false {
		fail(fmt.Errorf("initialization rejected: %s", safe(s.ready["message"])), "source")
		return
	}
	r.Initialization = "ok"
	platform, ok := obj(s.ready["sources"])[j.Sample.source()]
	if !ok {
		r.Initialization = "unsupported"
		r.Reason = "platform not declared"
		return
	}
	qualities, _ := obj(platform)["qualitys"].([]any)
	if len(qualities) > 0 {
		supported := false
		for _, q := range qualities {
			if q == j.Quality {
				supported = true
			}
		}
		if !supported {
			r.Initialization = "unsupported"
			r.Reason = "requested quality not declared"
			return
		}
	}
	s.cpu = 1500 * time.Millisecond
	s.deadline = time.Now().Add(12 * time.Second)
	req := map[string]any{"requestKey": j.ID, "source": j.Sample.source(), "action": "musicUrl", "info": map[string]any{"musicInfo": j.Sample.MusicInfo, "type": j.Quality}}
	resolveStart := time.Now()
	r.Resolution = "failed"
	e = s.eval("resolve", "__lx_sendRequest__("+jsJSON(req)+")")
	if e == nil {
		e = s.pump(func() bool { return s.response != nil })
	}
	r.ResolveMS = time.Since(resolveStart).Milliseconds()
	if e != nil {
		if errors.Is(e, context.DeadlineExceeded) {
			r.Resolution = "timeout"
		}
		if errors.Is(e, context.Canceled) {
			r.Resolution = "canceled"
		}
		fail(e, "source")
		return
	}
	if s.response["success"] != true {
		fail(fmt.Errorf("%s", safe(s.response["error"])), "source")
		return
	}
	raw, ok := s.response["url"].(string)
	u, e := url.Parse(raw)
	if !ok || e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		r.Reason = "script returned invalid music URL"
		r.FailureOrigin = "source"
		return
	}
	r.Resolution = "ok"
	resourceCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r.Resource = checkResource(resourceCtx, client, raw, j.Sample.ExpectedDurationMS)
	if r.Resource.Status != "recognized_audio" {
		r.FailureOrigin = "resource"
		r.Reason = r.Resource.Reason
		if strings.Contains(r.Reason, "probe_") {
			r.FailureOrigin = "probe"
		}
	}
	return
}
