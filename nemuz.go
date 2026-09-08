package nemuz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/kansaok/nemuz/internal/memory"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/kansaok/nemuz/internal/tool"
)

// Options configures an [Agent].
type Options struct {
	// Provider performs model calls. Required. Build one with [OpenProvider]
	// or supply your own.
	Provider Provider
	// Model names the model to request. Required.
	Model string

	// Home is where state lives — journal, memories, skills. It defaults to
	// NEMUZ_HOME, or ~/.nemuz.
	Home string
	// Workspace is the directory the built-in tools operate on. Defaults to
	// the current directory.
	Workspace string
	// System is the base system prompt. Skills and recalled memories are
	// appended to it.
	System string

	// Tools are added alongside the built-in ones.
	Tools []Tool
	// WithoutBuiltins leaves out read_file, write_file and list_dir, for an
	// agent whose tools are entirely your own.
	WithoutBuiltins bool
	// WithoutMemories skips recalling memories into the prompt.
	WithoutMemories bool
	// WithoutSkills skips adding active skills to the prompt.
	WithoutSkills bool

	// MaxSteps bounds the tool loop. Zero uses the default.
	MaxSteps int
}

// DefaultSystemPrompt is used when Options.System is empty.
const DefaultSystemPrompt = "You are a careful assistant. Use the tools to answer from the workspace."

// Agent runs turns, records them, and remembers what it learns.
//
// An Agent is safe for sequential use. Running turns concurrently on one Agent
// is not supported: turns share a workspace and a memory store, and interleaving
// them would make the journal a poor record of what happened.
type Agent struct {
	opts     Options
	paths    config.Paths
	blobs    *blob.Store
	memories *memory.Store
	skills   *skill.Store
	tools    *tool.Registry
	work     *tool.Workspace
}

// Open prepares an agent, creating its state directories if needed.
func Open(opts Options) (*Agent, error) {
	if opts.Provider == nil {
		return nil, errors.New("nemuz: Options.Provider is required")
	}
	if opts.Model == "" {
		return nil, errors.New("nemuz: Options.Model is required")
	}
	if opts.System == "" {
		opts.System = DefaultSystemPrompt
	}
	if opts.Workspace == "" {
		opts.Workspace = "."
	}

	paths := config.At(opts.Home)
	if opts.Home == "" {
		resolved, err := config.Resolve()
		if err != nil {
			return nil, err
		}
		paths = resolved
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}

	blobs, err := blob.Open(paths.Blobs)
	if err != nil {
		return nil, err
	}
	memories, err := memory.Open(paths.Memories)
	if err != nil {
		return nil, err
	}
	skills, err := skill.Open(paths.Skills)
	if err != nil {
		return nil, err
	}
	work, err := tool.NewWorkspace(opts.Workspace)
	if err != nil {
		return nil, err
	}

	tools := tool.NewRegistry()
	if !opts.WithoutBuiltins {
		if err := tools.Register(tool.NewReadFile(work), tool.NewWriteFile(work), tool.NewListDir(work)); err != nil {
			return nil, err
		}
	}
	if err := tools.Register(opts.Tools...); err != nil {
		return nil, err
	}

	return &Agent{
		opts: opts, paths: paths, blobs: blobs,
		memories: memories, skills: skills, tools: tools, work: work,
	}, nil
}

// Close releases the agent's resources. It is safe to call more than once.
func (a *Agent) Close() error { return nil }

// Home returns the state directory in use.
func (a *Agent) Home() string { return a.paths.Root }

// Workspace returns the resolved workspace directory.
func (a *Agent) Workspace() string { return a.work.Root() }

// ToolNames lists the tools this agent offers a model.
func (a *Agent) ToolNames() []string { return a.tools.Names() }

