package sourcecheck

import "testing"

func TestClassificationSeparatesOutcomeFromCause(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		want   string
	}{
		{"permission after blocked fallback", Result{Initialization: "ok", Resolution: "failed", FailureOrigin: "source", ProbeIssues: []string{"probe_policy: blocked"}}, OutcomeFailed},
		{"probe failure", Result{Initialization: "not_run", Resolution: "not_run", FailureOrigin: "probe"}, OutcomeFailed},
		{"failed legacy result marked probe", Result{Initialization: "ok", Resolution: "failed", FailureOrigin: "probe", Resource: ResourceResult{Status: "unverified"}}, OutcomeFailed},
		{"resolved unknown", Result{Initialization: "ok", Resolution: "ok", Resource: ResourceResult{Status: "unverified"}}, OutcomeUnverified},
		{"404", Result{Initialization: "ok", Resolution: "ok", Resource: ResourceResult{Status: "unreachable", HTTPStatus: 404}}, OutcomeFailed},
		{"recognized with blocked fallback", Result{Resolution: "ok", ProbeIssues: []string{"probe_policy: blocked"}, Resource: ResourceResult{Status: "recognized_audio"}}, OutcomeRecognized},
		{"short", Result{Resolution: "ok", Resource: ResourceResult{Status: "recognized_audio", Assessment: "suspected_short_resource"}}, OutcomeShort},
		{"unsupported", Result{Initialization: "unsupported"}, OutcomeUnsupported},
		{"canceled", Result{Resolution: "canceled"}, OutcomeCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyResult(tt.result); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
