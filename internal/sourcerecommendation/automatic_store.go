package sourcerecommendation

import (
	"bytes"
	"context"
	"crypto/rand"
	"cyapi-go/internal/sources"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var planPattern = regexp.MustCompile(`^p-[a-f0-9]{32}$`)
var decisionPattern = regexp.MustCompile(`^d-[a-f0-9]{32}$`)

func scriptName(path string) string {
	return strings.TrimSuffix(filepath.Base(filepath.FromSlash(path)), ".js")
}
func newID(prefix string) (string, error) {
	var n [16]byte
	_, e := rand.Read(n[:])
	return prefix + hex.EncodeToString(n[:]), e
}

type Plan struct {
	Format  int               `json:"format"`
	PlanID  string            `json:"planId"`
	Quality string            `json:"quality"`
	Targets map[string]Target `json:"targets"`
}
type AutomaticState struct {
	Format          int    `json:"format"`
	CurrentPlanID   string `json:"currentPlanId"`
	PreviousPlanID  string `json:"previousPlanId"`
	Paused          bool   `json:"paused"`
	ControlRevision uint64 `json:"controlRevision"`
	DecisionID      string `json:"decisionId"`
}
type Decision struct {
	ID             string        `json:"id"`
	PreviousID     string        `json:"previousId"`
	PlanID         string        `json:"planId"`
	PreviousPlanID string        `json:"previousPlanId"`
	At             time.Time     `json:"at"`
	Actor          string        `json:"actor"`
	Reason         string        `json:"reason"`
	Policy         string        `json:"policy"`
	Confirmation   *Confirmation `json:"confirmation,omitempty"`
}
type ControlInput struct {
	ExpectedPlanID   string `json:"expectedPlanId"`
	ExpectedRevision uint64 `json:"expectedControlRevision"`
	Paused           bool   `json:"paused"`
	FromPlanID       string `json:"fromPlanId"`
}

func (s *Store) autoPath(parts ...string) string {
	return filepath.Join(append([]string{s.Directory, "automatic"}, parts...)...)
}
func automaticJSON(path string, v any) error {
	b, e := read(path, 8<<20)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return ErrUnavailable
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrUnavailable
	}
	return nil
}
func saveJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return atomic(path, b)
}
func (s *Store) automaticState() (AutomaticState, error) {
	st := AutomaticState{Format: 1}
	e := automaticJSON(s.autoPath("state.json"), &st)
	if errors.Is(e, os.ErrNotExist) {
		return AutomaticState{Format: 1}, nil
	}
	if e != nil || st.Format != 1 || (st.CurrentPlanID != "" && !planPattern.MatchString(st.CurrentPlanID)) || (st.PreviousPlanID != "" && !planPattern.MatchString(st.PreviousPlanID)) || (st.DecisionID != "" && !decisionPattern.MatchString(st.DecisionID)) {
		return st, ErrUnavailable
	}
	return st, nil
}
func validPlan(p Plan) bool {
	if p.Format != 1 || !planPattern.MatchString(p.PlanID) || p.Quality != "128k" || len(p.Targets) < 1 || len(p.Targets) > 2 {
		return false
	}
	for platform, t := range p.Targets {
		if (platform != "qq" && platform != "netease") || !hashPattern.MatchString(t.SHA256) || t.Name == "" || len(t.Name) > 1024 || len(t.Path) > 1024 || !strings.HasSuffix(t.Path, ".js") {
			return false
		}
	}
	return true
}
func (s *Store) plan(id string) (Plan, error) {
	var p Plan
	if !planPattern.MatchString(id) {
		return p, ErrUnpublished
	}
	e := readJSON(s.autoPath("plans", id+".json"), &p)
	if e != nil || !validPlan(p) || p.PlanID != id {
		return p, ErrUnavailable
	}
	return p, nil
}
func (s *Store) AutomaticCurrent(ctx context.Context) (AutomaticState, Plan, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return AutomaticState{}, Plan{}, e
	}
	defer unlock()
	st, e := s.automaticState()
	if e != nil {
		return st, Plan{}, e
	}
	p, e := s.plan(st.CurrentPlanID)
	return st, p, e
}
func (s *Store) automaticHistory(st AutomaticState) ([]Decision, error) {
	out := []Decision{}
	id := st.DecisionID
	seen := map[string]bool{}
	for id != "" && len(out) < 100 {
		if !decisionPattern.MatchString(id) || seen[id] {
			return nil, ErrUnavailable
		}
		seen[id] = true
		var d Decision
		e := automaticJSON(s.autoPath("decisions", id+".json"), &d)
		if errors.Is(e, os.ErrNotExist) && len(out) >= 2 {
			break
		}
		if e != nil || d.ID != id {
			return nil, ErrUnavailable
		}
		if len(out) >= 2 && time.Since(d.At) > 30*24*time.Hour {
			break
		}
		out = append(out, d)
		id = d.PreviousID
	}
	return out, nil
}
func (s *Store) AutomaticHistory(ctx context.Context) ([]Decision, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return nil, e
	}
	defer unlock()
	st, e := s.automaticState()
	if e != nil {
		return nil, e
	}
	return s.automaticHistory(st)
}
func (s *Store) AutomaticControl(ctx context.Context, in ControlInput) (AutomaticState, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return AutomaticState{}, e
	}
	defer unlock()
	st, e := s.automaticState()
	if e != nil {
		return st, e
	}
	if st.CurrentPlanID != in.ExpectedPlanID || st.ControlRevision != in.ExpectedRevision {
		return st, ErrConflict
	}
	st.Paused = in.Paused
	st.ControlRevision++
	e = saveJSON(s.autoPath("state.json"), st)
	return st, e
}
func (s *Store) commitPlan(st AutomaticState, p Plan, d Decision) (AutomaticState, error) {
	var e error
	p.Format = 1
	p.Quality = "128k"
	p.PlanID, e = newID("p-")
	if e != nil {
		return st, e
	}
	if !validPlan(p) {
		return st, ErrInvalid
	}
	d.ID, e = newID("d-")
	if e != nil {
		return st, e
	}
	d.PlanID = p.PlanID
	d.PreviousID = st.DecisionID
	d.PreviousPlanID = st.CurrentPlanID
	if e = saveJSON(s.autoPath("plans", p.PlanID+".json"), p); e != nil {
		return st, e
	}
	if e = saveJSON(s.autoPath("decisions", d.ID+".json"), d); e != nil {
		return st, e
	}
	st.PreviousPlanID = st.CurrentPlanID
	st.CurrentPlanID = p.PlanID
	st.DecisionID = d.ID
	e = saveJSON(s.autoPath("state.json"), st)
	return st, e
}
func (s *Store) AutomaticPublish(ctx context.Context, expected AutomaticState, platform string, c Confirmation, p Policy, now time.Time, enabled bool) (AutomaticState, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return AutomaticState{}, e
	}
	defer unlock()
	st, e := s.automaticState()
	if e != nil {
		return st, e
	}
	if !enabled || st.Paused || st.ControlRevision != expected.ControlRevision {
		return st, ErrConflict
	}
	plan := Plan{Targets: map[string]Target{}}
	if st.CurrentPlanID != "" {
		plan, e = s.plan(st.CurrentPlanID)
		if e != nil {
			return st, e
		}
	}
	if plan.Targets[platform].SHA256 != c.CurrentHash || len(c.Rounds) < p.Rounds || c.Fingerprint != p.Fingerprint()+c.Rounds[0].SampleVersion+c.Rounds[0].ProbeVersion+platform || now.Before(c.LastAt) || now.Sub(c.FirstAt) > p.MaxAge {
		return st, ErrConflict
	}
	if s.Checks == nil || s.Sources == nil {
		return st, ErrUnavailable
	}
	// Lock order: recommendation publication -> catalog -> samples. These locks
	// cover only verified file reads and atomic state writes, never diagnostics.
	e = s.Sources.WithVerifiedScript(ctx, c.Candidate.Path, c.Candidate.SHA256, MaxScriptBytes, func(script sources.VerifiedScript) error {
		return s.Checks.WithSampleVersion(ctx, c.Rounds[0].SampleVersion, func() error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if e := atomic(filepath.Join(s.ArchiveDirectory, c.Candidate.SHA256+".js"), script.Content); e != nil {
				return e
			}
			plan.Targets[platform] = c.Candidate
			var err error
			st, err = s.commitPlan(st, plan, Decision{At: now, Actor: "system", Reason: "sustained_evidence_improvement", Policy: p.Fingerprint(), Confirmation: &c})
			return err
		})
	})
	if e == nil {
		_ = s.pruneAutomatic()
	}
	return st, e
}
func (s *Store) AutomaticRollback(ctx context.Context, in ControlInput, actor int64) (AutomaticState, error) {
	unlock, e := s.lock(ctx)
	if e != nil {
		return AutomaticState{}, e
	}
	defer unlock()
	st, e := s.automaticState()
	if e != nil {
		return st, e
	}
	if st.CurrentPlanID != in.ExpectedPlanID || st.ControlRevision != in.ExpectedRevision {
		return st, ErrConflict
	}
	if actor <= 0 {
		return st, ErrInvalid
	}
	history, e := s.automaticHistory(st)
	if e != nil {
		return st, e
	}
	found := false
	for _, d := range history {
		if d.PlanID == in.FromPlanID {
			found = true
		}
	}
	if !found {
		return st, ErrGone
	}
	plan, e := s.plan(in.FromPlanID)
	if e != nil {
		return st, e
	}
	for _, t := range plan.Targets {
		if _, e = s.archive(t.SHA256); e != nil {
			return st, e
		}
	}
	st.Paused = true
	st.ControlRevision++
	return s.commitPlan(st, plan, Decision{At: time.Now().UTC(), Actor: fmtActor(actor), Reason: "rollback_and_pause"})
}
func fmtActor(id int64) string { b, _ := json.Marshal(id); return "admin:" + string(b) }

