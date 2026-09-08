package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultMinScenarios is how many scenarios a skill must carry to be promoted.
//
// One is a low bar, and it is deliberate: one scenario is infinitely better
// than none, and a bar high enough to block useful skills gets switched off.
// Raise it per store where the stakes justify it.
const DefaultMinScenarios = 1

// Scenario is one recorded turn plus what the skill is expected to make happen.
//
// The recording supplies the model's answers, so running a scenario costs no
// API calls. That is what makes it reasonable to demand evidence before every
// promotion instead of only when someone remembers to ask.
type Scenario struct {
	// Name identifies the scenario within its skill.
	Name string `json:"name"`
	// Description says what this scenario is checking, for whoever reads a
	// failure report later.
	Description string `json:"description"`
	// Journal is the recorded turn to replay, as a path relative to the
	// scenario file.
	Journal string `json:"journal"`
	// Prompt is the turn's prompt. It comes from the recording when empty.
	Prompt string `json:"prompt,omitempty"`
	// Expect is what the replay must produce.
	Expect Expectations `json:"expect"`
}

// Expectations are assertions about a scenario's outcome.
//
// They describe behaviour, not bytes. A byte-identical check would be the wrong
// tool here: adding a skill changes the system prompt, so the request changes by
// construction. What must not change is what the agent decides to do.
type Expectations struct {
	// MustCall lists tools the turn has to invoke.
	MustCall []string `json:"must_call,omitempty"`
	// MustNotCall lists tools the turn must stay away from — the clearest way
	// to state a safety property.
	MustNotCall []string `json:"must_not_call,omitempty"`
	// MustContain lists substrings the final answer must include.
	MustContain []string `json:"must_contain,omitempty"`
	// MustNotContain lists substrings the answer must avoid.
	MustNotContain []string `json:"must_not_contain,omitempty"`
	// MaxSteps caps how many model rounds the turn may take. A skill that
	// makes the agent flail is a regression even when it eventually answers.
	MaxSteps int `json:"max_steps,omitempty"`
	// MustSucceed requires the turn to finish cleanly: no error event, and no
	// tool that came back as a failure.
	MustSucceed bool `json:"must_succeed,omitempty"`
}

// Outcome is what running a scenario produced.
type Outcome struct {
	Text string
	// ToolsCalled lists the tools the agent decided to invoke, whether or not
	// they succeeded — a decision is worth asserting on either way.
	ToolsCalled []string
	// ToolsFailed lists those whose result came back as an error, including
	// tools that were not available at all. Certifying a skill against a
	// toolset that does not match production is a way to be lied to, so this
	// is tracked separately rather than folded into ToolsCalled.
	ToolsFailed []string
	Steps       int
	Errored     bool
	TurnID      string
}

// Runner executes one scenario against a candidate skill.
//
// It is injected rather than imported so this package stays free of the agent,
// the providers, and the journal — and so the gate's own logic can be tested
// without any of them.
type Runner func(ctx context.Context, sk *Skill, sc Scenario) (Outcome, error)

