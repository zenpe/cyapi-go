package sourcerecommendation

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

const clientCheckInterval = 30 * time.Minute

type ClientCheckResult struct {
	Code              string `json:"code"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

// RequestClientCheck only admits a scheduling intent. It grants none of the
// administrator's override privileges and never starts a worker in the request.
func (a *Automatic) RequestClientCheck(ctx context.Context) (ClientCheckResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ClientCheckResult{}, err
	}
	if !a.Options.Enabled || a.Controller == nil {
		return ClientCheckResult{Code: "disabled"}, nil
	}
	// Keep admission atomic with administrative pause/control writes too.
	unlock, err := a.Store.lock(ctx)
	if err != nil {
		return ClientCheckResult{}, err
	}
	defer unlock()
	st, err := a.Store.automaticState()
	if err != nil {
		return ClientCheckResult{}, err
	}
	if st.Paused {
		return ClientCheckResult{Code: "paused"}, nil
	}
	r, err := a.runtime()
	if err != nil {
		return ClientCheckResult{}, err
	}
	if active := a.Controller.ActiveID(); active != "" && active != r.ActiveRunID {
		return ClientCheckResult{Code: "busy"}, nil
	}
	if r.ActiveRunID != "" {
		return ClientCheckResult{Code: "already_running"}, nil
	}
	if r.ClientCheckPending || r.RequestedID != "" || len(r.Queue) > 0 {
		return ClientCheckResult{Code: "already_requested"}, nil
	}
	now := a.now()
	if !r.LastClientCheck.IsZero() && now.Before(r.LastClientCheck.Add(clientCheckInterval)) {
		seconds := int(math.Ceil(r.LastClientCheck.Add(clientCheckInterval).Sub(now).Seconds()))
		return ClientCheckResult{Code: "cooldown", RetryAfterSeconds: seconds}, nil
	}
	r.ClientCheckPending = true
	r.LastClientCheck = now
	// Synchronize the revision for a request made after an earlier admin change;
	// otherwise Tick would discard the newly admitted intent as stale.
	if r.ControlRevision != st.ControlRevision {
		r.ControlRevision = st.ControlRevision
		for _, status := range r.Platforms {
			status.Confirmation = nil
		}
	}
	if err = saveJSON(a.Store.autoPath("runtime.json"), r); err != nil {
		return ClientCheckResult{}, err
	}
	return ClientCheckResult{Code: "accepted"}, nil
}

func (a *Automatic) RegisterClient(m *http.ServeMux, protect func(http.HandlerFunc) http.HandlerFunc) {
	m.HandleFunc("POST /lx/recommendation-check", protect(func(w http.ResponseWriter, r *http.Request) {
		result, err := a.RequestClientCheck(r.Context())
		if err != nil {
			automaticError(w, 503, "recommendation_unavailable")
			return
		}
		status := http.StatusConflict
		switch result.Code {
		case "accepted":
			status = http.StatusAccepted
		case "already_running", "already_requested":
			status = http.StatusOK
		case "cooldown":
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(result.RetryAfterSeconds))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(result)
	}))
}
