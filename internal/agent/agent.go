// Package agent runs the turn loop: ask the model, run the tools it asks for,
// feed the results back, and stop when it is done.
//
// The loop itself is deliberately small and holds no provider-specific or
// transport-specific knowledge. Model calls arrive through an llm.Provider that
// has already been wrapped for recording or replay, so the loop behaves
// identically whether it is talking to an API or to a cassette — which is
// exactly what makes a replayed turn trustworthy as evidence.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/tool"
)

// DefaultMaxSteps bounds a turn so a model that keeps asking for tools cannot
// loop forever.
const DefaultMaxSteps = 16

// Agent executes one turn at a time.
type Agent struct {
	// Provider performs model calls. Wrap it with llm.Record or llm.Replay
	// before handing it over; the loop does not journal model traffic itself.
	Provider llm.Provider
	// Tools are the tools offered to the model.
	Tools *tool.Registry
	// Journal receives turn, tool, and error events.
	Journal *journal.Writer
	// Model names the model to request.
	Model string
	// System is the system prompt.
	System string
	// MaxSteps bounds the tool loop. Zero means DefaultMaxSteps.
	MaxSteps int
}

// Outcome summarises a completed turn.
type Outcome struct {
	Text      string    `json:"text"`
	Steps     int       `json:"steps"`
	ToolCalls int       `json:"tool_calls"`
	Usage     llm.Usage `json:"usage"`
	TurnID    string    `json:"turn"`
}

// ErrStepLimit means the model kept asking for tools past MaxSteps.
var ErrStepLimit = errors.New("agent: step limit reached")

// Run executes one turn for prompt.
func (a *Agent) Run(ctx context.Context, prompt string) (Outcome, error) {
	if err := a.check(); err != nil {
		return Outcome{}, err
	}
	maxSteps := a.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	out := Outcome{TurnID: a.Journal.TurnID()}
	// turn.start carries everything needed to rebuild the first request. A
	// replay that cannot reconstruct the system prompt would build a different
	// request and be rejected — correctly, but unhelpfully.
	if _, err := a.Journal.Append(journal.KindTurnStart, map[string]any{
		"prompt": prompt,
		"model":  a.Model,
		"system": a.System,
		"tools":  a.Tools.Names(),
	}); err != nil {
		return out, err
	}

	messages := []llm.Message{{Role: llm.RoleUser, Text: prompt}}
	defs := a.Tools.Defs()

	for step := 1; step <= maxSteps; step++ {
		out.Steps = step

		resp, err := a.Provider.Complete(ctx, llm.Request{
			Model:    a.Model,
			System:   a.System,
			Messages: messages,
			Tools:    defs,
		})
		if err != nil {
			return out, a.fail(out, "model", err)
		}
		if err := resp.Validate(); err != nil {
			return out, a.fail(out, "model", err)
		}
		out.Usage.InputTokens += resp.Usage.InputTokens
		out.Usage.OutputTokens += resp.Usage.OutputTokens
		out.Usage.CachedTokens += resp.Usage.CachedTokens

		if !resp.WantsTools() {
			out.Text = resp.Text
			return out, a.finish(out, "end")
		}

		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})
		for _, call := range resp.ToolCalls {
			result, err := a.runTool(ctx, call)
			if err != nil {
				return out, a.fail(out, "tool", err)
			}
			out.ToolCalls++
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Text:       result.Content,
				IsError:    result.IsError,
			})
		}
	}

	err := fmt.Errorf("%w after %d steps", ErrStepLimit, maxSteps)
	return out, a.fail(out, "loop", err)
}

// runTool journals the call, executes it, and journals the result.
//
// A tool that reports IsError is not a loop failure: the model sees the message
// and gets a chance to recover, which is usually better than aborting the turn.
func (a *Agent) runTool(ctx context.Context, call llm.ToolCall) (tool.Result, error) {
	if _, err := a.Journal.Append(journal.KindToolCall, map[string]any{
		"id":   call.ID,
		"name": call.Name,
		"args": json.RawMessage(call.Args),
	}); err != nil {
		return tool.Result{}, err
	}

	t, ok := a.Tools.Get(call.Name)
	if !ok {
		// An unknown tool is the model's mistake, so it is reported back as a
		// tool error rather than aborting the turn.
		result := tool.Errorf("no tool named %q is available; the tools are: %v", call.Name, a.Tools.Names())
		return result, a.journalResult(call, result)
	}

	result, err := t.Run(ctx, call.Args)
	if err != nil {
		return tool.Result{}, fmt.Errorf("agent: tool %s: %w", call.Name, err)
	}
	return result, a.journalResult(call, result)
}

func (a *Agent) journalResult(call llm.ToolCall, result tool.Result) error {
	_, err := a.Journal.Append(journal.KindToolResult, map[string]any{
		"id":       call.ID,
		"name":     call.Name,
		"content":  result.Content,
		"is_error": result.IsError,
	})
	return err
}

// finish writes the closing event of a successful turn.
func (a *Agent) finish(out Outcome, reason string) error {
	_, err := a.Journal.Append(journal.KindTurnEnd, map[string]any{
		"reason":     reason,
		"steps":      out.Steps,
		"tool_calls": out.ToolCalls,
		"usage":      out.Usage,
	})
	return err
}

// fail journals the error and the turn's end, then returns the original error.
//
// A turn that failed is still a turn worth inspecting and replaying, so the
// journal is closed out properly rather than left truncated.
func (a *Agent) fail(out Outcome, stage string, cause error) error {
	if _, err := a.Journal.Append(journal.KindError, map[string]any{
		"stage": stage,
		"error": cause.Error(),
	}); err != nil {
		return err
	}
	if err := a.finish(out, "error"); err != nil {
		return err
	}
	return cause
}

func (a *Agent) check() error {
	switch {
	case a.Provider == nil:
		return errors.New("agent: no provider configured")
	case a.Tools == nil:
		return errors.New("agent: no tool registry configured")
	case a.Journal == nil:
		return errors.New("agent: no journal configured")
	case a.Model == "":
		return errors.New("agent: no model configured")
	}
	return nil
}
