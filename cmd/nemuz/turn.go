package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/spf13/cobra"
)

// turnSession holds everything a turn needs that stays the same across many
// turns in one process — the provider, the toolset, where state lives — so
// `nemuz chat` can run a whole conversation without reopening any of it, the
// same way `nemuz run` opens it once for its one turn.
type turnSession struct {
	paths       config.Paths
	provider    llm.Provider
	model       string
	ts          *toolset
	bs          *blob.Store
	baseSystem  string
	maxSteps    int
	useSkills   bool
	useMemories bool
	doReview    bool
	reviewModel string
	doCurate    bool
}

// runTurn runs skills, recall, the model, journaling, review, and curation —
// the sequence every turn goes through, kept in one place so `nemuz run` and
// `nemuz chat` cannot drift into answering differently for the same prompt.
//
// ctx carries the model call's cancellation and, when the delegate tool is
// registered, the remaining delegation depth — so a sub-agent's own runTurn
// call inherits one less level than its caller rather than a fresh budget.
func (s *turnSession) runTurn(ctx context.Context, cmd *cobra.Command, out io.Writer, prompt string, verbose bool) (agent.Outcome, string, error) {
	systemPrompt, skillNames := s.baseSystem, []string(nil)
	var err error
	if s.useSkills {
		systemPrompt, skillNames, err = activeSkillPrompt(s.baseSystem)
		if err != nil {
			return agent.Outcome{}, "", err
		}
	}

	// Recall is deterministic, so the memories a turn carries are a function
	// of the store and the prompt — and the ids are recorded below, so a
	// later reader can see what the agent was told.
	var recalled []string
	if s.useMemories {
		systemPrompt, recalled, err = recallIntoPrompt(systemPrompt, prompt)
		if err != nil {
			return agent.Outcome{}, "", err
		}
	}

	turnID, err := journal.NewTurnID()
	if err != nil {
		return agent.Outcome{}, "", err
	}
	w, err := journal.Create(s.paths.Journal, turnID, s.bs)
	if err != nil {
		return agent.Outcome{}, "", err
	}
	defer w.Close()

	a := &agent.Agent{
		Provider:    llm.Record(s.provider, w),
		Tools:       s.ts.Registry,
		Journal:     w,
		Model:       s.model,
		System:      systemPrompt,
		MaxSteps:    s.maxSteps,
		Environment: turnEnvironment(s.ts.Sandbox, recalled),
	}

	if verbose {
		fmt.Fprintf(out, "turn %s · %s · %s\nsandbox %s · %d tools: %s\n",
			turnID, s.provider.Name(), s.ts.Workspace, s.ts.Sandbox, s.ts.Registry.Len(), strings.Join(s.ts.Registry.Names(), ", "))
		if len(skillNames) > 0 {
			fmt.Fprintf(out, "%d skills: %s\n", len(skillNames), strings.Join(skillNames, ", "))
		}
		if len(recalled) > 0 {
			fmt.Fprintf(out, "%d memories recalled\n", len(recalled))
		}
		fmt.Fprintln(out)
	}

	outcome, runErr := a.Run(ctx, prompt)
	if closeErr := w.Close(); closeErr != nil && runErr == nil {
		runErr = closeErr
	}
	if runErr != nil {
		fmt.Fprintf(out, "\nThe turn failed, but it was recorded: nemuz journal show %s\n", turnID)
		return outcome, turnID, runErr
	}

	// The index is what makes `nemuz search` and `nemuz usage` work. It is
	// derived from the journal, so this is a convenience rather than a
	// commitment: `nemuz index rebuild` restores it exactly.
	indexTurn(cmd, turnID)

	// Recall is only useful if it improves over time, which means noting
	// which memories actually got used.
	if len(recalled) > 0 {
		if store, err := openMemoryStore(); err == nil {
			_ = store.RecordUse(recalled...)
		}
	}

	if s.doReview {
		result, reviewErr := runReview(cmd.Context(), reviewInput{
			provider:   s.provider,
			model:      firstNonEmpty(s.reviewModel, s.model),
			turnID:     turnID,
			prompt:     prompt,
			answer:     outcome.Text,
			toolsUsed:  s.ts.Registry.Names(),
			journalDir: s.paths.Journal,
			blobs:      s.bs,
		})
		reportReview(cmd, result, reviewErr)
		indexTurn(cmd, result.TurnID)
	}
	if s.doCurate {
		curateIfDue(cmd, s.ts)
	}
	return outcome, turnID, nil
}
