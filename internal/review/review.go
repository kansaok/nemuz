package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/memory"
	"github.com/kansaok/nemuz/internal/skill"
)

// DefaultMaxSteps bounds a review. Reviews are meant to be short; one that
// needs many rounds is not deciding, it is rambling.
const DefaultMaxSteps = 4

// SystemPrompt instructs the reviewer.
//
// It is written to make the reviewer conservative. The failure mode of an agent
// that remembers everything is a prompt full of noise, which degrades every
// later turn — so the prompt asks for restraint explicitly, and says plainly
// that saving nothing is a normal outcome.
const SystemPrompt = `You have just watched an agent finish a turn. Your only job is to decide whether anything from it is worth keeping.

Save a memory only for a fact that will still be true next week: where something lives, how this person prefers to work, a decision that was made and why. Do not save what happened in the turn itself — the journal already has that.

Draft a skill only for a procedure you would expect to be needed again, and only if it is specific enough to follow. A draft is a proposal: it is quarantined and must pass eval scenarios before anyone can use it.

Most turns are worth nothing. Saving nothing is the normal outcome and the correct one when nothing new was learned. Never save something merely to have saved something.

When you are done, answer in one short sentence saying what you kept, or that you kept nothing.`

// Input describes the turn being reviewed.
type Input struct {
	// TurnID is the conversation turn this review is about.
	TurnID string
	// Prompt is what the user asked.
	Prompt string
	// Answer is what the agent replied.
	Answer string
	// ToolsUsed are the tools the turn invoked.
	ToolsUsed []string
	// Errored says whether the turn failed.
	Errored bool
}

// Fingerprint identifies the shape of a turn, for novelty filtering.
//
// It covers the tools used and the distinctive words of the prompt, and
// deliberately not the answer: two turns that asked the same thing and used the
// same tools have nothing new to teach, whatever the model said back.
func (in Input) Fingerprint() string {
	tools := append([]string(nil), in.ToolsUsed...)
	sort.Strings(tools)

	words := make([]string, 0, 16)
	seen := map[string]bool{}
	for _, w := range significantWords(in.Prompt) {
		if !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
	}
	sort.Strings(words)

	h := sha256.New()
	h.Write([]byte(strings.Join(tools, ",")))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(words, ",")))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Result reports what a review did.
type Result struct {
	// Reviewed is false when the turn was skipped.
	Reviewed bool
	// Skipped explains why, when it was.
	Skipped string
	// Remembered lists memory ids created.
	Remembered []string
	// Drafted lists skill names proposed, all of them quarantined.
	Drafted []string
	// TurnID is the review's own journal turn, so the decision is auditable.
	TurnID string
	// Note is the reviewer's one-line summary.
	Note string
}

// Reviewer decides what to keep from a finished turn.
type Reviewer struct {
	// Provider performs the review's model calls. It should be a cheaper or
	// smaller model than the conversation used, and it must be a separate
	// call so the main turn's prompt cache is untouched.
	Provider llm.Provider
	// Model names the model to request.
	Model string
	// Memories and Skills are where the reviewer writes.
	Memories *memory.Store
	Skills   *skill.Store
	// JournalDir and Blobs record the review as its own turn.
	JournalDir string
	Blobs      *blob.Store
	// MaxSteps bounds the review. Zero means DefaultMaxSteps.
	MaxSteps int
	// Ledger skips turns whose shape has been reviewed recently. Nil reviews
	// every turn, which is what Hermes does and what costs the most.
	Ledger *Ledger
}

// Review examines one finished turn.
func (r *Reviewer) Review(ctx context.Context, in Input) (Result, error) {
	if err := r.check(); err != nil {
		return Result{}, err
	}
	if in.TurnID == "" {
		return Result{}, fmt.Errorf("review: no turn to review")
	}

	// A turn that failed teaches nothing reliable: whatever the agent was
	// doing, it did not work, and drafting a skill from it would encode the
	// failure.
	if in.Errored {
		return Result{Skipped: "the turn ended in an error"}, nil
	}

	if r.Ledger != nil {
		fingerprint := in.Fingerprint()
		seen, err := r.Ledger.Seen(fingerprint)
		if err != nil {
			return Result{}, err
		}
		if seen {
			return Result{Skipped: "a turn of this shape was reviewed recently"}, nil
		}
		if err := r.Ledger.Record(fingerprint); err != nil {
			return Result{}, err
		}
	}

	tools, remember, draft, err := Registry(r.Memories, r.Skills, in.TurnID)
	if err != nil {
		return Result{}, err
	}

	reviewTurn, err := journal.NewTurnID()
	if err != nil {
		return Result{}, err
	}
	w, err := journal.Create(r.JournalDir, reviewTurn, r.Blobs)
	if err != nil {
		return Result{}, err
	}
	defer w.Close()

	maxSteps := r.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	a := &agent.Agent{
		Provider: llm.Record(r.Provider, w),
		Tools:    tools,
		Journal:  w,
		Model:    r.Model,
		System:   SystemPrompt,
		MaxSteps: maxSteps,
		Environment: map[string]string{
			"role":     "review",
			"reviewed": in.TurnID,
		},
	}

	outcome, runErr := a.Run(ctx, transcript(in))
	if err := w.Close(); err != nil {
		return Result{}, err
	}

	result := Result{
		Reviewed:   true,
		TurnID:     reviewTurn,
		Remembered: remember.Saved(),
		Drafted:    draft.Drafted(),
		Note:       outcome.Text,
	}
	if runErr != nil {
		// A failed review is not a failed turn. Whatever the reviewer managed
		// to save before it broke is kept, and the caller is told.
		return result, fmt.Errorf("review of %s: %w", in.TurnID, runErr)
	}
	return result, nil
}

// transcript renders the turn for the reviewer to read.
func transcript(in Input) string {
	var b strings.Builder
	b.WriteString("The user asked:\n")
	b.WriteString(in.Prompt)
	b.WriteString("\n\nThe agent answered:\n")
	b.WriteString(in.Answer)
	if len(in.ToolsUsed) > 0 {
		b.WriteString("\n\nTools used: ")
		b.WriteString(strings.Join(in.ToolsUsed, ", "))
	}
	b.WriteString("\n\nIs anything here worth keeping?")
	return b.String()
}

func (r *Reviewer) check() error {
	switch {
	case r.Provider == nil:
		return fmt.Errorf("review: no provider configured")
	case r.Memories == nil:
		return fmt.Errorf("review: no memory store configured")
	case r.Skills == nil:
		return fmt.Errorf("review: no skill store configured")
	case r.JournalDir == "":
		return fmt.Errorf("review: no journal directory configured")
	case r.Model == "":
		return fmt.Errorf("review: no model configured")
	}
	return nil
}

// significantWords reduces text to lowercase terms worth fingerprinting.
func significantWords(text string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_'
	}) {
		if len(field) >= 3 {
			out = append(out, field)
		}
	}
	return out
}
