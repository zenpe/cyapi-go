package requestbody

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
)

func Read(w http.ResponseWriter, r *http.Request, max int64) (map[string]any, error) {
	if r.Method == "GET" {
		q, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil {
			return nil, e
		}
		b := map[string]any{}
		for k, v := range q {
			if len(v) != 1 {
				return nil, errors.New("duplicate parameter")
			}
			b[k] = v[0]
		}
		return b, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil {
		return nil, e
	}
	b := map[string]any{}
	if media == "application/json" {
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		if e = d.Decode(&b); e != nil || b == nil {
			return nil, errors.New("invalid body")
		}
		if d.Decode(new(any)) != io.EOF {
			return nil, errors.New("invalid body")
		}
		return b, nil
	}
	if media != "application/x-www-form-urlencoded" {
		return nil, errors.New("invalid media")
	}
	if e = r.ParseForm(); e != nil {
		return nil, e
	}
	for k, v := range r.PostForm {
		if len(v) != 1 {
			return nil, errors.New("duplicate parameter")
		}
		b[k] = v[0]
	}
	return b, nil
}
func Text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return ""
}
