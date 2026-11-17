package sourcerecommendation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cyapi-go/internal/sourcecheck"
)

func clientCheckFixture(t *testing.T) (*Automatic, *time.Time) {
	t.Helper()
	s, _, _ := fixture(t)
	now := time.Now().UTC()
	s.Checks = &sourcecheck.Service{Directory: t.TempDir(), Sources: s.Sources, ArchiveDirectory: s.ArchiveDirectory, Runner: automaticRunner{}, Now: func() time.Time { return now }}
	c := sourcecheck.NewController(context.Background(), s.Checks)
	t.Cleanup(func() { c.Close(context.Background()) })
	a := &Automatic{Store: s, Controller: c, Now: func() time.Time { return now }, Options: AutomaticOptions{Enabled: true, FullInterval: 72 * time.Hour, WatchInterval: 3 * time.Hour, Policy: DefaultPolicy()}}
	return a, &now
}

func TestClientCheckConcurrentDurableAdmission(t *testing.T) {
	a, now := clientCheckFixture(t)
	ctx := context.Background()
	r, _ := a.runtime()
	r.RetryAt = now.Add(time.Hour)
	r.ResumeAt = now.Add(2 * time.Hour)
	r.Platforms["qq"].Confirmation = &Confirmation{LastAt: *now}
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.RequestClientCheck(ctx)
			if err != nil {
				results <- "error"
			} else {
				results <- result.Code
			}
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for code := range results {
		if code == "accepted" {
			accepted++
		} else if code != "already_requested" {
			t.Fatal(code)
		}
	}
	if accepted != 1 {
		t.Fatal("admission count", accepted)
	}
	// A new Automatic instance reads the same durable intent and cooldown.
	restarted := &Automatic{Store: a.Store, Controller: a.Controller, Now: a.Now, Options: a.Options}
	result, err := restarted.RequestClientCheck(ctx)
	if err != nil || result.Code != "already_requested" {
		t.Fatal(result, err)
	}
	if err = a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	if !r.ClientCheckPending || !r.RetryAt.Equal(now.Add(time.Hour)) || !r.ResumeAt.Equal(now.Add(2*time.Hour)) || r.Platforms["qq"].Confirmation == nil || a.Controller.ActiveID() != "" {
		t.Fatal("request bypassed scheduling guards", r)
	}
	r.ClientCheckPending = false
	if err = a.save(r); err != nil {
		t.Fatal(err)
	}
	result, err = restarted.RequestClientCheck(ctx)
	if err != nil || result.Code != "cooldown" || result.RetryAfterSeconds != 1800 {
		t.Fatal(result, err)
	}
	*now = now.Add(-time.Minute)
	result, _ = restarted.RequestClientCheck(ctx)
	if result.Code != "cooldown" {
		t.Fatal("clock rollback bypassed cooldown")
	}
}

func TestClientCheckPreservesQueueAndAdministrativeControl(t *testing.T) {
	a, now := clientCheckFixture(t)
	ctx := context.Background()
	r, _ := a.runtime()
	r.Queue = []scanItem{{Platform: "qq", Target: Target{Path: "test.js"}}}
	r.Done["kept"] = true
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	result, err := a.RequestClientCheck(ctx)
	if err != nil || result.Code != "already_requested" {
		t.Fatal(result, err)
	}
	r, _ = a.runtime()
	if len(r.Queue) != 1 || !r.Done["kept"] || !r.LastClientCheck.IsZero() {
		t.Fatal("queue changed")
	}
	st, err := a.Store.AutomaticControl(ctx, ControlInput{Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	result, _ = a.RequestClientCheck(ctx)
	if result.Code != "paused" {
		t.Fatal(result)
	}
	_, err = a.Store.AutomaticControl(ctx, ControlInput{ExpectedRevision: st.ControlRevision})
	if err != nil {
		t.Fatal(err)
	}
	r.Queue = nil
	r.FullDue = now.Add(72 * time.Hour)
	if err = a.save(r); err != nil {
		t.Fatal(err)
	}
	result, err = a.RequestClientCheck(ctx)
	if err != nil || result.Code != "accepted" {
		t.Fatal(result, err)
	}
	if err = a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = a.runtime()
	if r.ClientCheckPending || r.ActiveRunID == "" {
		t.Fatal("no initial-plan scan after admin revision", r)
	}
	result, _ = a.RequestClientCheck(ctx)
	if result.Code != "already_running" {
		t.Fatal(result)
	}
	a.Options.Enabled = false
	result, _ = a.RequestClientCheck(ctx)
	if result.Code != "disabled" {
		t.Fatal(result)
	}
}

func TestClientCheckHTTPProtectionAndCooldown(t *testing.T) {
	a, _ := clientCheckFixture(t)
	mux := http.NewServeMux()
	a.RegisterClient(mux, func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer user" {
				w.WriteHeader(401)
				return
			}
			next(w, r)
		}
	})
	request := func(token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/lx/recommendation-check", nil)
		r.Header.Set("Authorization", token)
		mux.ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("Bearer user"); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("Bearer user"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	r, _ := a.runtime()
	r.ClientCheckPending = false
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	if w := request("Bearer user"); w.Code != 429 || w.Header().Get("Retry-After") != "1800" {
		t.Fatal(w.Code, w.Header())
	}
}

type clientCheckWaitingRunner struct{}

func (clientCheckWaitingRunner) Run(ctx context.Context, job sourcecheck.Job) sourcecheck.Result {
	<-ctx.Done()
	return sourcecheck.Result{JobID: job.ID}
}

func TestClientCheckDoesNotPreemptManualWork(t *testing.T) {
	a, _ := clientCheckFixture(t)
	a.Store.Checks.Runner = clientCheckWaitingRunner{}
	id, _, err := a.Controller.Start(context.Background(), sourcecheck.RunOptions{Path: "test.js", Platform: "qq"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.RequestClientCheck(context.Background())
	if err != nil || result.Code != "busy" || a.Controller.ActiveID() != id {
		t.Fatal("manual task changed", result, err)
	}
	r, _ := a.runtime()
	if r.ClientCheckPending || !r.LastClientCheck.IsZero() {
		t.Fatal("busy request queued work")
	}
}
