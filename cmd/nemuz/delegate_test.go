package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/kansaok/nemuz/internal/llm"
	"github.com/spf13/cobra"
)

func TestDelegateToolRefusesWhenDepthExhausted(t *testing.T) {
	sess := newTestSession(t, nil) // no scripted responses: exhaustion must be caught before any model call
	d := &delegateTool{session: sess, cmd: nil}

	ctx := withDelegateDepth(context.Background(), 0)
	result, err := d.Run(ctx, json.RawMessage(`{"task":"anything"}`))
	if err != nil {
		t.Fatalf("Run returned a Go error rather than a tool result: %v", err)
	}
	if !result.IsError {
		t.Fatal("delegating past the depth limit was not reported as an error to the model")
	}
	if !strings.Contains(result.Content, "depth") {
		t.Errorf("the refusal should explain why, got: %q", result.Content)
	}
}

func TestDelegateToolRejectsAnEmptyTask(t *testing.T) {
	sess := newTestSession(t, nil)
	d := &delegateTool{session: sess, cmd: nil}

	ctx := withDelegateDepth(context.Background(), DefaultDelegateDepth)
	result, err := d.Run(ctx, json.RawMessage(`{"task":"   "}`))
	if err != nil {
		t.Fatalf("Run returned a Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("an empty task was accepted")
	}
}

// TestDelegateRunsANestedTurnAndReturnsItsAnswer exercises the whole path: the
// parent agent asks for delegate, the sub-agent runs its own complete turn
// against the same scripted provider, and its answer comes back as the tool
// result the parent's next model call sees.
func TestDelegateRunsANestedTurnAndReturnsItsAnswer(t *testing.T) {
	sess := newTestSession(t, []llm.Response{
		{
			ToolCalls:  []llm.ToolCall{{ID: "1", Name: "delegate", Args: json.RawMessage(`{"task":"hitung 2+2"}`)}},
			StopReason: llm.StopToolUse,
		},
		{Text: "4", StopReason: llm.StopEnd}, // the sub-agent's own turn
		{Text: "jawabannya adalah 4", StopReason: llm.StopEnd},
	})
	if err := registerDelegateTool(sess, nil, io.Discard, DefaultDelegateDepth); err != nil {
		t.Fatal(err)
	}

	agentUnderTest := sess.provider.(*llm.Static)
	ctx := withDelegateDepth(context.Background(), DefaultDelegateDepth)
	outcome, _, err := sess.runTurn(ctx, &cobra.Command{}, io.Discard, "berapa 2+2?", false)
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	if !strings.Contains(outcome.Text, "4") {
		t.Errorf("the parent's final answer does not incorporate the sub-agent's result: %q", outcome.Text)
	}
	if outcome.ToolCalls != 1 {
		t.Errorf("tool calls = %d, want 1 (the delegate call itself)", outcome.ToolCalls)
	}
	if agentUnderTest.Calls() != 3 {
		t.Errorf("model was called %d times, want exactly 3 (parent, sub-agent, parent again)", agentUnderTest.Calls())
	}
}

// TestDelegateDepthIsEnforcedAcrossNesting proves the budget is spent per call
// chain, not per process: a depth of 1 lets the top turn delegate once, but
// the sub-agent it spawns must be refused when it tries to delegate again.
func TestDelegateDepthIsEnforcedAcrossNesting(t *testing.T) {
	sess := newTestSession(t, []llm.Response{
		{
			ToolCalls:  []llm.ToolCall{{ID: "1", Name: "delegate", Args: json.RawMessage(`{"task":"level one"}`)}},
			StopReason: llm.StopToolUse,
		},
		{ // the sub-agent, now at depth 0, tries to delegate again
			ToolCalls:  []llm.ToolCall{{ID: "1", Name: "delegate", Args: json.RawMessage(`{"task":"level two"}`)}},
			StopReason: llm.StopToolUse,
		},
		{Text: "gave up delegating, answered directly", StopReason: llm.StopEnd}, // sub-agent's real answer
		{Text: "done", StopReason: llm.StopEnd},                                  // parent's final answer
	})
	if err := registerDelegateTool(sess, nil, io.Discard, 1); err != nil {
		t.Fatal(err)
	}

	ctx := withDelegateDepth(context.Background(), 1)
	outcome, _, err := sess.runTurn(ctx, &cobra.Command{}, io.Discard, "top-level task", false)
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if outcome.Text != "done" {
		t.Errorf("outcome.Text = %q", outcome.Text)
	}

	oa := sess.provider.(*llm.Static)
	if oa.Calls() != 4 {
		t.Errorf("model was called %d times, want exactly 4", oa.Calls())
	}
}