// Run executes one turn and records it.
//
// The returned Outcome carries the turn id, which [Agent.Replay] and
// [Agent.Events] accept.
func (a *Agent) Run(ctx context.Context, prompt string) (Outcome, error) {
	system, recalled, err := a.buildPrompt(prompt)
	if err != nil {
		return Outcome{}, err
	}

	turnID, err := journal.NewTurnID()
	if err != nil {
		return Outcome{}, err
	}
	w, err := journal.Create(a.paths.Journal, turnID, a.blobs)
	if err != nil {
		return Outcome{}, err
	}
	defer w.Close()

	env := map[string]string{}
	if len(recalled) > 0 {
		env["memories"] = joinIDs(recalled)
	}

	runner := &agent.Agent{
		Provider:    llm.Record(a.opts.Provider, w),
		Tools:       a.tools,
		Journal:     w,
		Model:       a.opts.Model,
		System:      system,
		MaxSteps:    a.opts.MaxSteps,
		Environment: env,
	}
	out, runErr := runner.Run(ctx, prompt)
	if err := w.Close(); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		return out, runErr
	}

	if len(recalled) > 0 {
		// Recall only improves if the store learns which memories were used.
		_ = a.memories.RecordUse(recalled...)
	}
	return out, nil
}

// Replay runs a recorded turn again against its recording and reports the first
// difference, if any.
//
// It makes no model calls and needs no network: the answers come from the
// recording. A turn that replays cleanly is reproducible; one that does not has
// found a real difference in the code, the tools, or the workspace.
func (a *Agent) Replay(ctx context.Context, turnID string) error {
	turn, err := journal.Find(a.paths.Journal, turnID)
	if err != nil {
		return err
	}
	recorded, err := journal.Read(turn.Path)
	if err != nil {
		return err
	}
	cassette, err := journal.CassetteFrom(recorded, a.blobs)
	if err != nil {
		return err
	}
	start, err := replayStart(recorded, a.blobs)
	if err != nil {
		return err
	}

	replayID, err := journal.NewTurnID()
	if err != nil {
		return err
	}
	w, err := journal.Create(a.paths.Journal, replayID, a.blobs)
	if err != nil {
		return err
	}
	defer w.Close()

	runner := &agent.Agent{
		Provider:    llm.Replay(cassette, w),
		Tools:       a.tools,
		Journal:     w,
		Model:       start.Model,
		System:      start.System,
		MaxSteps:    a.opts.MaxSteps,
		Environment: start.Env,
	}
	_, runErr := runner.Run(ctx, start.Prompt)
	if err := w.Close(); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("nemuz: turn %s is not reproducible: %w", turn.ID, runErr)
	}

	replayed, err := journal.Read(w.Path())
	if err != nil {
		return err
	}
	if err := journal.Verify(recorded, replayed); err != nil {
		return fmt.Errorf("nemuz: turn %s is not reproducible: %w", turn.ID, err)
	}
	return nil
}

// Turns lists the recorded turns, oldest first.
func (a *Agent) Turns() ([]TurnInfo, error) {
	turns, err := journal.List(a.paths.Journal)
	if err != nil {
		return nil, err
	}
	out := make([]TurnInfo, 0, len(turns))
	for _, t := range turns {
		events, err := journal.Read(t.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, TurnInfo{ID: t.ID, Events: len(events), Digest: journal.Digest(events)})
	}
	return out, nil
}

// Events returns one turn's events, so a caller can inspect exactly what
// happened without parsing the journal themselves.
func (a *Agent) Events(turnID string) ([]Event, error) {
	turn, err := journal.Find(a.paths.Journal, turnID)
	if err != nil {
		return nil, err
	}
	return journal.Read(turn.Path)
}

// Remember stores a fact for later recall. Kind is "user", "project" or
// "reference".
func (a *Agent) Remember(text, kind string, tags ...string) (Fact, error) {
	m, err := a.memories.Remember(text, memory.Kind(kind), "", tags)
	if err != nil {
		return Fact{}, err
	}
	return toFact(m), nil
}

// Recall returns the memories a prompt would pull in, in the order it would
// pull them. It is the same call [Agent.Run] makes.
func (a *Agent) Recall(query string, limit int) ([]Fact, error) {
	found, err := a.memories.Recall(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(found))
	for _, m := range found {
		out = append(out, toFact(m))
	}
	return out, nil
}

