package sourcerecommendation

import (
	"context"
	"time"

	"cyapi-go/internal/sourcecheck"
)

// Deliberately short initial window; always bounded by policy and watch cadence.
func (a *Automatic) reuseWindow() time.Duration {
	return min(15*time.Minute, a.Options.Policy.MaxAge, a.Options.WatchInterval)
}

// reuseTime requires a complete group from ONE run. Per-observation times are
// essential: a long manual scan finishing now must not renew its early samples.
func (a *Automatic) reuseTime(report sourcecheck.Report, target Target, platform, sampleVersion string, now time.Time) (time.Time, bool) {
	if report.SampleSetVersion != sampleVersion {
		return time.Time{}, false
	}
	observations, err := Evaluate(report, platform, a.Options.Policy, now)
	if err != nil {
		return time.Time{}, false
	}
	found := false
	for _, x := range observations {
		if x.Target == target {
			found = true
		}
	}
	if !found {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, x := range report.Results {
		if x.Platform != platform || x.ScriptPath != target.Path || x.ScriptSHA256 != target.SHA256 {
			continue
		}
		if x.CheckedAt.IsZero() || x.CheckedAt.Before(report.StartedAt) || x.CheckedAt.After(*report.FinishedAt) || x.CheckedAt.After(now) {
			return time.Time{}, false
		}
		if earliest.IsZero() || x.CheckedAt.Before(earliest) {
			earliest = x.CheckedAt
		}
	}
	return earliest, !earliest.IsZero() && now.Sub(earliest) < a.reuseWindow()
}

func (a *Automatic) reusableReport(ctx context.Context, target Target, platform, version, exclude string, now time.Time) string {
	id := ""
	_ = a.Store.Checks.VisitRecentReports(ctx, func(report sourcecheck.Report) bool {
		if report.SampleSetVersion != version || report.ProbeVersion != sourcecheck.ProbeVersion {
			return true
		}
		for _, x := range report.Results {
			if x.Platform == platform && x.ScriptPath == target.Path && x.ScriptSHA256 == target.SHA256 {
				if _, ok := a.reuseTime(report, target, platform, version, now); ok && report.RunID != exclude {
					id = report.RunID
				}
				// Never skip the newest matching failure or incomplete evidence to
				// resurrect an older success. A miss simply starts a fresh worker.
				return false
			}
		}
		return true
	})
	return id
}
