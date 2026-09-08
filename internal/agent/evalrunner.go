package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/kansaok/nemuz/internal/tool"
)

// ScenarioRunner runs a skill's eval scenarios by replaying recorded turns.
//
// # Why this costs nothing
//
// The model's answers come from the recording, so certifying a skill makes no
// API calls and needs no network. That is the point: a gate that costs money
// every time gets skipped, and a gate that gets skipped protects nothing.
//
// # Why replay is lenient here
//
// Adding a skill changes the system prompt, so the request necessarily differs
// from the recording. Strict replay would fail on the first call for a reason
// that says nothing about whether the skill is any good. Evals therefore assert
// on behaviour — which tools ran, what the answer said, how long it took —
// while strict replay stays what it is: the check that nemuz itself has not
// changed its mind.
type ScenarioRunner struct {
	// Tools are offered to the model during evaluation. They should be the
	// same set the skill will have in service.
	Tools *tool.Registry
	// Blobs resolves spilled payloads in recorded turns.
	Blobs *blob.Store
	// JournalDir is where each evaluation writes its own turn.
	JournalDir string
	// Model overrides the model named in the recording.
	Model string
	// BaseSystem is the system prompt the skill is appended to.
	BaseSystem string
	// MaxSteps bounds an evaluation turn.
	MaxSteps int
}

// Run implements skill.Runner.
func (r *ScenarioRunner) Run(ctx context.Context, sk *skill.Skill, sc skill.Scenario) (skill.Outcome, error) {
	recorded, err := journal.Read(sc.Journal)
	if err != nil {
		return skill.Outcome{}, fmt.Errorf("scenario %s: %w", sc.Name, err)
	}
	cassette, err := journal.CassetteFrom(recorded, r.Blobs)
	if err != nil {
		return skill.Outcome{}, fmt.Errorf("scenario %s: %w", sc.Name, err)
	}

	start, err := recordedStart(recorded, r.Blobs)
	if err != nil {
		return skill.Outcome{}, fmt.Errorf("scenario %s: %w", sc.Name, err)
	}
	prompt := sc.Prompt
	if prompt == "" {
		prompt = start.Prompt
	}
	if prompt == "" {
		return skill.Outcome{}, fmt.Errorf("scenario %s: neither the scenario nor the recording supplies a prompt", sc.Name)
	}
	model := r.Model
	if model == "" {
		model = start.Model
	}

	turnID, err := journal.NewTurnID()
	if err != nil {
		return skill.Outcome{}, err
	}
	w, err := journal.Create(r.JournalDir, turnID, r.Blobs)
	if err != nil {
		return skill.Outcome{}, err
	}
	defer w.Close()

	a := &Agent{
		Provider: llm.Replay(cassette, w).Lenient(),
		Tools:    r.Tools,
		Journal:  w,
		Model:    model,
		System:   systemWithSkill(r.BaseSystem, sk),
		MaxSteps: r.MaxSteps,
	}

	outcome, runErr := a.Run(ctx, prompt)
	if err := w.Close(); err != nil {
		return skill.Outcome{}, err
	}

	events, err := journal.Read(w.Path())
	if err != nil {
		return skill.Outcome{}, err
	}

	called, failed := toolOutcomes(events)
	result := skill.Outcome{
		Text:        outcome.Text,
		Steps:       outcome.Steps,
		ToolsCalled: called,
		ToolsFailed: failed,
		Errored:     runErr != nil || hasError(events),
		TurnID:      turnID,
	}
	// A turn that failed is a result the expectations get to judge, not an
	// error the runner raises: a scenario may exist precisely to assert that
	// a skill stops the agent from doing something.
	return result, nil
}

// systemWithSkill appends a skill's instructions to the base system prompt.
func systemWithSkill(base string, sk *skill.Skill) string {
	if sk == nil {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return sk.Prompt()
	}
	return base + "\n\n" + sk.Prompt()
}

// toolOutcomes lists the tools a turn invoked and those whose results came back
// as failures, in call order with repeats collapsed.
//
// The two are separate because they answer different questions: what the agent
// decided to do, and whether the environment let it.
func toolOutcomes(events []journal.Event) (called, failed []string) {
	seenCall := map[string]bool{}
	seenFail := map[string]bool{}
	for _, e := range events {
		var payload struct {
			Name    string `json:"name"`
			IsError bool   `json:"is_error"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.Name == "" {
			continue
		}
		switch e.Kind {
		case journal.KindToolCall:
			if !seenCall[payload.Name] {
				seenCall[payload.Name] = true
				called = append(called, payload.Name)
			}
		case journal.KindToolResult:
			if payload.IsError && !seenFail[payload.Name] {
				seenFail[payload.Name] = true
				failed = append(failed, payload.Name)
			}
		}
	}
	return called, failed
}

func hasError(events []journal.Event) bool {
	for _, e := range events {
		if e.Kind == journal.KindError {
			return true
		}
	}
	return false
}

// recordedStart is the opening event of a recorded turn.
type recordedTurnStart struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	System string `json:"system"`
}

func recordedStart(events []journal.Event, bs *blob.Store) (recordedTurnStart, error) {
	for _, e := range events {
		if e.Kind != journal.KindTurnStart {
			continue
		}
		body, err := e.Content(bs)
		if err != nil {
			return recordedTurnStart{}, err
		}
		var start recordedTurnStart
		if err := json.Unmarshal(body, &start); err != nil {
			return recordedTurnStart{}, fmt.Errorf("decode turn.start: %w", err)
		}
		return start, nil
	}
	return recordedTurnStart{}, fmt.Errorf("the recording has no turn.start event")
}
