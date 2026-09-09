package consolidate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/review"
	"github.com/kansaok/nemuz/internal/skill"
)

// SystemPrompt instructs the model comparing two skills.
//
// It asks for the same restraint review's prompt asks for merging memories:
// most pairs that look similar on the surface are not actually the same idea,
// and declining is the ordinary, correct outcome.
const SystemPrompt = `You are comparing two procedures an agent taught itself, to decide whether they are the same idea written twice.

Merge them only if a person reading both would say "these do the same thing" — not merely that they touch similar topics or share some steps. If they differ in when they apply, what they assume, or what they actually do, they are not redundant, and keeping them separate is the correct and common outcome.

If you merge, write a single procedure that covers what both originals covered. Do not simply pick one and discard the other's detail.

Write in the language the originals are written in.

Use exactly one tool: merge_skills or keep_separate. After it returns, close with one short sentence saying what you decided.`

// DefaultMaxSteps bounds a comparison. Like review, this is a short,
// conservative call, not a conversation.
const DefaultMaxSteps = 3

// Decision is the outcome for one pair.
const (
	DecisionMerged       = "merged"
	DecisionKeptSeparate = "kept_separate"
	DecisionFailed       = "failed"
)

// Outcome is what happened to one candidate pair.
type Outcome struct {
	A, B     string
	Decision string
	// NewSkill is the merged skill's name, set only when Decision is Merged.
	NewSkill string
	// Reason explains a kept_separate decision, or a failure.
	Reason string
	// TurnID is the comparison's own recorded turn, so the reasoning behind a
	// decision is auditable the same way a review's is.
	TurnID string
	// Skipped is true when the pair was not asked about at all, because the
	// ledger had it recorded as recently considered.
	Skipped bool
}

// Report is what a consolidation run did.
type Report struct {
	RanAt    time.Time
	Outcomes []Outcome
}

// Merged counts how many pairs actually merged.
func (r Report) Merged() int {
	var n int
	for _, o := range r.Outcomes {
		if o.Decision == DecisionMerged {
			n++
		}
	}
	return n
}

// Consolidator finds overlapping skills and asks a model to compare them.
type Consolidator struct {
	// Provider performs the comparison's model calls. A smaller model is
	// reasonable here, the same way review's is: judging whether two short
	// procedures overlap is an easier task than the turns that produced them.
	Provider llm.Provider
	Model    string
	// Skills is the store to read from and write merges into.
	Skills *skill.Store
	// JournalDir and Blobs record each comparison as its own turn.
	JournalDir string
	Blobs      *blob.Store
	// MaxPairs bounds how many candidates are considered per run. Zero uses
	// DefaultMaxPairs.
	MaxPairs int
	// MaxSteps bounds one comparison. Zero uses DefaultMaxSteps.
	MaxSteps int
	// Ledger skips pairs considered recently. Nil asks about every candidate
	// every run.
	Ledger *review.Ledger
	// Now is the time source. Nil uses time.Now.
	Now func() time.Time
}

