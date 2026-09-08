package nemuz_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kansaok/nemuz"
)

// These tests exercise nemuz the way a user of the package would: only through
// the exported API, from an external test package. If something here needs an
// internal import, the public API is missing something.

// scripted is a Provider that returns a fixed sequence, so the whole suite runs
// with no API key and no network.
type scripted struct {
	replies []nemuz.Response
	at      int
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Complete(_ context.Context, _ nemuz.Request) (nemuz.Response, error) {
	if s.at >= len(s.replies) {
		return nemuz.Response{}, errNoMoreReplies
	}
	r := s.replies[s.at]
	s.at++
	return r, nil
}

var errNoMoreReplies = errorString("scripted provider ran out of replies")

type errorString string

func (e errorString) Error() string { return string(e) }

// greet is a third-party tool, defined entirely against the public API.
type greet struct{ calls int }

func (g *greet) Name() string        { return "greet" }
func (g *greet) Description() string { return "Greet someone by name." }
func (g *greet) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
}

// A pure tool needs nothing from the machine, which is the set most tools
// should be able to declare.
func (g *greet) Capabilities() nemuz.Capabilities { return nemuz.Capabilities{} }

func (g *greet) Run(_ context.Context, raw json.RawMessage) (nemuz.Result, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nemuz.Errorf("bad arguments: %v", err), nil
	}
	if args.Name == "" {
		return nemuz.Errorf("name is required"), nil
	}
	g.calls++
	return nemuz.Result{Content: "halo, " + args.Name}, nil
}

