// Package review runs a second, smaller turn after a conversation to decide
// what was worth learning from it.
//
// This is the half of self-learning nemuz borrows from Hermes Agent, which
// showed the mechanism works: fork the agent after a turn, hand it a tiny
// toolset, and ask it whether anything should be kept. What nemuz adds is on
// the other side — a skill the reviewer writes lands in quarantine and proves
// itself before it is ever given to a model.
//
// # The toolset is the safety property
//
// A reviewer can do exactly two things: remember a fact, and draft a skill. It
// cannot read files, run commands, or reach the network, because those tools
// are not in its registry at all. That is a stronger guarantee than a prompt
// asking it to behave, and it is checked by a test.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kansaok/nemuz/internal/memory"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/kansaok/nemuz/internal/tool"
)

// RememberTool lets the reviewer store a fact.
type RememberTool struct {
	Store *memory.Store
	// Turn is recorded as the memory's source, so a fact can be traced back
	// to the conversation that produced it.
	Turn string

	saved []string
}

// NewRememberTool returns the memory-writing tool for one review.
func NewRememberTool(store *memory.Store, turn string) *RememberTool {
	return &RememberTool{Store: store, Turn: turn}
}

func (t *RememberTool) Name() string { return "remember" }
func (t *RememberTool) Description() string {
	return "Store a durable fact about the user or their project. Use it for things that will still be true next week, not for details of the conversation that just happened."
}
func (t *RememberTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"text":{"type":"string","description":"The fact, in one or two sentences"},` +
		`"kind":{"type":"string","enum":["user","project","reference"],"description":"user: about the person. project: about the work. reference: a pointer to something external"},` +
		`"tags":{"type":"array","items":{"type":"string"},"description":"Optional labels to help recall it later"}` +
		`},"required":["text","kind"],"additionalProperties":false}`)
}

// Capabilities is empty: this tool writes to nemuz's own state directory and
// never touches the workspace, so there is nothing for the sandbox to grant.
func (t *RememberTool) Capabilities() tool.Capabilities { return tool.Capabilities{} }

// Saved returns the ids stored during this review.
func (t *RememberTool) Saved() []string { return t.saved }

func (t *RememberTool) Run(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var args struct {
		Text string   `json:"text"`
		Kind string   `json:"kind"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tool.Errorf("could not parse arguments: %v", err), nil
	}
	m, err := t.Store.Remember(args.Text, memory.Kind(args.Kind), t.Turn, args.Tags)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	t.saved = append(t.saved, m.ID)
	return tool.Result{Content: fmt.Sprintf("remembered as %s", m.ID)}, nil
}

// DraftSkillTool lets the reviewer write a skill.
//
// Everything it writes is quarantined. There is no argument, and no code path,
// that produces an active skill: promotion happens only through the eval gate,
// with a passing report. The reviewer is told this in the description, so it
// knows a draft is a proposal rather than a change.
type DraftSkillTool struct {
	Store *skill.Store
	Now   func() time.Time

	drafted []string
}

// NewDraftSkillTool returns the skill-writing tool for one review.
func NewDraftSkillTool(store *skill.Store) *DraftSkillTool {
	return &DraftSkillTool{Store: store, Now: time.Now}
}

func (t *DraftSkillTool) Name() string { return "draft_skill" }
func (t *DraftSkillTool) Description() string {
	return "Propose a reusable procedure learned from this turn. The draft is quarantined: it is stored for review and cannot be used until it passes eval scenarios. Only draft something you would expect to need again."
}
func (t *DraftSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string","description":"Lowercase words joined by hyphens, e.g. deploy-to-staging"},` +
		`"description":{"type":"string","description":"One line: when should this skill be used?"},` +
		`"body":{"type":"string","description":"The procedure, in Markdown"}` +
		`},"required":["name","description","body"],"additionalProperties":false}`)
}

// Capabilities is empty, for the same reason RememberTool's is.
func (t *DraftSkillTool) Capabilities() tool.Capabilities { return tool.Capabilities{} }

// Drafted returns the skill names written during this review.
func (t *DraftSkillTool) Drafted() []string { return t.drafted }

func (t *DraftSkillTool) Run(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tool.Errorf("could not parse arguments: %v", err), nil
	}

	name := strings.TrimSpace(args.Name)
	if existing, err := t.Store.Load(name); err == nil {
		// Overwriting an active skill from a background review would let a
		// promoted, proven procedure be replaced by an unproven one without
		// passing the gate.
		return tool.Errorf("a skill called %q already exists and is %s; propose a different name", name, existing.State), nil
	}

	now := t.Now
	if now == nil {
		now = time.Now
	}
	sk, err := skill.New(name, args.Description, args.Body, skill.ByAgent, now())
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	if err := t.Store.Save(sk); err != nil {
		return tool.Errorf("%v", err), nil
	}
	t.drafted = append(t.drafted, sk.Name)
	return tool.Result{Content: fmt.Sprintf(
		"drafted %s in quarantine; it needs eval scenarios before it can be used", sk.Name)}, nil
}

// Registry builds the reviewer's toolset: these two tools and nothing else.
func Registry(memories *memory.Store, skills *skill.Store, turn string) (*tool.Registry, *RememberTool, *DraftSkillTool, error) {
	remember := NewRememberTool(memories, turn)
	draft := NewDraftSkillTool(skills)

	reg := tool.NewRegistry()
	if err := reg.Register(remember, draft); err != nil {
		return nil, nil, nil, err
	}
	return reg, remember, draft, nil
}
