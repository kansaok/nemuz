package consolidate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kansaok/nemuz/internal/skill"
	"github.com/kansaok/nemuz/internal/tool"
)

// MergeTool lets the model propose replacing two skills with one.
//
// Like review's DraftSkillTool, this can only propose: what it writes goes
// nowhere until the caller decides to apply it, and even then the merged
// skill starts in quarantine like any agent-written skill. A skill that
// approved its own merger would be evidence of nothing.
type MergeTool struct {
	a, b *skill.Skill

	proposed bool
	name     string
	desc     string
	body     string
}

// NewMergeTool returns the merge tool scoped to one candidate pair.
func NewMergeTool(a, b *skill.Skill) *MergeTool { return &MergeTool{a: a, b: b} }

func (t *MergeTool) Name() string { return "merge_skills" }
func (t *MergeTool) Description() string {
	return fmt.Sprintf(
		"Propose replacing %q and %q with a single merged skill, because they teach the same thing. The merge is a proposal: it is quarantined and needs its own passing evidence before anyone can use it.",
		t.a.Name, t.b.Name)
}
func (t *MergeTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string","description":"Lowercase words joined by hyphens"},` +
		`"description":{"type":"string","description":"One line: when should this skill be used?"},` +
		`"body":{"type":"string","description":"The merged procedure, in Markdown, covering what both originals covered"}` +
		`},"required":["name","description","body"],"additionalProperties":false}`)
}
func (t *MergeTool) Capabilities() tool.Capabilities { return tool.Capabilities{} }

// Proposed reports whether a merge was proposed, and its contents.
func (t *MergeTool) Proposed() (name, description, body string, ok bool) {
	return t.name, t.desc, t.body, t.proposed
}

func (t *MergeTool) Run(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tool.Errorf("could not parse arguments: %v", err), nil
	}
	if args.Name == "" || args.Description == "" || args.Body == "" {
		return tool.Errorf("name, description and body are all required"), nil
	}
	t.name, t.desc, t.body, t.proposed = args.Name, args.Description, args.Body, true
	return tool.Result{Content: fmt.Sprintf("proposed merging into %q, pending review", args.Name)}, nil
}

// KeepSeparateTool lets the model explicitly decline to merge.
//
// Giving "no" its own tool, rather than reading a lack of merge_skills as
// silent decline, makes a considered "these are actually different" visible
// in the journal — the same reason review's reviewer has an explicit nothing-
// to-report path rather than just stopping.
type KeepSeparateTool struct {
	decided bool
	reason  string
}

func (t *KeepSeparateTool) Name() string { return "keep_separate" }
func (t *KeepSeparateTool) Description() string {
	return "Decide these two skills are not actually redundant and should both stay as they are."
}
func (t *KeepSeparateTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"reason":{"type":"string","description":"Why they are different enough to keep separate"}` +
		`},"required":["reason"],"additionalProperties":false}`)
}
func (t *KeepSeparateTool) Capabilities() tool.Capabilities { return tool.Capabilities{} }

// Decided reports whether this was the outcome, and why.
func (t *KeepSeparateTool) Decided() (reason string, ok bool) { return t.reason, t.decided }

func (t *KeepSeparateTool) Run(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	var args struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tool.Errorf("could not parse arguments: %v", err), nil
	}
	t.reason, t.decided = args.Reason, true
	return tool.Result{Content: "kept separate: " + args.Reason}, nil
}

// Registry builds the comparison's toolset: these two tools and nothing else.
func Registry(a, b *skill.Skill) (*tool.Registry, *MergeTool, *KeepSeparateTool, error) {
	merge := NewMergeTool(a, b)
	keep := &KeepSeparateTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(merge, keep); err != nil {
		return nil, nil, nil, err
	}
	return reg, merge, keep, nil
}
