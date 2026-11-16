package sourcerecommendation

import (
	"bytes"
	"context"
	"cyapi-go/internal/sourcecheck"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"time"
)

type PlatformHealth struct {
	SuspectedEncrypted int       `json:"suspectedEncrypted"`
	Duplicate          int       `json:"duplicate"`
	Recognized         int       `json:"recognized"`
	Short              int       `json:"short"`
	Failed             int       `json:"failed"`
	Unverified         int       `json:"unverified"`
	Unsupported        int       `json:"unsupported"`
	Canceled           int       `json:"canceled"`
	Limited            int       `json:"limited"`
	CheckedAt          time.Time `json:"checkedAt"`
	ExpiresAt          time.Time `json:"expiresAt"`
}
type Descriptor struct {
	ReleaseID    string                     `json:"releaseId"`
	ScriptSHA256 string                     `json:"scriptSha256"`
	ResolvedName string                     `json:"resolvedName"`
	PublishedAt  time.Time                  `json:"publishedAt"`
	Note         string                     `json:"note"`
	Health       map[string]*PlatformHealth `json:"health"`
}

func (s *Store) Describe(r Release) Descriptor {
	d := Descriptor{r.ReleaseID, r.ScriptSHA256, r.OriginalName, r.PublishedAt, r.Note, map[string]*PlatformHealth{}}
	if s.Checks == nil {
		return d
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	key := r.ScriptSHA256 + ":" + s.Checks.HealthVersion()
	if key == s.healthKey && s.health != nil {
		d.Health = s.health
		return d
	}
	defer func() { s.healthKey = key; s.health = d.Health }()
	_, version, e := s.Checks.Samples(context.Background())
	if e != nil {
		return d
	}
	reports, e := s.Checks.Reports(20)
	if e != nil {
		return d
	}
	seen := map[string]bool{}
	for _, report := range reports {
		if report.SampleSetVersion != version || report.ProbeVersion != sourcecheck.ProbeVersion {
			continue
		}
		for _, x := range report.Results {
			if x.ScriptSHA256 != r.ScriptSHA256 || seen[x.Platform+":"+x.SampleID] {
				continue
			}
			seen[x.Platform+":"+x.SampleID] = true
			h := d.Health[x.Platform]
			if h == nil {
				h = &PlatformHealth{CheckedAt: x.CheckedAt}
				d.Health[x.Platform] = h
			}
			if x.CheckedAt.Before(h.CheckedAt) {
				h.CheckedAt = x.CheckedAt
			}
			h.ExpiresAt = h.CheckedAt.Add(24 * time.Hour)
			if x.Resource.ReasonCode == "suspected_encrypted_resource" {
				h.SuspectedEncrypted++
			}
			if len(x.Resource.DuplicateSampleIDs) > 0 {
				h.Duplicate++
			}
			category := sourcecheck.ClassifyResult(x)
			if category == sourcecheck.OutcomeFailed && sourcecheck.ProbeLimited(x) {
				h.Limited++
			}
			switch category {
			case sourcecheck.OutcomeUnsupported:
				h.Unsupported++
			case sourcecheck.OutcomeCanceled:
				h.Canceled++
			case sourcecheck.OutcomeShort:
				h.Short++
			case sourcecheck.OutcomeRecognized:
				h.Recognized++
			case sourcecheck.OutcomeUnverified:
				h.Unverified++
			default:
				h.Failed++
			}
		}
	}
	return d
}
func (s *Store) CatalogItem() (map[string]string, error) {
	r, e := s.Current()
	if e != nil {
		return nil, e
	}
	return map[string]string{"path": CatalogPath, "name": "推荐音源", "directory": "推荐", "kind": "recommendation", "releaseId": r.ReleaseID, "sha256": r.ScriptSHA256, "resolvedName": r.OriginalName, "publishedAt": r.PublishedAt.Format(time.RFC3339Nano)}, nil
}
func publicError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code})
}
func (s *Store) Register(m *http.ServeMux) { m.HandleFunc("GET /lx/recommendation", s.description) }
func (s *Store) description(w http.ResponseWriter, r *http.Request) {
	release, e := s.Current()
	if errors.Is(e, ErrUnpublished) {
		publicError(w, 404, "recommendation_unpublished")
		return
	}
	if e != nil {
		publicError(w, 503, "recommendation_unavailable")
		return
	}
	b, _ := json.Marshal(map[string]any{"status": 200, "recommendation": s.Describe(release)})
	etag := `"` + digest(b) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	w.Write(b)
}
func (s *Store) ServeRecommendedScript(w http.ResponseWriter, r *http.Request) {
	query, e := parseQuery(r)
	if e != nil {
		publicError(w, 400, "invalid_recommendation_hash")
		return
	}
	hash := query
	release, b, e := s.Script(r.Context(), hash)
	if e != nil {
		switch {
		case errors.Is(e, ErrUnpublished):
			publicError(w, 404, "recommendation_unpublished")
		case errors.Is(e, os.ErrNotExist):
			publicError(w, 404, "recommendation_hash_unknown")
		case errors.Is(e, ErrGone):
			publicError(w, 410, "recommendation_hash_expired")
		default:
			publicError(w, 503, "recommendation_unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+digest(b)+`"`)
	if hash != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "recommended.js", release.PublishedAt, bytes.NewReader(b))
}
func parseQuery(r *http.Request) (string, error) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return "", e
	}
	v, exists := q["sha256"]
	if !exists {
		return "", nil
	}
	if len(v) != 1 || !hashPattern.MatchString(v[0]) {
		return "", ErrInvalid
	}
	return v[0], nil
}
