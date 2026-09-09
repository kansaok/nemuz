package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kansaok/nemuz/internal/tool"
	"github.com/spf13/cobra"
)

// DefaultDelegateDepth bounds how many levels deep an agent may hand a
// sub-task to another agent turn before delegating further is refused. Zero
// disables delegation entirely — no `delegate` tool is offered at all.
const DefaultDelegateDepth = 2

// delegateDepthKey is unexported so only withDelegateDepth and
// delegateDepthFrom in this file can read or write it.
type delegateDepthKey struct{}

// withDelegateDepth records how many further levels of delegation the turn
// running under ctx may still grant. Seeded once per top-level turn; each
// nested delegate call derives a child context one level lower, so the limit
// is enforced per call chain rather than per process.
func withDelegateDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, delegateDepthKey{}, depth)
}

// delegateDepthFrom reads the remaining depth, defaulting to 0 — no context
// ever produced outside this file, meaning delegation was never enabled.
func delegateDepthFrom(ctx context.Context) int {
	if n, ok := ctx.Value(delegateDepthKey{}).(int); ok {
		return n
	}
	return 0
}

// delegateTool lets an agent hand a bounded sub-task to another agent turn
// running the exact same model, toolset, and sandbox — and get back only its
// answer, not its whole step-by-step transcript. It exists so a coordinating
// agent can break a large task into independent pieces without trying to
// hold the whole plan in one context window, the same reason an orchestrating
// agent needs a subagent tool of its own.
//
// It is registered directly into the shared toolset, so its depth budget
// cannot live on the tool or the session — both are shared across every turn
// in a `nemuz chat` process — and must instead travel on ctx, decremented
// fresh for each nested call.
type delegateTool struct {
	session *turnSession
	cmd     *cobra.Command
	out     io.Writer
}

// newDelegateTool registers "delegate" into ts.Registry, unless depth is zero.
func registerDelegateTool(sess *turnSession, cmd *cobra.Command, out io.Writer, depth int) error {
	if depth <= 0 {
		return nil
	}
	return sess.ts.Registry.Register(&delegateTool{session: sess, cmd: cmd, out: out})
}

type delegateArgs struct {
	Task string `json:"task"`
}

func (d *delegateTool) Name() string { return "delegate" }

func (d *delegateTool) Description() string {
	return "Hand a self-contained sub-task to another agent turn, with the same tools " +
		"and workspace, and get back its final answer. The sub-agent has no memory of " +
		"this conversation beyond what `task` tells it, so include everything it needs. " +
		"Use this to split a large task into independent pieces rather than doing " +
		"everything in one long turn. Delegation nests only a bounded number of levels " +
		"deep; once exhausted, this tool reports that instead of running."
}

func (d *delegateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task": {
				"type": "string",
				"description": "A self-contained description of the sub-task, including any context the sub-agent needs — it starts with no memory of this conversation."
			}
		},
		"required": ["task"]
	}`)
}

// Capabilities is zero: delegate itself touches nothing. Whatever the
// sub-agent's own tool calls need is already covered by the shared registry's
// existing grants, since it is the exact same registry.
func (d *delegateTool) Capabilities() tool.Capabilities { return tool.Capabilities{} }

func (d *delegateTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var a delegateArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Errorf("delegate: invalid arguments: %v", err), nil
	}
	task := strings.TrimSpace(a.Task)
	if task == "" {
		return tool.Errorf("delegate: task must not be empty"), nil
	}

	depth := delegateDepthFrom(ctx)
	if depth <= 0 {
		return tool.Errorf("delegate: maximum delegation depth reached; finish this task directly instead of delegating further"), nil
	}

	// A shallow copy: the sub-agent shares the provider, toolset, and paths,
	// but never reviews or curates on its own — those judge whether a top-level
	// interaction was worth remembering, and a delegated sub-task answering to
	// another agent rather than the operator is not that.
	sub := *d.session
	sub.doReview = false
	sub.doCurate = false
	sub.baseSystem = d.session.baseSystem + "\n\nYou are handling a sub-task delegated by " +
		"another agent, not talking to the operator directly. Answer the task " +
		"completely and concisely; your reply is returned as a tool result, not shown as chat."

	outcome, turnID, err := sub.runTurn(withDelegateDepth(ctx, depth-1), d.cmd, d.out, task, false)
	if err != nil {
		return tool.Errorf("delegate: sub-task failed: %v", err), nil
	}
	return tool.Result{Content: fmt.Sprintf("%s\n\n(sub-agent turn: %s)", outcome.Text, turnID)}, nil
}
