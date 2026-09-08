package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/memory"
	"github.com/kansaok/nemuz/internal/review"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/spf13/cobra"
)

// recallIntoPrompt appends the memories relevant to a prompt, and returns their
// ids so the turn can record what it was told.
func recallIntoPrompt(base, prompt string) (string, []string, error) {
	store, err := openMemoryStore()
	if err != nil {
		return base, nil, err
	}
	recalled, err := store.Recall(prompt, memory.DefaultRecallLimit)
	if err != nil {
		return base, nil, err
	}
	if len(recalled) == 0 {
		return base, nil, nil
	}

	ids := make([]string, 0, len(recalled))
	for _, m := range recalled {
		ids = append(ids, m.ID)
	}
	return base + "\n\n" + memory.Prompt(recalled), ids, nil
}

// turnEnvironment is what gets recorded in turn.start beside the prompt.
//
// The recalled memory ids belong here rather than nowhere: a turn's answer
// depends on what the agent was reminded of, and a reader six weeks later has
// no other way to find out.
func turnEnvironment(sandboxMode string, recalled []string) map[string]string {
	env := map[string]string{"sandbox": sandboxMode}
	if len(recalled) > 0 {
		env["memories"] = strings.Join(recalled, ",")
	}
	return env
}

// reviewInput is everything the post-turn review needs.
type reviewInput struct {
	provider   llm.Provider
	model      string
	turnID     string
	prompt     string
	answer     string
	toolsUsed  []string
	journalDir string
	blobs      *blob.Store
}

// runReview decides what the finished turn was worth keeping.
//
// It runs after the answer is printed, so a slow or failing review never delays
// or breaks the turn the user was waiting for.
func runReview(ctx context.Context, in reviewInput) (review.Result, error) {
	paths, err := config.Resolve()
	if err != nil {
		return review.Result{}, err
	}
	memories, err := memory.Open(paths.Memories)
	if err != nil {
		return review.Result{}, err
	}
	skills, err := skill.Open(paths.Skills)
	if err != nil {
		return review.Result{}, err
	}
	ledger, err := review.OpenLedger(filepath.Join(paths.Root, "reviewed.txt"))
	if err != nil {
		return review.Result{}, err
	}

	r := &review.Reviewer{
		Provider:   in.provider,
		Model:      in.model,
		Memories:   memories,
		Skills:     skills,
		JournalDir: in.journalDir,
		Blobs:      in.blobs,
		Ledger:     ledger,
	}
	return r.Review(ctx, review.Input{
		TurnID:    in.turnID,
		Prompt:    in.prompt,
		Answer:    in.answer,
		ToolsUsed: in.toolsUsed,
	})
}

// reportReview prints what the review decided.
//
// A failed review is reported and then dropped. The turn already succeeded, and
// failing the command afterwards would punish the user for a background task
// they did not ask for.
func reportReview(cmd *cobra.Command, result review.Result, err error) {
	out := cmd.OutOrStdout()
	if err != nil {
		fmt.Fprintf(out, "\nreview failed: %v\n", err)
		return
	}
	if !result.Reviewed {
		fmt.Fprintf(out, "\nreview skipped — %s\n", result.Skipped)
		return
	}
	if len(result.Remembered) == 0 && len(result.Drafted) == 0 {
		fmt.Fprintf(out, "\nreview: nothing worth keeping\n")
		return
	}

	fmt.Fprintln(out)
	if n := len(result.Remembered); n > 0 {
		fmt.Fprintf(out, "review: remembered %d fact(s) — nemuz memory ls\n", n)
	}
	for _, name := range result.Drafted {
		fmt.Fprintf(out, "review: drafted skill %q in quarantine — nemuz skill show %s\n", name, name)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
