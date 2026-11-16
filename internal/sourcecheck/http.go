package sourcecheck

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	if opts["body"] == nil && opts["form"] == nil && opts["formData"] != nil && method != "GET" && method != "HEAD" {
		payload, err = encodeMultipart(opts["formData"], headers)
		if err != nil {
			return
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
	if opts["audioResource"] == true {
		applyAudioHeaders(req)
		client = resourceHTTPClient(client)
	}
	resp, e := client.Do(req)
	if e != nil {
		err = e
		return
	}
	defer resp.Body.Close()
	event["status"] = resp.StatusCode
	event["contentType"] = safe(resp.Header.Get("Content-Type"))
	var wire bytes.Buffer
	var reader io.Reader = io.TeeReader(io.LimitReader(resp.Body, maxBytes+1), &wire)
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		if method == "HEAD" || resp.StatusCode == 204 || resp.StatusCode == 304 {
			break
		}
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
	responseLimit := maxBytes
	if limit, e := strconv.ParseFloat(fmt.Sprint(opts["maxResponseBytes"]), 64); e == nil && limit > 0 && limit < float64(maxBytes) {
		responseLimit = int(math.Ceil(limit))
	}
	data, e := io.ReadAll(io.LimitReader(reader, int64(responseLimit)+1))
	if e != nil {
		err = e
		return
	}
	if len(data) > responseLimit {
		err = fmt.Errorf("%w: response exceeds byte limit", ErrLimit)
		return
	}
	if e = accountDecoded(client, len(data)); e != nil {
		err = e
		return
	}
	event["bytes"] = len(data)
	rawData := wire.Bytes()
	if len(rawData) > maxBytes {
		err = ErrLimit
		return
	}
	var body any
	encoding, hasEncoding := opts["encoding"]
	if hasEncoding && encoding == nil {
		binary := make([]int, len(data))
		for i, b := range data {
			binary[i] = int(b)
		}
		body = binary
	} else {
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
		data, _, e = transform.Bytes(unicode.UTF8.NewDecoder(), data)
		if e != nil {
			err = e
			return
		}
		body = string(data)
		if opts["json"] != false {
			var parsed any
			if json.Unmarshal(data, &parsed) == nil {
				body = parsed
			}
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
	out = map[string]any{"success": true, "body": body, "response": map[string]any{"statusCode": resp.StatusCode, "statusMessage": http.StatusText(resp.StatusCode), "headers": rh, "body": body, "rawBase64": base64.StdEncoding.EncodeToString(rawData), "bytes": len(rawData)}}
	return
}

// Multipart accepts values and in-memory bytes only, never local file paths.
func encodeMultipart(value any, headers http.Header) ([]byte, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid formData")
	}
	out := &limitedMultipartBuffer{}
	writer := multipart.NewWriter(out)
	for name, value := range fields {
		if len(name) > 8192 {
			return nil, ErrLimit
		}
		var content []byte
		filename, contentType := "", "application/octet-stream"
		file := false
		switch v := value.(type) {
		case map[string]any:
			raw, exists := v["buffer"]
			if !exists {
				return nil, fmt.Errorf("unsupported formData field")
			}
			var e error
			content, e = multipartBytes(raw)
			if e != nil {
				return nil, e
			}
			filename = str(v["filename"])
			if filename == "" {
				filename = "blob"
			}
			if len(filename) > 1024 {
				return nil, ErrLimit
			}
			if text := str(v["content_type"]); text != "" {
				contentType = text
			}
			if len(contentType) > 256 {
				return nil, ErrLimit
			}
			if _, _, e := mime.ParseMediaType(contentType); e != nil {
				return nil, fmt.Errorf("invalid multipart content type")
			}
			file = true
		case []any:
			var e error
			content, e = multipartBytes(v)
			if e != nil {
				return nil, e
			}
			filename = "blob"
			file = true
		case string:
			content = []byte(v)
		case nil:
			content = []byte{}
		case bool, float64, int, int64, json.Number:
			content = []byte(fmt.Sprint(v))
		default:
			return nil, fmt.Errorf("unsupported formData field")
		}
		if len(content) > maxBytes {
			return nil, ErrLimit
		}
		if file {
			escape := func(s string) string {
				return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\r", "%0D", "\n", "%0A").Replace(s)
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="`+escape(name)+`"; filename="`+escape(filename)+`"`)
			h.Set("Content-Type", contentType)
			part, e := writer.CreatePart(h)
			if e != nil {
				return nil, e
			}
			if _, e = part.Write(content); e != nil {
				return nil, e
			}
		} else {
			if e := writer.WriteField(name, string(content)); e != nil {
				return nil, e
			}
		}
	}
	if e := writer.Close(); e != nil {
		return nil, e
	}
	headers.Set("Content-Type", writer.FormDataContentType())
	return out.Bytes(), nil
}

type limitedMultipartBuffer struct{ bytes.Buffer }

func (b *limitedMultipartBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > maxBytes {
		return 0, ErrLimit
	}
	return b.Buffer.Write(data)
}
func multipartBytes(value any) ([]byte, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid multipart bytes")
	}
	if len(list) > maxBytes {
		return nil, ErrLimit
	}
	out := make([]byte, len(list))
	for i, v := range list {
		n := num(v)
		if n < 0 || n > 255 || math.IsNaN(n) || math.Trunc(n) != n {
			return nil, fmt.Errorf("invalid multipart byte")
		}
		out[i] = byte(n)
	}
	return out, nil
}
