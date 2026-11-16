package sourcecheck

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

func performHTTP(ctx context.Context, client *http.Client, rawURL string, opts map[string]any) (out, event map[string]any) {
	started := time.Now()
	u, err := url.Parse(rawURL)
	host := ""
	if u != nil {
		host = u.Hostname()
	}
	event = map[string]any{"host": host}
	out = map[string]any{"success": false}
	defer func() {
		event["elapsedMs"] = time.Since(started).Milliseconds()
		if err != nil {
			out["error"] = safe(err)
			out["failureKind"] = "transport"
			event["error"] = safe(err)
		}
	}()
	if err != nil {
		return
	}
	method := strings.ToUpper(str(opts["method"]))
	if method == "" {
		method = "GET"
	}
	event["method"] = method
	headers := http.Header{}
	for k, v := range obj(opts["headers"]) {
		headers.Set(k, str(v))
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", "lx-music-request")
	}
	if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}
	headers.Set("Accept-Encoding", "gzip")
	var payload []byte
	if method == "GET" {
		headers.Del("Content-Type")
	} else if v := opts["body"]; v != nil {
		switch b := v.(type) {
		case string:
			payload = []byte(b)
		case map[string]any:
			payload, _ = json.Marshal(b)
			if headers.Get("Content-Type") == "" {
				headers.Set("Content-Type", "application/json")
			}
		case []any:
			for _, x := range b {
				payload = append(payload, byte(num(x)))
			}
		}
	} else if v := opts["form"]; v != nil {
		switch f := v.(type) {
		case string:
			payload = []byte(f)
		case map[string]any:
			pairs := []string{}
			for k, v := range f {
				pairs = append(pairs, strings.ReplaceAll(url.QueryEscape(k), "+", "%20")+"="+strings.ReplaceAll(url.QueryEscape(str(v)), "+", "%20"))
			}
			payload = []byte(strings.Join(pairs, "&"))
			if headers.Get("Content-Type") == "" {
				headers.Set("Content-Type", "application/x-www-form-urlencoded")
			}
		}
	}
	if len(payload) > maxBytes {
		err = ErrLimit
		return
	}
	req, e := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(payload))
	if e != nil {
		err = e
		return
	}
	req.Header = headers
	resp, e := client.Do(req)
	if e != nil {
		err = e
		return
	}
	defer resp.Body.Close()
	event["status"] = resp.StatusCode
	event["contentType"] = safe(resp.Header.Get("Content-Type"))
	var reader io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		gz, e := gzip.NewReader(reader)
		if e != nil {
			err = e
			return
		}
		defer gz.Close()
		reader = gz
	case "", "identity":
	default:
		err = fmt.Errorf("unsupported content encoding")
		return
	}
	data, e := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if e != nil {
		err = e
		return
	}
	if len(data) > maxBytes {
		err = fmt.Errorf("%w: response exceeds 8MiB", ErrLimit)
		return
	}
	if e = accountDecoded(client, len(data)); e != nil {
		err = e
		return
	}
	event["bytes"] = len(data)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if at := strings.Index(ct, "charset="); at >= 0 {
		label := strings.Trim(strings.Split(ct[at+8:], ";")[0], " \"'")
		if label != "utf-8" && label != "utf8" {
			enc, e := htmlindex.Get(label)
			if e != nil {
				err = e
				return
			}
			data, _, e = transform.Bytes(enc.NewDecoder(), data)
			if e != nil {
				err = e
				return
			}
		}
	}
	if len(data) > maxBytes {
		err = ErrLimit
		return
	}
	if !utf8.Valid(data) {
		err = fmt.Errorf("invalid UTF-8 response")
		return
	}
	var body any = string(data)
	if opts["json"] != false {
		var parsed any
		if json.Unmarshal(data, &parsed) == nil {
			body = parsed
		}
	}
	if b, ok := body.(map[string]any); ok {
		if c := b["code"]; c != nil {
			switch c.(type) {
			case string, float64:
				event["businessCode"] = safe(c)
			}
		}
		for _, k := range []string{"msg", "message", "error"} {
			if m, ok := b[k].(string); ok && m != "" {
				event["businessMessage"] = safe(m)
				break
			}
		}
	}
	rh := map[string]any{}
	for k, v := range resp.Header {
		rh[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	out = map[string]any{"success": true, "body": body, "response": map[string]any{"statusCode": resp.StatusCode, "statusMessage": http.StatusText(resp.StatusCode), "headers": rh, "body": body, "bytes": len(data)}}
	return
}
