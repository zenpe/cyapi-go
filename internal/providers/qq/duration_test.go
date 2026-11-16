package qq

import (
	"encoding/json"
	"testing"
)

func TestMappedDurationUsesOfficialSeconds(t *testing.T) {
	got := mapped(map[string]any{"mid": "MID", "interval": json.Number("212")})
	if got["durationMs"] != int64(212000) {
		t.Fatalf("%+v", got)
	}
	for _, bad := range []any{nil, json.Number("0"), json.Number("-1"), json.Number("86401")} {
		if _, ok := mapped(map[string]any{"interval": bad})["durationMs"]; ok {
			t.Fatal("invalid duration retained", bad)
		}
	}
}
