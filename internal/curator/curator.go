// Package curator keeps the skill collection honest while nobody is watching.
//
// It does three things, none of which needs a model:
//
//   - Re-verifies active skills against their own scenarios. A skill that
//     passed once is not proven forever: tools change, plugins change, nemuz
//     changes. Because scenarios replay recordings, re-verification is free,
//     which is the only reason it can be routine rather than an event.
//   - Retires skills nobody uses.
//   - Clears out quarantine, so it does not become the drawer where the agent's
//     bad ideas accumulate.
//
// Hermes Agent's curator asks an auxiliary model to review skills and decide.
// This one decides from evidence it already has. A model would be needed for
// consolidating two overlapping skills into one, which is a real gap and a
// deliberate one: the judgements below are the ones that can be made without
// guessing.
//
// # Invariants
//
// These hold whatever the policy says:
//
//   - Only skills the agent wrote for itself are touched. A person's skill is a
//     deliberate act, and automatic maintenance must not undo one.
//   - Pinned skills are never transitioned.
//   - Nothing is deleted. Archiving is the strongest action, and it is
//     reversible.
package curator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kansaok/nemuz/internal/skill"
)

// Policy defaults. They are deliberately unhurried: a curator that retires
// things quickly is one people switch off.
const (
	DefaultRetireUnusedAfter = 90 * 24 * time.Hour
	DefaultQuarantineExpiry  = 30 * 24 * time.Hour
	DefaultMinInterval       = 24 * time.Hour
)

// Policy configures what the curator does.
type Policy struct {
	// RetireUnusedAfter archives an active skill that has gone this long
	// unused. Zero uses the default; negative disables retirement.
	RetireUnusedAfter time.Duration
	// QuarantineExpiry archives a quarantined skill that has sat this long
	// without being certified. Zero uses the default; negative disables it.
	QuarantineExpiry time.Duration
	// SkipReverify leaves active skills alone instead of re-running their
	// scenarios. Re-verification is the curator's most useful job, so this
	// exists for the case where the eval environment is not available.
	SkipReverify bool
	// MinInterval is the shortest gap between runs. Zero uses the default.
	MinInterval time.Duration
}

func (p Policy) retireAfter() time.Duration {
	if p.RetireUnusedAfter == 0 {
		return DefaultRetireUnusedAfter
	}
	return p.RetireUnusedAfter
}

func (p Policy) quarantineExpiry() time.Duration {
	if p.QuarantineExpiry == 0 {
		return DefaultQuarantineExpiry
	}
	return p.QuarantineExpiry
}

func (p Policy) minInterval() time.Duration {
	if p.MinInterval == 0 {
		return DefaultMinInterval
	}
	return p.MinInterval
}

// What a curator did to one skill.
const (
	ActionKept     = "kept"
	ActionDemoted  = "demoted"
	ActionArchived = "archived"
	ActionFailed   = "failed"
)

