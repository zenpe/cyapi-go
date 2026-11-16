package music

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDurationContract(t *testing.T) {
	b, err := os.ReadFile("testdata/audio_duration_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Intervals []struct {
			DurationMS *int64
			Interval   *string
		}
		Assessments []struct {
			ExpectedMS, ObservedMS *int64
			Result                 string
		}
	}
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, row := range f.Intervals {
		var ms int64
		if row.DurationMS != nil {
			ms = *row.DurationMS
		}
		want := ""
		if row.Interval != nil {
			want = *row.Interval
		}
		if got := LXInterval(ms); got != want {
			t.Fatalf("%d: %q != %q", ms, got, want)
		}
	}
	for _, row := range f.Assessments {
		var expected, observed int64
		if row.ExpectedMS != nil {
			expected = *row.ExpectedMS
		}
		if row.ObservedMS != nil {
			observed = *row.ObservedMS
		}
		if got := AssessAudioDuration(expected, observed); got != row.Result {
			t.Fatalf("%+v: %s", row, got)
		}
	}
}
