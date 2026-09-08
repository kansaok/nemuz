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

// ---------- self-learning through the public API ----------

// TestReviewKeepsWhatItLearned exercises the loop a library user gets: run a
// turn, review it, and find the fact waiting for the next turn.
func TestReviewKeepsWhatItLearned(t *testing.T) {
	a := newAgent(t, []nemuz.Response{
		// The turn itself.
		{Text: "Deploy dijalankan lewat scripts/ship.sh.", StopReason: nemuz.StopEnd},
		// The review that follows.
		{
			StopReason: nemuz.StopToolUse,
			ToolCalls: []nemuz.ToolCall{{ID: "r1", Name: "remember", Args: args(t, map[string]any{
				"text": "Deploy dijalankan lewat scripts/ship.sh",
				"kind": "project",
				"tags": []string{"deploy"},
			})}},
		},
		{Text: "Saya simpan satu fakta.", StopReason: nemuz.StopEnd},
	})

	out, err := a.Run(context.Background(), "bagaimana cara deploy?")
	if err != nil {
		t.Fatal(err)
	}

	got, err := a.Review(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reviewed {
		t.Fatalf("the turn was skipped: %s", got.Skipped)
	}
	if len(got.Remembered) != 1 {
		t.Fatalf("remembered %v", got.Remembered)
	}
	if got.TurnID == "" {
		t.Error("the review left no turn of its own to inspect")
	}

	recalled, err := a.Recall("cara deploy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recalled) != 1 || !strings.Contains(recalled[0].Text, "ship.sh") {
		t.Fatalf("the fact is not recallable: %+v", recalled)
	}
}

// TestReviewedSkillsArriveQuarantined is the guarantee that matters most to
// someone embedding nemuz: a background review cannot widen what the agent does.
func TestReviewedSkillsArriveQuarantined(t *testing.T) {
	a := newAgent(t, []nemuz.Response{
		{Text: "selesai", StopReason: nemuz.StopEnd},
		{
			StopReason: nemuz.StopToolUse,
			ToolCalls: []nemuz.ToolCall{{ID: "r1", Name: "draft_skill", Args: args(t, map[string]any{
				"name":        "cara-deploy",
				"description": "Cara men-deploy proyek ini.",
				"body":        "Jalankan scripts/ship.sh.",
			})}},
		},
		{Text: "Saya usulkan satu skill.", StopReason: nemuz.StopEnd},
	})

	out, err := a.Run(context.Background(), "bagaimana cara deploy?")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Review(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Drafted) != 1 {
		t.Fatalf("drafted %v", got.Drafted)
	}

	skills, err := a.Skills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("skills are %+v", skills)
	}
	if skills[0].State != "quarantine" {
		t.Fatalf("a drafted skill is %q; a review must not be able to activate one", skills[0].State)
	}
	if skills[0].Scenarios != 0 {
		t.Errorf("a fresh draft has %d scenarios", skills[0].Scenarios)
	}
}

// TestCurateDemotesASkillThatStoppedWorking is the curator's point, reached
// entirely through the public API.
func TestCurateDemotesASkillThatStoppedWorking(t *testing.T) {
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

	// A skill that claims to be proven, with a scenario demanding a tool this
	// agent does not have. Curation should notice and send it back.
	writeSkill(t, a, "butuh-tool-hilang", "active", "plugin__hilang", out.TurnID)

	report, err := a.Curate(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed != 1 {
		t.Fatalf("curation changed %d skills: %+v", report.Changed, report.Actions)
	}
	if report.Actions[0].Action != "demoted" {
		t.Fatalf("action is %+v", report.Actions[0])
	}

	skills, err := a.Skills()
	if err != nil {
		t.Fatal(err)
	}
	if skills[0].State != "quarantine" {
		t.Fatalf("the broken skill is %q", skills[0].State)
	}
	if skills[0].PromotedBy != "" {
		t.Error("a demoted skill kept its promotion evidence")
	}
}

func TestCurateLeavesAWorkingSkillAlone(t *testing.T) {
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
	writeSkill(t, a, "masih-benar", "active", "greet", out.TurnID)

	report, err := a.Curate(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed != 0 {
		t.Fatalf("a working skill was changed: %+v", report.Actions)
	}
	if len(report.Actions) != 1 || report.Actions[0].Action != "kept" {
		t.Fatalf("actions are %+v", report.Actions)
	}
}

// writeSkill drops a skill and one scenario onto disk, the way a review would.
func writeSkill(t *testing.T, a *nemuz.Agent, name, state, mustCall, turnID string) {
	t.Helper()

	evalDir, err := a.EvalDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evalDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Copy the recording the scenario replays.
	journalFile := filepath.Join(a.Home(), "journal", turnID+".jsonl")
	body, err := os.ReadFile(journalFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evalDir, "turn.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	scenario := `{"name":"dasar","journal":"turn.jsonl","expect":{"must_call":["` + mustCall + `"]}}`
	if err := os.WriteFile(filepath.Join(evalDir, "dasar.json"), []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}

	skillFile := "---\n" +
		"name: " + name + "\n" +
		"description: Skill uji coba.\n" +
		"state: " + state + "\n" +
		"created_by: agent\n" +
		"created_at: 2026-09-08T00:00:00Z\n" +
		"last_used_at: 2026-09-08T00:00:00Z\n" +
		"use_count: 1\n" +
		"promoted_by: eval-lama\n" +
		"---\n\nLangkah pertama.\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(evalDir), "SKILL.md"), []byte(skillFile), 0o600); err != nil {
		t.Fatal(err)
	}
}