// Forget deletes a memory.
func (a *Agent) Forget(id string) error { return a.memories.Forget(id) }

// Skills lists every skill and its state.
func (a *Agent) Skills() ([]SkillInfo, error) {
	all, err := a.skills.List()
	if err != nil {
		return nil, err
	}
	out := make([]SkillInfo, 0, len(all))
	for _, sk := range all {
		scenarios, err := a.skills.Scenarios(sk.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, SkillInfo{
			Name: sk.Name, Description: sk.Description,
			State: string(sk.State), WrittenBy: string(sk.CreatedBy),
			Uses: sk.UseCount, Scenarios: len(scenarios), PromotedBy: sk.PromotedBy,
		})
	}
	return out, nil
}

// buildPrompt assembles the system prompt for a turn.
func (a *Agent) buildPrompt(prompt string) (string, []string, error) {
	system := a.opts.System

	if !a.opts.WithoutSkills {
		active, err := a.skills.Active()
		if err != nil {
			return "", nil, err
		}
		if len(active) > 0 {
			system += "\n\n# Learned skills\n"
			for _, sk := range active {
				system += "\n" + sk.Prompt()
			}
		}
	}

	var recalled []string
	if !a.opts.WithoutMemories {
		found, err := a.memories.Recall(prompt, memory.DefaultRecallLimit)
		if err != nil {
			return "", nil, err
		}
		if len(found) > 0 {
			system += "\n\n" + memory.Prompt(found)
			for _, m := range found {
				recalled = append(recalled, m.ID)
			}
		}
	}
	return system, recalled, nil
}

// ProviderSpec selects and configures a built-in provider adapter.
type ProviderSpec struct {
	// Provider is a name from [ProviderNames].
	Provider string
	// Model overrides the provider's default model.
	Model string
	// BaseURL overrides the endpoint, for proxies and self-hosted servers.
	BaseURL string
	// APIKey overrides the environment variable. Prefer the environment.
	APIKey string
}

// OpenProvider builds one of the built-in adapters, reading its API key from the
// environment unless one is given.
func OpenProvider(spec ProviderSpec) (Provider, error) {
	return provider.Open(provider.Spec{
		Provider: spec.Provider, Model: spec.Model,
		BaseURL: spec.BaseURL, APIKey: spec.APIKey,
	})
}

// ProviderNames lists the providers [OpenProvider] understands.
//
// Most of them share one adapter, because most services speak the OpenAI shape.
// Anything else that does can be reached by setting ProviderSpec.BaseURL.
func ProviderNames() []string { return provider.Names() }

type replayTurnStart struct {
	Prompt string            `json:"prompt"`
	Model  string            `json:"model"`
	System string            `json:"system"`
	Env    map[string]string `json:"env,omitempty"`
}

func replayStart(events []Event, bs *blob.Store) (replayTurnStart, error) {
	for _, e := range events {
		if e.Kind != journal.KindTurnStart {
			continue
		}
		body, err := e.Content(bs)
		if err != nil {
			return replayTurnStart{}, err
		}
		var start replayTurnStart
		if err := json.Unmarshal(body, &start); err != nil {
			return replayTurnStart{}, err
		}
		if start.Prompt == "" {
			return replayTurnStart{}, errors.New("nemuz: the recording has no prompt")
		}
		return start, nil
	}
	return replayTurnStart{}, errors.New("nemuz: the recording has no turn.start event")
}

func toFact(m *memory.Memory) Fact {
	return Fact{
		ID: m.ID, Text: m.Text, Kind: string(m.Kind),
		Tags: m.Tags, Turn: m.Source, Uses: m.UseCount, Pinned: m.Pinned,
	}
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

// EvalDir returns where a skill's eval scenarios live, for callers that write
// scenarios themselves.
func (a *Agent) EvalDir(skillName string) (string, error) {
	dir, err := a.skills.EvalDir(skillName)
	if err != nil {
		return "", err
	}
	return filepath.Clean(dir), nil
}
