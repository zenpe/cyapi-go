package music

import "fmt"

// Song metadata and resource observations use milliseconds, never mixed units.
func ValidDurationMS(ms int64) bool { return ms > 0 && ms <= 86400000 }

func LXInterval(ms int64) string {
	if !ValidDurationMS(ms) || ms < 1000 {
		return ""
	}
	seconds := ms / 1000
	return fmt.Sprintf("%02d:%02d", seconds/60, seconds%60)
}

// AssessAudioDuration does not establish identity or successful decoding.
func AssessAudioDuration(expected, observed int64) string {
	if !ValidDurationMS(expected) || !ValidDurationMS(observed) {
		return "unknown"
	}
	if observed*100 < expected*80 && expected-observed > 15000 {
		return "suspected_short_resource"
	}
	return "no_short_duration_detected"
}
