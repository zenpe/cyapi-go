package sourcerecommendation

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"cyapi-go/internal/sourcecheck"
)

const PolicyVersion = "platform-availability-v2"

type Policy struct {
	MinSamples  int           `json:"minSamples"`
	MinPositive int           `json:"minPositiveSamples"`
	MaxAge      time.Duration `json:"evidenceMaxAge"`
}

func DefaultPolicy() Policy { return Policy{3, 2, 6 * time.Hour} }
func (p Policy) Fingerprint() string {
	b, _ := json.Marshal(struct {
		MinSamples, MinPositive int
		MaxAge                  time.Duration
	}{p.MinSamples, p.MinPositive, p.MaxAge})
	return digest(append([]byte(PolicyVersion), b...))
}

type Target struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Name   string `json:"name"`
}
type Evidence struct {
	Target        Target            `json:"target"`
	Platform      string            `json:"platform"`
	RunID         string            `json:"runId"`
	SampleVersion string            `json:"sampleVersion"`
	ProbeVersion  string            `json:"probeVersion"`
	StartedAt     time.Time         `json:"startedAt"`
	CheckedAt     time.Time         `json:"checkedAt"`
	N             int               `json:"n"`
	P             int               `json:"p"`
	V             int               `json:"v"`
	F             int               `json:"f"`
	E             int               `json:"e"`
	U             int               `json:"u"`
	Unsupported   int               `json:"unsupported"`
	Songs         map[string]string `json:"songs"`
}

func Evaluate(r sourcecheck.Report, platform string, p Policy, now time.Time) ([]Evidence, error) {
	if r.State != "completed" || r.Completed != r.Planned || len(r.Results) != r.Planned || r.FinishedAt == nil || r.FinishedAt.After(now) || now.Sub(*r.FinishedAt) > p.MaxAge || r.ProbeVersion != sourcecheck.ProbeVersion {
		return nil, fmt.Errorf("incomplete, stale or incompatible evidence")
	}
	samples := map[string]sourcecheck.Sample{}
	tracks := map[string]bool{}
	for _, s := range r.Samples {
		if s.Enabled && s.Platform == platform {
			if tracks[s.TrackID] {
				return nil, fmt.Errorf("duplicate track in sample set")
			}
			tracks[s.TrackID] = true
			samples[s.ID] = s
		}
	}
	if len(samples) < p.MinSamples {
		return nil, fmt.Errorf("insufficient samples")
	}
	grouped := map[string]*Evidence{}
	for _, x := range r.Results {
		if x.Platform != platform {
			continue
		}
		s, ok := samples[x.SampleID]
		if !ok || x.Quality != "128k" || x.ProbeVersion != r.ProbeVersion || (x.Isolation != "cgroup_v2_512MiB" && x.Isolation != "windows_job_512MiB") || !hashPattern.MatchString(x.ScriptSHA256) {
			return nil, fmt.Errorf("invalid or unconfined result")
		}
		key := x.ScriptPath + "\x00" + x.ScriptSHA256
		e := grouped[key]
		if e == nil {
			e = &Evidence{Target: Target{x.ScriptPath, x.ScriptSHA256, scriptName(x.ScriptPath)}, Platform: platform, RunID: r.RunID, SampleVersion: r.SampleSetVersion, ProbeVersion: r.ProbeVersion, StartedAt: r.StartedAt, CheckedAt: *r.FinishedAt, N: len(samples), Songs: map[string]string{}}
			grouped[key] = e
		}
		if _, ok = e.Songs[s.TrackID]; ok {
			return nil, fmt.Errorf("duplicate result")
		}
		outcome := sourcecheck.ClassifyResult(x)
		kind := "F"
		switch {
		case outcome == sourcecheck.OutcomeRecognized && len(x.Resource.DuplicateSampleIDs) == 0:
			kind = "P"
			e.P++
			if s.ExpectedDurationMS != nil && x.Resource.DurationMS != nil && x.Resource.Assessment == "no_short_duration_detected" {
				e.V++
			}
		case sourcecheck.ProbeLimited(x) && outcome != sourcecheck.OutcomeRecognized:
			kind = "E"
			e.E++
		case outcome == sourcecheck.OutcomeUnverified:
			kind = "U"
			e.U++
		case outcome == sourcecheck.OutcomeUnsupported:
			kind = "unsupported"
			e.Unsupported++
		default:
			e.F++
		}
		e.Songs[s.TrackID] = kind
	}
	out := []Evidence{}
	for _, e := range grouped {
		if len(e.Songs) != e.N {
			return nil, fmt.Errorf("missing sample results")
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.P != b.P {
			return a.P > b.P
		}
		if a.V != b.V {
			return a.V > b.V
		}
		if a.E != b.E {
			return a.E < b.E
		}
		if a.Target.Path != b.Target.Path {
			return a.Target.Path < b.Target.Path
		}
		return a.Target.SHA256 < b.Target.SHA256
	})
	return out, nil
}

// Confirmation remains readable for historical decisions written before v2.
type Confirmation struct {
	Fingerprint     string            `json:"fingerprint"`
	CurrentHash     string            `json:"currentHash"`
	Candidate       Target            `json:"candidate"`
	FirstAt         time.Time         `json:"firstAt"`
	LastAt          time.Time         `json:"lastAt"`
	Rounds          []Evidence        `json:"rounds"`
	IncumbentRounds []Evidence        `json:"incumbentRounds,omitempty"`
	Gains           map[string]string `json:"gains"`
}
