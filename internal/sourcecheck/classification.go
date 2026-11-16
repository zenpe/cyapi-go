package sourcecheck

const (
	OutcomeFailed      = "failed"
	OutcomeUnverified  = "unverified"
	OutcomeRecognized  = "recognized_audio"
	OutcomeShort       = "suspected_short_resource"
	OutcomeUnsupported = "unsupported"
	OutcomeCanceled    = "canceled"
)

// ClassifyResult describes the observation, independently of its cause. A failed
// attempt involving probe restrictions is still failed, not proof of a dead source.
func ClassifyResult(r Result) string {
	if r.Outcome == OutcomeCanceled || r.Resolution == "canceled" {
		return OutcomeCanceled
	}
	if r.Initialization == "unsupported" {
		return OutcomeUnsupported
	}
	if r.Resolution != "ok" {
		return OutcomeFailed
	}
	switch r.Resource.Status {
	case "recognized_audio":
		if r.Resource.Assessment == "suspected_short_resource" {
			return OutcomeShort
		}
		return OutcomeRecognized
	case "unverified":
		return OutcomeUnverified
	default:
		return OutcomeFailed
	}
}

func ProbeLimited(r Result) bool { return r.FailureOrigin == "probe" || len(r.ProbeIssues) > 0 }

func finalizeResult(r *Result) {
	r.Outcome = ClassifyResult(*r)
	if r.Outcome != OutcomeFailed && r.Outcome != OutcomeCanceled {
		return
	}
	switch {
	case r.FailureOrigin == "sample":
		r.FailureStage = "sample"
	case r.Initialization == "not_run":
		r.FailureStage = "probe"
	case r.Initialization != "ok":
		r.FailureStage = "initialization"
	case r.Resolution != "ok":
		r.FailureStage = "resolution"
	default:
		r.FailureStage = "resource"
	}
}