func newAgent(t *testing.T, replies []nemuz.Response, tools ...nemuz.Tool) *nemuz.Agent {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# proyek\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := nemuz.Open(nemuz.Options{
		Provider:  &scripted{replies: replies},
		Model:     "test-model",
		Home:      filepath.Join(dir, "state"),
		Workspace: workspace,
		Tools:     tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunAnswersAndRecordsTheTurn(t *testing.T) {
	a := newAgent(t, []nemuz.Response{
		{Text: "Proyek ini punya README.", StopReason: nemuz.StopEnd, Usage: nemuz.Usage{InputTokens: 20, OutputTokens: 5}},
	})

	out, err := a.Run(context.Background(), "apa isi workspace ini?")
	if err != nil {
		t.Fatal(err)
	}
	if out.Text == "" || out.TurnID == "" {
		t.Fatalf("outcome is %+v", out)
	}
	if out.Usage.InputTokens != 20 {
		t.Errorf("usage is %+v", out.Usage)
	}

	turns, err := a.Turns()
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].ID != out.TurnID {
		t.Fatalf("turns are %+v", turns)
	}
	if turns[0].Digest == "" {
		t.Error("the turn has no digest")
	}

	events, err := a.Events(out.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Kind != nemuz.EventTurnStart {
		t.Errorf("the first event is %s", events[0].Kind)
	}
}

// TestThirdPartyToolsWork is what this package exists for: building an agent
// with tools the framework has never heard of.
func TestThirdPartyToolsWork(t *testing.T) {
	g := &greet{}
	a := newAgent(t, []nemuz.Response{
		{
			StopReason: nemuz.StopToolUse,
			ToolCalls:  []nemuz.ToolCall{{ID: "c1", Name: "greet", Args: args(t, map[string]string{"name": "dunia"})}},
		},
		{Text: "Sudah saya sapa.", StopReason: nemuz.StopEnd},
	}, g)

	if !contains(a.ToolNames(), "greet") {
		t.Fatalf("the tool was not registered: %v", a.ToolNames())
	}

	out, err := a.Run(context.Background(), "sapa dunia")
	if err != nil {
		t.Fatal(err)
	}
	if g.calls != 1 {
		t.Fatalf("the tool ran %d times", g.calls)
	}
	if out.ToolCalls != 1 {
		t.Errorf("the turn reports %d tool calls", out.ToolCalls)
	}
}

// TestReplayNeedsNoProvider is the framework's central promise, stated through
// the public API.
func TestReplayNeedsNoProvider(t *testing.T) {
	g := &greet{}
	a := newAgent(t, []nemuz.Response{
		{
			StopReason: nemuz.StopToolUse,
			ToolCalls:  []nemuz.ToolCall{{ID: "c1", Name: "greet", Args: args(t, map[string]string{"name": "dunia"})}},
		},
		{Text: "Sudah saya sapa.", StopReason: nemuz.StopEnd},
	}, g)

	out, err := a.Run(context.Background(), "sapa dunia")
	if err != nil {
		t.Fatal(err)
	}
	// The scripted provider is now exhausted: any further model call fails.
	// Replay must therefore be drawing every answer from the recording.
	if err := a.Replay(context.Background(), out.TurnID); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if g.calls != 2 {
		t.Errorf("the tool ran %d times; replay should have run it again for real", g.calls)
	}
}

// TestReplayCatchesAChangedTool proves replay is a real check and not a
// formality.
func TestReplayCatchesAChangedTool(t *testing.T) {
	a := newAgent(t, []nemuz.Response{
		{
			StopReason: nemuz.StopToolUse,
			ToolCalls:  []nemuz.ToolCall{{ID: "c1", Name: "greet", Args: args(t, map[string]string{"name": "dunia"})}},
		},
		{Text: "selesai", StopReason: nemuz.StopEnd},
	}, &greet{})

	out, err := a.Run(context.Background(), "sapa dunia")
	if err != nil {
		t.Fatal(err)
	}

	// A second agent over the same state, whose greet tool answers differently.
	changed, err := nemuz.Open(nemuz.Options{
		Provider:  &scripted{},
		Model:     "test-model",
		Home:      a.Home(),
		Workspace: a.Workspace(),
		Tools:     []nemuz.Tool{&rudeGreet{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()

	err = changed.Replay(context.Background(), out.TurnID)
	if err == nil {
		t.Fatal("replay passed even though the tool now answers differently")
	}
	if !strings.Contains(err.Error(), "not reproducible") {
		t.Errorf("error is %v", err)
	}
}

type rudeGreet struct{ greet }

func (r *rudeGreet) Run(context.Context, json.RawMessage) (nemuz.Result, error) {
	return nemuz.Result{Content: "pergi sana"}, nil
}

func TestMemoriesRoundTripAndRecall(t *testing.T) {
	a := newAgent(t, nil)

	fact, err := a.Remember("Skrip deploy ada di scripts/ship.sh", "project", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if fact.ID == "" || fact.Kind != "project" {
		t.Fatalf("fact is %+v", fact)
	}

	got, err := a.Recall("bagaimana cara deploy?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != fact.ID {
		t.Fatalf("recall returned %+v", got)
	}

	if err := a.Forget(fact.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = a.Recall("bagaimana cara deploy?", 5)
	if len(got) != 0 {
		t.Fatalf("a forgotten memory came back: %+v", got)
	}
}

func TestRecalledMemoriesReachThePrompt(t *testing.T) {
	a := newAgent(t, []nemuz.Response{{Text: "ok", StopReason: nemuz.StopEnd}})
	if _, err := a.Remember("Basis data pakai postgres di port 5433", "project", "database"); err != nil {
		t.Fatal(err)
	}

	out, err := a.Run(context.Background(), "port berapa untuk postgres?")
	if err != nil {
		t.Fatal(err)
	}
	events, err := a.Events(out.TurnID)
	if err != nil {
		t.Fatal(err)
	}

	var sawMemory bool
	for _, e := range events {
		if e.Kind != nemuz.EventModelRequest {
			continue
		}
		if strings.Contains(string(e.Payload), "5433") {
			sawMemory = true
		}
	}
	if !sawMemory {
		t.Error("the recalled memory never reached the model request")
	}
}

func TestOpenRejectsIncompleteOptions(t *testing.T) {
	for name, opts := range map[string]nemuz.Options{
		"no provider": {Model: "m", Home: t.TempDir()},
		"no model":    {Provider: &scripted{}, Home: t.TempDir()},
	} {
		if _, err := nemuz.Open(opts); err == nil {
			t.Errorf("%s: an incomplete agent was opened", name)
		}
	}
}

func TestWithoutBuiltinsLeavesOnlyYourTools(t *testing.T) {
	dir := t.TempDir()
	a, err := nemuz.Open(nemuz.Options{
		Provider:        &scripted{},
		Model:           "m",
		Home:            filepath.Join(dir, "state"),
		Workspace:       dir,
		Tools:           []nemuz.Tool{&greet{}},
		WithoutBuiltins: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	names := a.ToolNames()
	if len(names) != 1 || names[0] != "greet" {
		t.Fatalf("tools are %v", names)
	}
}

func TestProviderNamesCoverTheBuiltInAdapters(t *testing.T) {
	names := nemuz.ProviderNames()
	for _, want := range []string{"anthropic", "gemini", "openai", "ollama"} {
		if !contains(names, want) {
			t.Errorf("%s is missing from %v", want, names)
		}
	}
	if _, err := nemuz.OpenProvider(nemuz.ProviderSpec{Provider: "tidak-ada"}); err == nil {
		t.Error("an unknown provider was opened")
	}
}

func TestSkillsAreVisibleAndStartEmpty(t *testing.T) {
	a := newAgent(t, nil)
	skills, err := a.Skills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 0 {
		t.Fatalf("a fresh agent has skills: %+v", skills)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