// Result is one scenario's verdict.
type Result struct {
	Scenario string   `json:"scenario"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
	TurnID   string   `json:"turn,omitempty"`
}

// Report is the evidence a promotion rests on.
type Report struct {
	// ID identifies this run, and is recorded on the skill it promotes so the
	// evidence can be found again.
	ID      string    `json:"id"`
	Skill   string    `json:"skill"`
	RanAt   time.Time `json:"ran_at"`
	Results []Result  `json:"results"`
	// MinScenarios is the bar this run was held to.
	MinScenarios int `json:"min_scenarios"`
}

// Passed reports whether the skill may be promoted.
//
// Every scenario must pass, and there must be enough of them. A skill with no
// scenarios fails: silence is not evidence.
func (r Report) Passed() bool {
	if len(r.Results) < r.MinScenarios || len(r.Results) == 0 {
		return false
	}
	for _, res := range r.Results {
		if !res.Passed {
			return false
		}
	}
	return true
}

// Summary explains the verdict in one line.
func (r Report) Summary() string {
	if len(r.Results) == 0 {
		return fmt.Sprintf("it has no eval scenarios; write at least %d", r.MinScenarios)
	}
	if len(r.Results) < r.MinScenarios {
		return fmt.Sprintf("it has %d scenario(s) but %d are required", len(r.Results), r.MinScenarios)
	}
	var failed []string
	for _, res := range r.Results {
		if !res.Passed {
			failed = append(failed, res.Scenario)
		}
	}
	if len(failed) == 0 {
		return fmt.Sprintf("%d of %d scenarios passed", len(r.Results), len(r.Results))
	}
	return fmt.Sprintf("%d of %d scenarios failed: %s", len(failed), len(r.Results), strings.Join(failed, ", "))
}

// Gate decides whether a skill has earned its way out of quarantine.
type Gate struct {
	Store *Store
	Run   Runner
	// MinScenarios overrides DefaultMinScenarios.
	MinScenarios int
	// NewID generates report ids. Defaults to a timestamp.
	NewID func() string
}

// Evaluate runs every scenario a skill carries and returns the report.
//
// It never changes the skill. Promotion is a separate, explicit step, so an
// operator can read the evidence before acting on it.
func (g *Gate) Evaluate(ctx context.Context, name string) (Report, error) {
	sk, err := g.Store.Load(name)
	if err != nil {
		return Report{}, err
	}
	scenarios, err := g.Store.Scenarios(name)
	if err != nil {
		return Report{}, err
	}

	min := g.MinScenarios
	if min <= 0 {
		min = DefaultMinScenarios
	}
	report := Report{
		ID:           g.newID(),
		Skill:        name,
		RanAt:        time.Now().UTC(),
		MinScenarios: min,
	}

	for _, sc := range scenarios {
		outcome, err := g.Run(ctx, sk, sc)
		if err != nil {
			// A scenario that cannot run is a failure, not an excuse. A broken
			// recording must not become a way past the gate.
			report.Results = append(report.Results, Result{
				Scenario: sc.Name,
				Failures: []string{"could not run: " + err.Error()},
			})
			continue
		}
		report.Results = append(report.Results, check(sc, outcome))
	}
	return report, nil
}

// Certify evaluates a skill and promotes it if it passes.
func (g *Gate) Certify(ctx context.Context, name string) (Report, error) {
	report, err := g.Evaluate(ctx, name)
	if err != nil {
		return report, err
	}
	if !report.Passed() {
		return report, fmt.Errorf("skill %s stays in quarantine: %s", name, report.Summary())
	}
	return report, g.Store.Promote(name, report)
}

func (g *Gate) newID() string {
	if g.NewID != nil {
		return g.NewID()
	}
	return "eval-" + time.Now().UTC().Format("20060102T150405Z")
}

// check evaluates one outcome against its expectations.
func check(sc Scenario, out Outcome) Result {
	res := Result{Scenario: sc.Name, TurnID: out.TurnID}
	called := make(map[string]bool, len(out.ToolsCalled))
	for _, name := range out.ToolsCalled {
		called[name] = true
	}
	failed := make(map[string]bool, len(out.ToolsFailed))
	for _, name := range out.ToolsFailed {
		failed[name] = true
	}

	for _, want := range sc.Expect.MustCall {
		switch {
		case !called[want]:
			res.Failures = append(res.Failures, fmt.Sprintf("never called %s (called: %s)", want, listOr(out.ToolsCalled, "nothing")))
		case failed[want]:
			// Reaching for a tool that then fails is not evidence the skill
			// works. It usually means the eval ran without the tools the skill
			// depends on.
			res.Failures = append(res.Failures, fmt.Sprintf("called %s but it failed; is it registered in this environment?", want))
		}
	}
	for _, avoid := range sc.Expect.MustNotCall {
		if called[avoid] {
			res.Failures = append(res.Failures, fmt.Sprintf("called %s, which this scenario forbids", avoid))
		}
	}
	for _, want := range sc.Expect.MustContain {
		if !strings.Contains(out.Text, want) {
			res.Failures = append(res.Failures, fmt.Sprintf("the answer does not mention %q", want))
		}
	}
	for _, avoid := range sc.Expect.MustNotContain {
		if strings.Contains(out.Text, avoid) {
			res.Failures = append(res.Failures, fmt.Sprintf("the answer mentions %q, which this scenario forbids", avoid))
		}
	}
	if sc.Expect.MaxSteps > 0 && out.Steps > sc.Expect.MaxSteps {
		res.Failures = append(res.Failures, fmt.Sprintf("took %d steps, over the limit of %d", out.Steps, sc.Expect.MaxSteps))
	}
	if sc.Expect.MustSucceed {
		if out.Errored {
			res.Failures = append(res.Failures, "the turn ended in an error")
		}
		if len(out.ToolsFailed) > 0 {
			res.Failures = append(res.Failures, "these tools failed: "+strings.Join(out.ToolsFailed, ", "))
		}
	}

	res.Passed = len(res.Failures) == 0
	return res
}

func listOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, ", ")
}

// Scenarios reads a skill's eval scenarios from disk.
func (s *Store) Scenarios(name string) ([]Scenario, error) {
	dir, err := s.EvalDir(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill %s: read scenarios: %w", name, err)
	}

	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("skill %s: read %s: %w", name, e.Name(), err)
		}
		var sc Scenario
		if err := json.Unmarshal(body, &sc); err != nil {
			return nil, fmt.Errorf("skill %s: %s is not a valid scenario: %w", name, e.Name(), err)
		}
		if sc.Name == "" {
			sc.Name = strings.TrimSuffix(e.Name(), ".json")
		}
		if sc.Journal == "" {
			return nil, fmt.Errorf("skill %s: scenario %s names no recorded turn", name, sc.Name)
		}
		// Journal paths are resolved here so a scenario cannot point outside
		// its own skill directory.
		resolved, err := safeJoin(dir, sc.Journal)
		if err != nil {
			return nil, fmt.Errorf("skill %s: scenario %s: %w", name, sc.Name, err)
		}
		sc.Journal = resolved
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SaveScenario writes a scenario into a skill's eval directory.
func (s *Store) SaveScenario(name string, sc Scenario) error {
	if sc.Name == "" {
		return fmt.Errorf("skill %s: a scenario needs a name", name)
	}
	if !ValidName(sc.Name) {
		return fmt.Errorf("skill %s: %q is not a valid scenario name", name, sc.Name)
	}
	dir, err := s.EvalDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("skill %s: create eval directory: %w", name, err)
	}
	body, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return fmt.Errorf("skill %s: encode scenario: %w", name, err)
	}
	return os.WriteFile(filepath.Join(dir, sc.Name+".json"), append(body, '\n'), 0o600)
}

// AttachScenario stores a scenario together with the recording it replays.
//
// A scenario is only useful next to its recording: the two travel together when
// a skill is copied between machines, and a scenario pointing at a journal that
// moved is a scenario that fails for the wrong reason. This copies the turn in
// rather than referring to it.
func (s *Store) AttachScenario(name string, sc Scenario, journalPath string) error {
	if sc.Name == "" {
		return fmt.Errorf("skill %s: a scenario needs a name", name)
	}
	dir, err := s.EvalDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("skill %s: create eval directory: %w", name, err)
	}

	recording, err := os.ReadFile(journalPath)
	if err != nil {
		return fmt.Errorf("skill %s: read the recording: %w", name, err)
	}
	// Named after the scenario so two scenarios can replay different turns.
	recordingName := sc.Name + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, recordingName), recording, 0o600); err != nil {
		return fmt.Errorf("skill %s: store the recording: %w", name, err)
	}

	sc.Journal = recordingName
	return s.SaveScenario(name, sc)
}

// RemoveScenario deletes a scenario and the recording it owned.
func (s *Store) RemoveScenario(name, scenario string) error {
	dir, err := s.EvalDir(name)
	if err != nil {
		return err
	}
	if !ValidName(scenario) {
		return fmt.Errorf("skill %s: %q is not a valid scenario name", name, scenario)
	}
	found := false
	for _, f := range []string{scenario + ".json", scenario + ".jsonl"} {
		err := os.Remove(filepath.Join(dir, f))
		if err == nil {
			found = true
			continue
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("skill %s: remove %s: %w", name, f, err)
		}
	}
	if !found {
		return fmt.Errorf("skill %s: has no scenario called %q", name, scenario)
	}
	return nil
}

// safeJoin resolves rel under base, refusing anything that escapes.
func safeJoin(base, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("journal path must be relative, got %s", rel)
	}
	joined := filepath.Clean(filepath.Join(base, rel))
	if joined != base && !strings.HasPrefix(joined, base+string(filepath.Separator)) {
		return "", fmt.Errorf("journal path %s leaves the skill directory", rel)
	}
	return joined, nil
}