// Called under the SAME publication lock by legacy prune, automatic prune and backup.
func (s *Store) automaticReferences() (map[string]bool, []string, error) {
	hashes := map[string]bool{}
	files := []string{}
	st, e := s.automaticState()
	if e != nil {
		return nil, nil, e
	}
	if st.CurrentPlanID == "" {
		if _, err := os.Stat(s.autoPath("state.json")); err == nil {
			files = append(files, "state.json")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		return hashes, files, nil
	}
	files = append(files, "state.json")
	history, e := s.automaticHistory(st)
	if e != nil {
		return nil, nil, e
	}
	plans := map[string]bool{st.CurrentPlanID: true}
	if st.PreviousPlanID != "" {
		plans[st.PreviousPlanID] = true
	}
	for _, d := range history {
		files = append(files, "decisions/"+d.ID+".json")
		plans[d.PlanID] = true
	}
	for id := range plans {
		p, e := s.plan(id)
		if e != nil {
			return nil, nil, e
		}
		files = append(files, "plans/"+id+".json")
		for _, t := range p.Targets {
			hashes[t.SHA256] = true
		}
	}
	return hashes, files, nil
}
func (s *Store) pruneAutomatic() error {
	_, files, e := s.automaticReferences()
	if e != nil {
		return e
	}
	keep := map[string]bool{}
	for _, f := range files {
		keep[f] = true
	}
	for _, dir := range []string{"plans", "decisions"} {
		entries, e := os.ReadDir(s.autoPath(dir))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		for _, x := range entries {
			rel := dir + "/" + x.Name()
			info, e := x.Info()
			if e != nil {
				return e
			}
			if !x.IsDir() && !keep[rel] && time.Since(info.ModTime()) > 24*time.Hour {
				if e = os.Remove(s.autoPath(dir, x.Name())); e != nil {
					return e
				}
			}
		}
	}
	return s.pruneArchiveUnion()
}

func (s *Store) pruneArchiveUnion() error {
	refs, _, e := s.automaticReferences()
	if e != nil {
		return e
	}
	legacy, e := s.history()
	if e != nil {
		return e
	}
	for _, r := range legacy {
		refs[r.ScriptSHA256] = true
	}
	entries, e := os.ReadDir(s.ArchiveDirectory)
	if e != nil {
		return e
	}
	for _, x := range entries {
		hash := strings.TrimSuffix(x.Name(), ".js")
		if !strings.HasSuffix(x.Name(), ".js") || !hashPattern.MatchString(hash) || refs[hash] {
			continue
		}
		info, e := x.Info()
		if e != nil {
			return e
		}
		if time.Since(info.ModTime()) < 24*time.Hour {
			continue
		}
		if e = atomic(filepath.Join(s.ArchiveDirectory, hash+".gone"), []byte("expired")); e != nil {
			return e
		}
		if e = os.Remove(filepath.Join(s.ArchiveDirectory, x.Name())); e != nil {
			return e
		}
	}
	return nil
}