// Run finds candidate pairs and asks the model about each.
func (c *Consolidator) Run(ctx context.Context) (Report, error) {
	if err := c.check(); err != nil {
		return Report{}, err
	}
	report := Report{RanAt: c.now().UTC()}

	all, err := c.Skills.List()
	if err != nil {
		return report, err
	}
	pairs := FindCandidates(all, c.MaxPairs)

	for _, pair := range pairs {
		outcome, err := c.consider(ctx, pair)
		if err != nil {
			return report, err
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}
	return report, nil
}

func (c *Consolidator) consider(ctx context.Context, pair Pair) (Outcome, error) {
	base := Outcome{A: pair.A.Name, B: pair.B.Name}

	if c.Ledger != nil {
		fp := pair.Fingerprint()
		seen, err := c.Ledger.Seen(fp)
		if err != nil {
			return base, err
		}
		if seen {
			base.Skipped = true
			return base, nil
		}
		if err := c.Ledger.Record(fp); err != nil {
			return base, err
		}
	}

	reg, merge, keep, err := Registry(pair.A, pair.B)
	if err != nil {
		return base, err
	}

	turnID, err := journal.NewTurnID()
	if err != nil {
		return base, err
	}
	w, err := journal.Create(c.JournalDir, turnID, c.Blobs)
	if err != nil {
		return base, err
	}
	defer w.Close()

	maxSteps := c.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	a := &agent.Agent{
		Provider:    llm.Record(c.Provider, w),
		Tools:       reg,
		Journal:     w,
		Model:       c.Model,
		System:      SystemPrompt,
		MaxSteps:    maxSteps,
		Environment: map[string]string{"role": "consolidate", "compares": pair.A.Name + "," + pair.B.Name},
	}

	_, runErr := a.Run(ctx, comparisonPrompt(pair.A, pair.B))
	if closeErr := w.Close(); closeErr != nil && runErr == nil {
		runErr = closeErr
	}
	base.TurnID = turnID

	// The decision is read from which tool actually fired, checked before
	// runErr rather than instead of it. A model that decides correctly and
	// then produces an empty closing sentence — observed against a real
	// provider, where the agent loop treats an empty final answer as a turn
	// failure — must not have its already-captured decision discarded because
	// of what happened one step later.
	if name, desc, body, ok := merge.Proposed(); ok {
		newName, err := c.applyMerge(pair, name, desc, body)
		if err != nil {
			base.Decision, base.Reason = DecisionFailed, err.Error()
			return base, nil
		}
		base.Decision, base.NewSkill = DecisionMerged, newName
		return base, nil
	}
	if reason, ok := keep.Decided(); ok {
		base.Decision, base.Reason = DecisionKeptSeparate, reason
		return base, nil
	}

	if runErr != nil {
		base.Decision, base.Reason = DecisionFailed, runErr.Error()
		return base, nil
	}
	base.Decision, base.Reason = DecisionFailed, "the model answered without using either tool"
	return base, nil
}

// applyMerge creates the merged skill, carries over both sources' scenarios,
// and archives the sources.
//
// The merged skill starts in quarantine like any skill the agent writes for
// itself — a merge proposed by a model is not evidence that it works, only a
// claim to be checked the same way any other agent-written skill is. Carried-
// over scenarios still replay the same recorded turns; if the merged body no
// longer produces the same behaviour, they will fail, which is the gate
// working as intended rather than a problem with reusing them.
func (c *Consolidator) applyMerge(pair Pair, name, description, body string) (string, error) {
	name = strings.TrimSpace(name)
	if !skill.ValidName(name) {
		return "", fmt.Errorf("the proposed name %q is not usable", name)
	}
	// A name colliding with something other than the two being merged would
	// silently overwrite an unrelated skill.
	if existing, err := c.Skills.Load(name); err == nil {
		if existing.Name != pair.A.Name && existing.Name != pair.B.Name {
			return "", fmt.Errorf("a different skill called %q already exists", name)
		}
	}

	merged, err := skill.New(name, description, body, skill.ByAgent, c.now())
	if err != nil {
		return "", err
	}

	// A source whose name the merge kept already has its own scenarios in
	// place — copying them back into the same directory under a new name
	// would just duplicate them pointlessly.
	if pair.A.Name != name {
		if err := c.carryScenarios(pair.A, name, pair.A.Name+"-"); err != nil {
			return "", err
		}
	}
	if pair.B.Name != name {
		if err := c.carryScenarios(pair.B, name, pair.B.Name+"-"); err != nil {
			return "", err
		}
	}
	// Saved after copying scenarios in, not before: if copying fails partway,
	// no skill file claims evidence it does not fully have yet.
	if err := c.Skills.Save(merged); err != nil {
		return "", err
	}

	reason := fmt.Sprintf("consolidated into %s", name)
	// A source whose name the model chose to keep IS the merged skill now:
	// saving it above already replaced that file with the merged, quarantined
	// content. Archiving it again would immediately undo the merge — Archive
	// would load the file just written, and turn the fresh quarantine skill
	// back into an archived one before anyone ever saw it. Found by testing
	// against a real model, which reused one source's name where every
	// scripted test in this package had always invented a new one.
	if pair.A.Name != name {
		if err := c.Skills.Archive(pair.A.Name, reason); err != nil {
			return "", err
		}
	}
	if pair.B.Name != name {
		if err := c.Skills.Archive(pair.B.Name, reason); err != nil {
			return "", err
		}
	}
	return name, nil
}

// carryScenarios copies one source skill's eval scenarios into the merged
// skill's eval directory, prefixing scenario names to avoid collisions when
// both sources happen to name theirs the same thing (commonly "basic").
func (c *Consolidator) carryScenarios(from *skill.Skill, toName, prefix string) error {
	scenarios, err := c.Skills.Scenarios(from.Name)
	if err != nil {
		return fmt.Errorf("read %s's scenarios: %w", from.Name, err)
	}
	for _, sc := range scenarios {
		sc.Name = prefix + sc.Name
		if err := c.Skills.AttachScenario(toName, sc, sc.Journal); err != nil {
			return fmt.Errorf("carry scenario %s from %s: %w", sc.Name, from.Name, err)
		}
	}
	return nil
}

func comparisonPrompt(a, b *skill.Skill) string {
	return fmt.Sprintf(
		"Skill A: %s\n%s\n\n%s\n\nSkill B: %s\n%s\n\n%s\n\nAre these the same idea written twice?",
		a.Name, a.Description, a.Body, b.Name, b.Description, b.Body)
}

func (c *Consolidator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Consolidator) check() error {
	switch {
	case c.Provider == nil:
		return fmt.Errorf("consolidate: no provider configured")
	case c.Skills == nil:
		return fmt.Errorf("consolidate: no skill store configured")
	case c.JournalDir == "":
		return fmt.Errorf("consolidate: no journal directory configured")
	case c.Model == "":
		return fmt.Errorf("consolidate: no model configured")
	}
	return nil
}
