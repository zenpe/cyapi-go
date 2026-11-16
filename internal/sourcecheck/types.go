// Package sourcecheck runs bounded, isolated LX source diagnostics.
package sourcecheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const ProtocolVersion = 1
const ProbeVersion = "goja-probe-v5"
const MaxScriptBytes = 8 << 20
const MaxJobBytes = 12 << 20
const TaskTimeout = 40 * time.Second

var ErrBusy = errors.New("source check already running")
var ErrLimit = errors.New("probe_limit")
var ErrEnvironment = errors.New("source check environment unavailable")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type Sample struct {
	ID                 string         `json:"id"`
	Platform           string         `json:"platform"`
	TrackID            string         `json:"trackId"`
	Enabled            bool           `json:"enabled"`
	Title              string         `json:"title"`
	MusicInfo          map[string]any `json:"musicInfo"`
	ExpectedDurationMS *int64         `json:"expectedDurationMs"`
	DurationEvidence   string         `json:"durationEvidence,omitempty"`
}

func (s Sample) source() string {
	if s.Platform == "qq" {
		return "tx"
	}
	return "wy"
}
func (s Sample) validate() error {
	if !idPattern.MatchString(s.ID) || (s.Platform != "qq" && s.Platform != "netease") || s.TrackID == "" || len(s.TrackID) > 100 {
		return errors.New("invalid sample identity")
	}
	if str(s.MusicInfo["source"]) != s.source() || str(s.MusicInfo["songmid"]) != s.TrackID {
		return fmt.Errorf("sample %s has inconsistent musicInfo", s.ID)
	}
	if s.Platform == "netease" && str(s.MusicInfo["id"]) != s.TrackID {
		return errors.New("netease sample requires matching numeric id")
	}
	if s.ExpectedDurationMS != nil && (*s.ExpectedDurationMS <= 0 || *s.ExpectedDurationMS > 24*60*60*1000 || s.DurationEvidence == "") {
		return errors.New("expected duration requires positive value and evidence")
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > 64<<10 {
		return errors.New("sample exceeds size limit")
	}
	return nil
}

type SampleSet struct {
	Format  int      `json:"format"`
	Samples []Sample `json:"samples"`
}

func DecodeSamples(r io.Reader) (SampleSet, string, error) { return decodeSamples(r, true) }
func decodeSamples(r io.Reader, requireEnabled bool) (SampleSet, string, error) {
	var set SampleSet
	if err := decodeLimited(r, &set, 1<<20); err != nil {
		return set, "", err
	}
	if set.Format != 1 || (requireEnabled && len(set.Samples) == 0) || len(set.Samples) > 32 {
		return set, "", errors.New("expected 1..32 samples, format=1")
	}
	seen := map[string]bool{}
	enabled := 0
	for _, s := range set.Samples {
		if err := s.validate(); err != nil {
			return set, "", err
		}
		if seen[s.ID] {
			return set, "", errors.New("duplicate sample id")
		}
		seen[s.ID] = true
		if s.Enabled {
			enabled++
		}
	}
	if requireEnabled && enabled == 0 {
		return set, "", errors.New("no enabled samples")
	}
	b, _ := json.Marshal(set)
	return set, digest(b), nil
}

type Job struct {
	Format       int    `json:"format"`
	ID           string `json:"jobId"`
	ScriptPath   string `json:"scriptPath"`
	ScriptSHA256 string `json:"scriptSha256"`
	Script       []byte `json:"script"`
	Sample       Sample `json:"sample"`
	Quality      string `json:"quality"`
}

func (j Job) validate() error {
	if j.Format != ProtocolVersion || !idPattern.MatchString(j.ID) || j.Quality != "128k" || len(j.Script) == 0 || len(j.Script) > MaxScriptBytes || digest(j.Script) != j.ScriptSHA256 {
		return errors.New("invalid worker job or script digest")
	}
	return j.Sample.validate()
}

type ResourceResult struct {
	Extension          string   `json:"extension,omitempty"`
	HeaderHex          string   `json:"headerHex,omitempty"`
	DuplicateSampleIDs []string `json:"duplicateSampleIds,omitempty"`
	Complete           bool     `json:"complete,omitempty"`
	SHA256             string   `json:"sha256,omitempty"`
	Status             string   `json:"status"`
	Host               string   `json:"host,omitempty"`
	FinalHost          string   `json:"finalHost,omitempty"`
	HTTPStatus         int      `json:"httpStatus,omitempty"`
	ContentType        string   `json:"contentType,omitempty"`
	ContentRange       string   `json:"contentRange,omitempty"`
	ContentLength      int64    `json:"contentLength,omitempty"`
	Signature          string   `json:"signature,omitempty"`
	ReadLimit          bool     `json:"readLimit,omitempty"`
	Codec              string   `json:"codec,omitempty"`
	Bytes              int      `json:"bytes"`
	Format             string   `json:"format,omitempty"`
	DurationMS         *int64   `json:"durationMs,omitempty"`
	DurationMethod     string   `json:"durationMethod,omitempty"`
	Assessment         string   `json:"assessment"`
	Reason             string   `json:"reason,omitempty"`
	ReasonCode         string   `json:"reasonCode,omitempty"`
	ElapsedMS          int64    `json:"elapsedMs"`
}
type Result struct {
	Format         int              `json:"format"`
	JobID          string           `json:"jobId"`
	ScriptPath     string           `json:"scriptPath"`
	ScriptSHA256   string           `json:"scriptSha256"`
	SampleID       string           `json:"sampleId"`
	Platform       string           `json:"platform"`
	Quality        string           `json:"quality"`
	ProbeVersion   string           `json:"probeVersion"`
	CheckedAt      time.Time        `json:"checkedAt"`
	Initialization string           `json:"initialization"`
	Resolution     string           `json:"resolution"`
	Resource       ResourceResult   `json:"resource"`
	Outcome        string           `json:"outcome,omitempty"`
	FailureStage   string           `json:"failureStage,omitempty"`
	ProbeIssues    []string         `json:"probeIssues,omitempty"`
	FailureOrigin  string           `json:"failureOrigin,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	LoadMS         int64            `json:"loadMs"`
	ResolveMS      int64            `json:"resolveMs"`
	ElapsedMS      int64            `json:"elapsedMs"`
	HTTP           []map[string]any `json:"http,omitempty"`
	Isolation      string           `json:"isolation,omitempty"`
}

func resultFor(j Job) Result {
	return Result{Format: ProtocolVersion, JobID: j.ID, ScriptPath: j.ScriptPath, ScriptSHA256: j.ScriptSHA256, SampleID: j.Sample.ID, Platform: j.Sample.Platform, Quality: j.Quality, ProbeVersion: ProbeVersion, CheckedAt: time.Now().UTC(), Initialization: "not_run", Resolution: "not_run", Resource: ResourceResult{Status: "not_run", Assessment: "unknown"}}
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func decodeLimited(r io.Reader, v any, limit int64) error {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return e
	}
	if int64(len(b)) > limit {
		return ErrLimit
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON input")
	}
	return nil
}
