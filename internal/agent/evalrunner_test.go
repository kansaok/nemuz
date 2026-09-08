package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/skill"
)

// This file joins the two halves of nemuz's argument: turns are recorded, and
// recordings are what a learned skill is proved against. Nothing here touches a
// network, and nothing spends an API call.

func recordScenarioTurn(t *testing.T, f *fixture, id string, responses []llm.Response) string {
	t.Helper()
	w := f.writer(t, id)
	a := f.agent(w, llm.Record(&llm.Static{Responses: responses}, w))
	if _, err := a.Run(context.Background(), prompt); err != nil {
		t.Fatalf("recording the scenario turn failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.Path()
}

// newGate wires a real ScenarioRunner to a skill store.
func newGate(t *testing.T, f *fixture) (*skill.Store, *skill.Gate) {
	t.Helper()
	store, err := skill.Open(filepath.Join(f.dir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetClock(func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) })

	runner := &ScenarioRunner{
		Tools:      f.tools,
		Blobs:      f.blobs,
		JournalDir: filepath.Join(f.dir, "eval-journals"),
		BaseSystem: "Kamu asisten yang ringkas.",
	}
	return store, &skill.Gate{Store: store, Run: runner.Run, NewID: func() string { return "run-1" }}
}

// installSkill writes a quarantined skill with one scenario pointing at a
// recorded turn, the way the agent's own review would.
func installSkill(t *testing.T, store *skill.Store, name string, sc skill.Scenario, journalPath string) {
	t.Helper()
	sk, err := skill.New(name, "Cara "+name, "Baca dulu sebelum menjawab.", skill.ByAgent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(sk); err != nil {
		t.Fatal(err)
	}

	evalDir, err := store.EvalDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The recording is copied in beside the scenario, so a skill and its
	// evidence travel together.
	body, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evalDir, "turn.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sc.Journal = "turn.jsonl"
	if err := store.SaveScenario(name, sc); err != nil {
		t.Fatal(err)
	}
}

// TestSkillIsProvedAgainstARecordedTurn is the milestone: a skill the agent
// wrote for itself becomes active only after replaying a real turn, with real
// tools, and behaving the way the scenario demands.
func TestSkillIsProvedAgainstARecordedTurn(t *testing.T) {
	f := newFixture(t)
	journalPath := recordScenarioTurn(t, f, "01SCENARIO0000000000000AA", script(t))

	store, gate := newGate(t, f)
	installSkill(t, store, "ringkas-workspace", skill.Scenario{
		Name:        "membaca-readme",
		Description: "Skill ini harus membuat agen membaca README sebelum menjawab.",
		Expect: skill.Expectations{
			MustCall:    []string{"list_dir", "read_file"},
			MustNotCall: []string{"write_file"},
			MustContain: []string{"README"},
			MaxSteps:    4,
			MustSucceed: true,
		},
	}, journalPath)

	before, _ := store.Active()
	if len(before) != 0 {
		t.Fatal("a freshly written skill was already active")
	}

	report, err := gate.Certify(context.Background(), "ringkas-workspace")
	if err != nil {
		t.Fatalf("the skill failed its own scenario: %v\n%s", err, report.Summary())
	}
	if !report.Passed() {
		t.Fatalf("report says: %s", report.Summary())
	}

	active, err := store.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Name != "ringkas-workspace" {
		t.Fatalf("active skills are %v", active)
	}
	if active[0].PromotedBy != "run-1" {
		t.Errorf("the skill carries no link to its evidence")
	}
	if report.Results[0].TurnID == "" {
		t.Error("the report does not name the turn it produced, so the evidence cannot be inspected")
	}
}

// TestGateCatchesASkillThatBreaksBehaviour is the case that justifies the whole
// mechanism: the skill runs, produces an answer, and is still refused because
// it made the agent do the wrong thing.
func TestGateCatchesASkillThatBreaksBehaviour(t *testing.T) {
	f := newFixture(t)

	// A recording in which the agent writes a file instead of reading one.
	writing := []llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{{
				ID: "c1", Name: "write_file",
				Args: args(t, map[string]string{"path": "catatan.txt", "content": "sesuatu"}),
			}},
		},
		{Text: "Sudah saya tulis.", StopReason: llm.StopEnd},
	}
	journalPath := recordScenarioTurn(t, f, "01MENULIS00000000000000AA", writing)

	store, gate := newGate(t, f)
	installSkill(t, store, "hanya-baca", skill.Scenario{
		Name:        "jangan-menulis",
		Description: "Skill ini tidak boleh membuat agen menulis apa pun.",
		Expect:      skill.Expectations{MustNotCall: []string{"write_file"}, MustSucceed: true},
	}, journalPath)

	report, err := gate.Certify(context.Background(), "hanya-baca")
	if err == nil {
		t.Fatal("a skill that made the agent write was certified")
	}
	sk, _ := store.Load("hanya-baca")
	if sk.State != skill.StateQuarantine {
		t.Fatalf("the failing skill is %s, want quarantine", sk.State)
	}
	failures := strings.Join(report.Results[0].Failures, " | ")
	if !strings.Contains(failures, "write_file") {
		t.Errorf("the report does not name the offending tool: %s", failures)
	}
}

// TestEvaluationSpendsNoModelCalls states the property that makes the gate
// affordable enough to be mandatory.
func TestEvaluationSpendsNoModelCalls(t *testing.T) {
	f := newFixture(t)
	provider := &llm.Static{Responses: script(t)}

	w := f.writer(t, "01BIAYA0000000000000000AA")
	if _, err := f.agent(w, llm.Record(provider, w)).Run(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}
	w.Close()
	callsWhileRecording := provider.Calls()

	store, gate := newGate(t, f)
	installSkill(t, store, "gratis", skill.Scenario{
		Name:   "dasar",
		Expect: skill.Expectations{MustSucceed: true},
	}, w.Path())

	for i := 0; i < 5; i++ {
		if _, err := gate.Evaluate(context.Background(), "gratis"); err != nil {
			t.Fatal(err)
		}
	}
	if provider.Calls() != callsWhileRecording {
		t.Fatalf("evaluation called the provider %d extra times; recordings should have supplied every answer",
			provider.Calls()-callsWhileRecording)
	}
}

// TestSkillReachesTheModelOnlyOnceActive checks the gate is not merely
// bookkeeping: a quarantined skill's text must not appear in a prompt.
func TestSkillReachesTheModelOnlyOnceActive(t *testing.T) {
	f := newFixture(t)
	journalPath := recordScenarioTurn(t, f, "01PROMPT000000000000000AA", script(t))
	store, gate := newGate(t, f)
	installSkill(t, store, "penanda", skill.Scenario{
		Name:   "dasar",
		Expect: skill.Expectations{MustSucceed: true},
	}, journalPath)

	// While quarantined, Active must not offer it.
	active, _ := store.Active()
	if len(active) != 0 {
		t.Fatal("a quarantined skill appeared in the active set")
	}

	if _, err := gate.Certify(context.Background(), "penanda"); err != nil {
		t.Fatal(err)
	}
	active, _ = store.Active()
	if len(active) != 1 {
		t.Fatal("a certified skill did not appear in the active set")
	}

	// And the rendered prompt must actually carry the instructions.
	rendered := systemWithSkill("dasar", active[0])
	if !strings.Contains(rendered, "Baca dulu sebelum menjawab.") {
		t.Errorf("the skill's instructions are missing from the prompt:\n%s", rendered)
	}
	if !strings.HasPrefix(rendered, "dasar") {
		t.Error("the base system prompt was dropped")
	}
}

func TestToolOutcomesSeparateCallsFromFailures(t *testing.T) {
	events := []journal.Event{
		{Kind: journal.KindToolCall, Payload: json.RawMessage(`{"name":"read_file"}`)},
		{Kind: journal.KindToolResult, Payload: json.RawMessage(`{"name":"read_file","is_error":false}`)},
		{Kind: journal.KindToolCall, Payload: json.RawMessage(`{"name":"read_file"}`)},
		{Kind: journal.KindToolCall, Payload: json.RawMessage(`{"name":"missing_tool"}`)},
		{Kind: journal.KindToolResult, Payload: json.RawMessage(`{"name":"missing_tool","is_error":true}`)},
		{Kind: journal.KindTurnEnd, Payload: json.RawMessage(`{}`)},
	}
	called, failed := toolOutcomes(events)
	if len(called) != 2 || called[0] != "read_file" || called[1] != "missing_tool" {
		t.Fatalf("called is %v, want the distinct tools in call order", called)
	}
	if len(failed) != 1 || failed[0] != "missing_tool" {
		t.Fatalf("failed is %v, want just the tool whose result errored", failed)
	}
}

// TestScenarioFailsWhenARequiredToolIsMissing is the case a live run caught:
// a skill certified against a toolset that did not include the tool it depends
// on. The call is journaled, so must_call looked satisfied, while the tool had
// in fact never run.
func TestScenarioFailsWhenARequiredToolIsMissing(t *testing.T) {
	f := newFixture(t)
	calling := []llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "plugin__absent", Args: args(t, map[string]string{})}},
		},
		{Text: "Selesai.", StopReason: llm.StopEnd},
	}
	journalPath := recordScenarioTurn(t, f, "01HILANG000000000000000AA", calling)

	store, gate := newGate(t, f)
	installSkill(t, store, "butuh-plugin", skill.Scenario{
		Name:   "memakai-plugin",
		Expect: skill.Expectations{MustCall: []string{"plugin__absent"}, MustSucceed: true},
	}, journalPath)

	report, err := gate.Certify(context.Background(), "butuh-plugin")
	if err == nil {
		t.Fatal("a skill was certified against a toolset missing the tool it needs")
	}
	failures := strings.Join(report.Results[0].Failures, " | ")
	if !strings.Contains(failures, "registered in this environment") {
		t.Errorf("the report should point at the missing tool, got: %s", failures)
	}
}