// Action is one decision, with the reason it was made.
type Action struct {
	Skill  string `json:"skill"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// Report is what a run did.
type Report struct {
	RanAt   time.Time `json:"ran_at"`
	Actions []Action  `json:"actions"`
	// Skipped explains why a run did nothing, when it did nothing.
	Skipped string `json:"skipped,omitempty"`
	// DryRun means the actions were decided but not applied.
	DryRun bool `json:"dry_run,omitempty"`
}

// Changed reports whether anything would change.
func (r Report) Changed() int {
	var n int
	for _, a := range r.Actions {
		if a.Action != ActionKept {
			n++
		}
	}
	return n
}

// Summary explains a run in one line.
func (r Report) Summary() string {
	if r.Skipped != "" {
		return "skipped — " + r.Skipped
	}
	if len(r.Actions) == 0 {
		return "no agent-written skills to curate"
	}
	changed := r.Changed()
	if changed == 0 {
		return fmt.Sprintf("%d skill(s) checked, all still good", len(r.Actions))
	}
	verb := "changed"
	if r.DryRun {
		verb = "would change"
	}
	return fmt.Sprintf("%d of %d skill(s) %s", changed, len(r.Actions), verb)
}

// Curator maintains a skill store.
type Curator struct {
	// Skills is the store to maintain.
	Skills *skill.Store
	// Gate re-verifies active skills. Nil skips re-verification.
	Gate *skill.Gate
	// State persists when the curator last ran. Nil runs every time.
	State *State
	// Policy configures the thresholds.
	Policy Policy
	// Now is the time source. Nil uses time.Now.
	Now func() time.Time
	// DryRun decides without changing anything.
	DryRun bool
}

// Due reports whether enough time has passed since the last run.
func (c *Curator) Due() (bool, error) {
	if c.State == nil {
		return true, nil
	}
	last, paused, err := c.State.Read()
	if err != nil {
		return false, err
	}
	if paused {
		return false, nil
	}
	if last.IsZero() {
		return true, nil
	}
	return c.now().Sub(last) >= c.Policy.minInterval(), nil
}

// Run curates the collection.
//
// It is meant to be called when the agent is idle — after a turn, before
// exiting — rather than from a daemon. There is nothing to schedule and nothing
// to keep running.
func (c *Curator) Run(ctx context.Context) (Report, error) {
	if c.Skills == nil {
		return Report{}, errors.New("curator: no skill store configured")
	}
	report := Report{RanAt: c.now().UTC(), DryRun: c.DryRun}

	due, err := c.Due()
	if err != nil {
		return report, err
	}
	if !due {
		report.Skipped = fmt.Sprintf("last run was less than %s ago", c.Policy.minInterval())
		return report, nil
	}

	all, err := c.Skills.List()
	if err != nil {
		return report, err
	}
	for _, sk := range all {
		// Anything a person wrote, or pinned, is off limits. This is checked
		// here as well as in the store so the reason appears in the report.
		if !sk.Curatable() {
			continue
		}
		action := c.decide(ctx, sk)
		report.Actions = append(report.Actions, action)
		if c.DryRun || action.Action == ActionKept || action.Action == ActionFailed {
			continue
		}
		if err := c.apply(sk, action); err != nil {
			return report, err
		}
	}
	sort.Slice(report.Actions, func(i, j int) bool { return report.Actions[i].Skill < report.Actions[j].Skill })

	if !c.DryRun && c.State != nil {
		if err := c.State.RecordRun(report.RanAt); err != nil {
			return report, err
		}
	}
	return report, nil
}

// decide works out what should happen to one skill.
func (c *Curator) decide(ctx context.Context, sk *skill.Skill) Action {
	switch sk.State {
	case skill.StateQuarantine:
		return c.decideQuarantined(sk)
	case skill.StateActive:
		return c.decideActive(ctx, sk)
	default:
		return Action{Skill: sk.Name, Action: ActionKept, Reason: "already archived"}
	}
}

func (c *Curator) decideQuarantined(sk *skill.Skill) Action {
	expiry := c.Policy.quarantineExpiry()
	if expiry < 0 {
		return Action{Skill: sk.Name, Action: ActionKept, Reason: "quarantine expiry is disabled"}
	}
	age := c.now().Sub(sk.CreatedAt)
	if age < expiry {
		return Action{Skill: sk.Name, Action: ActionKept,
			Reason: fmt.Sprintf("in quarantine for %s, expires at %s", round(age), round(expiry))}
	}
	return Action{Skill: sk.Name, Action: ActionArchived,
		Reason: fmt.Sprintf("sat in quarantine for %s without being certified", round(age))}
}

func (c *Curator) decideActive(ctx context.Context, sk *skill.Skill) Action {
	// Retirement is considered first: re-verifying a skill nobody uses spends
	// effort proving something that is about to be put away.
	if retire := c.Policy.retireAfter(); retire > 0 {
		if idle, ok := c.idleFor(sk); ok && idle >= retire {
			return Action{Skill: sk.Name, Action: ActionArchived,
				Reason: fmt.Sprintf("unused for %s", round(idle))}
		}
	}

	if c.Policy.SkipReverify || c.Gate == nil {
		return Action{Skill: sk.Name, Action: ActionKept, Reason: "not re-verified"}
	}

	// A skill that passed once is not proven forever. Because scenarios replay
	// recordings, checking again costs nothing.
	report, err := c.Gate.Evaluate(ctx, sk.Name)
	if err != nil {
		return Action{Skill: sk.Name, Action: ActionFailed,
			Reason: "could not re-verify: " + err.Error()}
	}
	if report.Passed() {
		return Action{Skill: sk.Name, Action: ActionKept, Reason: report.Summary()}
	}
	return Action{Skill: sk.Name, Action: ActionDemoted,
		Reason: "no longer passes its own scenarios: " + report.Summary()}
}

// idleFor returns how long a skill has gone unused.
//
// A skill that has never been used is measured from when it was created, so one
// written this morning is not retired before anyone can reach for it.
//
// UpdatedAt is deliberately not part of this. It changes on every write to the
// store — bumping a use counter, pinning, even the curator saving the skill
// back — so treating it as a sign of use would make a skill look busy for
// having been touched by bookkeeping.
func (c *Curator) idleFor(sk *skill.Skill) (time.Duration, bool) {
	since := sk.LastUsedAt
	if since.IsZero() {
		since = sk.CreatedAt
	}
	if since.IsZero() {
		return 0, false
	}
	return c.now().Sub(since), true
}

func (c *Curator) apply(sk *skill.Skill, action Action) error {
	switch action.Action {
	case ActionArchived:
		return c.Skills.Archive(sk.Name, action.Reason)
	case ActionDemoted:
		return c.Skills.Demote(sk.Name, action.Reason)
	default:
		return nil
	}
}

func (c *Curator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// round trims a duration to whole hours, for readable reasons.
func round(d time.Duration) time.Duration { return d.Round(time.Hour) }

// State remembers when the curator last ran, and whether it is paused.
type State struct{ path string }

// OpenState prepares curator state at path.
func OpenState(path string) (*State, error) {
	if path == "" {
		return nil, errors.New("curator: empty state path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("curator: create state directory: %w", err)
	}
	return &State{path: path}, nil
}

type stateFile struct {
	LastRunAt time.Time `json:"last_run_at"`
	Paused    bool      `json:"paused"`
}

// Read returns when the curator last ran, and whether it is paused.
func (s *State) Read() (time.Time, bool, error) {
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("curator: read state: %w", err)
	}
	var f stateFile
	if err := json.Unmarshal(body, &f); err != nil {
		// A corrupt state file should not stop curation; the worst case is
		// running once when it was not strictly due.
		return time.Time{}, false, nil
	}
	return f.LastRunAt, f.Paused, nil
}

// RecordRun notes that the curator ran at t.
func (s *State) RecordRun(t time.Time) error {
	_, paused, err := s.Read()
	if err != nil {
		return err
	}
	return s.write(stateFile{LastRunAt: t.UTC(), Paused: paused})
}

// SetPaused stops or resumes automatic curation.
func (s *State) SetPaused(paused bool) error {
	last, _, err := s.Read()
	if err != nil {
		return err
	}
	return s.write(stateFile{LastRunAt: last, Paused: paused})
}

func (s *State) write(f stateFile) error {
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("curator: encode state: %w", err)
	}
	return os.WriteFile(s.path, append(body, '\n'), 0o600)
}
