package consolidate

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/review"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/kansaok/nemuz/internal/tool"
)

// These tests exercise the whole path a consolidation run takes: candidate
// pairs found locally, a scripted model asked about each, and — when it
// merges — a real skill.Gate proving the carried-over scenarios still pass.
// Nothing here touches a network; the model's answers come from llm.Static,
// the same seam agent and review's own tests use.

func fixedNow() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }

type fixture struct {
	dir    string
	skills *skill.Store
	blobs  *blob.Store
	tools  *tool.Registry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()

	sk, err := skill.Open(filepath.Join(dir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	sk.SetClock(fixedNow)

	bs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := tool.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	if err := tools.Register(tool.NewReadFile(ws), tool.NewListDir(ws)); err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, skills: sk, blobs: bs, tools: tools}
}

// recordTurn records one real turn — a scripted model, real tools, a real
// journal — so it can serve as evidence for a scenario.
func (f *fixture) recordTurn(t *testing.T, id, prompt string, responses []llm.Response) string {
	t.Helper()
	w, err := journal.Create(filepath.Join(f.dir, "journal"), id, f.blobs)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	a := &agent.Agent{
		Provider: llm.Record(&llm.Static{Responses: responses}, w),
		Tools:    f.tools,
		Journal:  w,
		Model:    "test-model",
	}
	if _, err := a.Run(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.Path()
}

// activeSkillWithScenario installs a proven skill: active, with one scenario
// that actually passes against a real recorded turn.
func (f *fixture) activeSkillWithScenario(t *testing.T, name, description string) *skill.Skill {
	t.Helper()
	sk, err := skill.New(name, description, "Langkah: "+description, skill.ByAgent, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.skills.Save(sk); err != nil {
		t.Fatal(err)
	}

	turnPath := f.recordTurn(t, name+"-turn", "kerjakan "+name, []llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "list_dir", Args: json.RawMessage(`{"path":"."}`)}},
		},
		{Text: "selesai", StopReason: llm.StopEnd},
	})
	if err := f.skills.AttachScenario(name, skill.Scenario{
		Name:        "basic",
		Description: "harus memanggil list_dir",
		Expect:      skill.Expectations{MustCall: []string{"list_dir"}, MustSucceed: true},
	}, turnPath); err != nil {
		t.Fatal(err)
	}

	report, err := (&skill.Gate{Store: f.skills, Run: (&agent.ScenarioRunner{
		Tools: f.tools, Blobs: f.blobs, JournalDir: filepath.Join(f.dir, "eval-journal"),
	}).Run, NewID: func() string { return "eval-" + name }}).Certify(context.Background(), name)
	if err != nil {
		t.Fatalf("setting up %s as a proven active skill failed: %v\n%s", name, err, report.Summary())
	}
	sk, err = f.skills.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

func mergeResponse(name, description, body string) llm.Response {
	args, _ := json.Marshal(map[string]string{"name": name, "description": description, "body": body})
	return llm.Response{
		StopReason: llm.StopToolUse,
		ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "merge_skills", Args: args}},
	}
}

func keepSeparateResponse(reason string) llm.Response {
	args, _ := json.Marshal(map[string]string{"reason": reason})
	return llm.Response{
		StopReason: llm.StopToolUse,
		ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "keep_separate", Args: args}},
	}
}

func newConsolidator(f *fixture, responses []llm.Response) *Consolidator {
	return &Consolidator{
		Provider:   &llm.Static{Responses: responses},
		Model:      "test-model",
		Skills:     f.skills,
		JournalDir: filepath.Join(f.dir, "consolidate-journal"),
		Blobs:      f.blobs,
		Now:        fixedNow,
	}
}

// TestMergeProducesAQuarantinedSkillWithWorkingScenarios is the headline
// claim: a merge is not trusted because the model proposed it. The result
// starts in quarantine, and its carried-over scenarios still have to pass a
// real gate before it can be promoted — proving reuse of existing evidence
// actually works end to end.
func TestMergeProducesAQuarantinedSkillWithWorkingScenarios(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "deploy-staging", "Deploy ke staging dengan ship.sh")
	f.activeSkillWithScenario(t, "deploy-ke-staging", "Cara deploy ke staging pakai ship.sh")

	c := newConsolidator(f, []llm.Response{
		mergeResponse("deploy-final", "Deploy ke staging.", "Jalankan ship.sh staging."),
		{Text: "digabung.", StopReason: llm.StopEnd},
	})
	report, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Merged() != 1 {
		t.Fatalf("report is %+v", report.Outcomes)
	}
	outcome := report.Outcomes[0]
	if outcome.NewSkill != "deploy-final" {
		t.Fatalf("new skill is %q", outcome.NewSkill)
	}
	if outcome.TurnID == "" {
		t.Error("the comparison left no turn to inspect")
	}

	merged, err := f.skills.Load("deploy-final")
	if err != nil {
		t.Fatal(err)
	}
	if merged.State != skill.StateQuarantine {
		t.Fatalf("a merge proposed by a model is active without passing a gate: %s", merged.State)
	}

	scenarios, err := f.skills.Scenarios("deploy-final")
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("carried %d scenarios, want both sources' one each: %+v", len(scenarios), scenarios)
	}

	// The carried scenarios must actually replay and pass — proving the
	// recordings and expectations survived the copy, not just the file names.
	gateReport, err := (&skill.Gate{Store: f.skills, Run: (&agent.ScenarioRunner{
		Tools: f.tools, Blobs: f.blobs, JournalDir: filepath.Join(f.dir, "eval-journal-2"),
	}).Run}).Evaluate(context.Background(), "deploy-final")
	if err != nil {
		t.Fatal(err)
	}
	if !gateReport.Passed() {
		t.Fatalf("carried-over scenarios do not pass: %s", gateReport.Summary())
	}
}

// TestMergeArchivesBothSources checks the sources are retired, not deleted,
// with a reason a person can trace back to the merge that replaced them.
func TestMergeArchivesBothSources(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "a-deploy", "Deploy ke staging dengan ship.sh")
	f.activeSkillWithScenario(t, "b-deploy", "Deploy ke staging dengan ship.sh juga")

	c := newConsolidator(f, []llm.Response{
		mergeResponse("gabungan-deploy", "Deploy ke staging.", "Jalankan ship.sh."),
		{Text: "digabung.", StopReason: llm.StopEnd},
	})
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"a-deploy", "b-deploy"} {
		sk, err := f.skills.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		if sk.State != skill.StateArchived {
			t.Errorf("%s is %s, want archived", name, sk.State)
		}
		if !strings.Contains(sk.Reason, "gabungan-deploy") {
			t.Errorf("%s's archive reason does not name the merge: %q", name, sk.Reason)
		}
	}
}

// TestKeepSeparateChangesNothing is the common, correct outcome: most pairs
// that look similar on the surface are not the same idea.
func TestKeepSeparateChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "deploy-staging", "Deploy ke staging")
	f.activeSkillWithScenario(t, "deploy-produksi", "Deploy ke produksi, prosedurnya beda")

	c := newConsolidator(f, []llm.Response{
		keepSeparateResponse("staging dan produksi punya langkah verifikasi berbeda"),
		{Text: "dipertahankan terpisah.", StopReason: llm.StopEnd},
	})
	report, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Merged() != 0 {
		t.Fatalf("a pair that should stay separate was merged: %+v", report.Outcomes)
	}
	if report.Outcomes[0].Decision != DecisionKeptSeparate {
		t.Fatalf("decision is %q", report.Outcomes[0].Decision)
	}

	for _, name := range []string{"deploy-staging", "deploy-produksi"} {
		sk, err := f.skills.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		if sk.State != skill.StateActive {
			t.Errorf("%s is %s; keeping separate must not change either skill", name, sk.State)
		}
	}
}

// TestLedgerSkipsAPairAskedAboutRecently keeps a stable set of near-duplicate
// skills from being re-asked about, and paid for, on every run.
func TestLedgerSkipsAPairAskedAboutRecently(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "deploy-a", "Deploy ke staging")
	f.activeSkillWithScenario(t, "deploy-b", "Deploy ke staging juga")

	ledger, err := review.OpenLedger(filepath.Join(f.dir, "ledger.txt"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &llm.Static{Responses: []llm.Response{
		keepSeparateResponse("beda"),
		{Text: "dipertahankan terpisah.", StopReason: llm.StopEnd},
	}}
	c := newConsolidator(f, nil)
	c.Provider = provider
	c.Ledger = ledger

	first, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Outcomes) != 1 || first.Outcomes[0].Skipped {
		t.Fatalf("the first run should ask about the pair: %+v", first.Outcomes)
	}

	second, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Outcomes) != 1 || !second.Outcomes[0].Skipped {
		t.Fatalf("the second run should skip the same pair: %+v", second.Outcomes)
	}
	// One comparison takes two model calls: the tool decision, then the
	// closing answer. The repeat run must add none.
	if provider.Calls() != 2 {
		t.Errorf("the model was called %d times, want exactly 2 — the repeat should have cost nothing", provider.Calls())
	}
}

func TestMergeRefusesToCollideWithAnUnrelatedSkill(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "deploy-a", "Deploy ke staging")
	f.activeSkillWithScenario(t, "deploy-b", "Deploy ke staging juga")
	f.activeSkillWithScenario(t, "unrelated", "Sesuatu yang lain sama sekali")

	c := newConsolidator(f, []llm.Response{
		mergeResponse("unrelated", "Nama tabrakan.", "isi"),
		{Text: "digabung.", StopReason: llm.StopEnd},
	})
	report, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcomes[0].Decision != DecisionFailed {
		t.Fatalf("a merge colliding with an unrelated skill was accepted: %+v", report.Outcomes[0])
	}

	untouched, err := f.skills.Load("unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if untouched.State != skill.StateActive || untouched.Description != "Sesuatu yang lain sama sekali" {
		t.Fatalf("the unrelated skill was overwritten: %+v", untouched)
	}
}

func TestConsolidatorRefusesIncompleteConfiguration(t *testing.T) {
	f := newFixture(t)
	for name, c := range map[string]*Consolidator{
		"no provider": {Model: "m", Skills: f.skills, JournalDir: f.dir},
		"no skills":   {Provider: &llm.Static{}, Model: "m", JournalDir: f.dir},
		"no journal":  {Provider: &llm.Static{}, Model: "m", Skills: f.skills},
		"no model":    {Provider: &llm.Static{}, Skills: f.skills, JournalDir: f.dir},
	} {
		if _, err := c.Run(context.Background()); err == nil {
			t.Errorf("%s: an incomplete consolidator ran anyway", name)
		}
	}
}

// TestMergeCanKeepOneSourcesName is the regression for a bug live testing
// found: the model is free to name the merge after one of the two sources
// rather than inventing a third name, and applying that merge must not
// immediately archive the very file it just wrote as the result.
func TestMergeCanKeepOneSourcesName(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "lihat-isi-proyek", "Melihat isi direktori proyek")
	f.activeSkillWithScenario(t, "cek-struktur-direktori", "Memeriksa struktur direktori proyek")

	// The proposed name matches one of the two sources exactly.
	c := newConsolidator(f, []llm.Response{
		mergeResponse("cek-struktur-direktori", "Memeriksa struktur direktori proyek sebelum bekerja.", "Panggil list_dir."),
		{Text: "digabung.", StopReason: llm.StopEnd},
	})
	report, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Merged() != 1 {
		t.Fatalf("outcomes: %+v", report.Outcomes)
	}

	// The merge must survive as quarantine, not be clobbered back into
	// archived by the very call that was supposed to retire its sibling.
	merged, err := f.skills.Load("cek-struktur-direktori")
	if err != nil {
		t.Fatal(err)
	}
	if merged.State != skill.StateQuarantine {
		t.Fatalf("the merge destroyed itself: state is %s, want quarantine", merged.State)
	}
	if merged.Description != "Memeriksa struktur direktori proyek sebelum bekerja." {
		t.Errorf("the merged body did not take effect: %+v", merged)
	}

	// The other source, whose name was not reused, is archived as normal.
	other, err := f.skills.Load("lihat-isi-proyek")
	if err != nil {
		t.Fatal(err)
	}
	if other.State != skill.StateArchived {
		t.Errorf("the other source is %s, want archived", other.State)
	}

	// Both sources' scenarios must have carried over even though one of them
	// shares its directory with the merge result.
	scenarios, err := f.skills.Scenarios("cek-struktur-direktori")
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("carried %d scenarios, want 2: %+v", len(scenarios), scenarios)
	}

	gateReport, err := (&skill.Gate{Store: f.skills, Run: (&agent.ScenarioRunner{
		Tools: f.tools, Blobs: f.blobs, JournalDir: filepath.Join(f.dir, "eval-journal-3"),
	}).Run}).Evaluate(context.Background(), "cek-struktur-direktori")
	if err != nil {
		t.Fatal(err)
	}
	if !gateReport.Passed() {
		t.Fatalf("the same-name merge cannot be certified: %s", gateReport.Summary())
	}
}

// TestDecisionSurvivesAnEmptyClosingAnswer is the regression for a second bug
// live testing found: the model correctly called keep_separate, then produced
// an essentially empty closing sentence, which the agent loop treats as a turn
// failure. The decision that already happened must not be discarded because
// of what the model did one step later.
func TestDecisionSurvivesAnEmptyClosingAnswer(t *testing.T) {
	f := newFixture(t)
	f.activeSkillWithScenario(t, "deploy-staging", "Deploy ke staging tanpa approval")
	f.activeSkillWithScenario(t, "deploy-produksi", "Deploy ke produksi, wajib approval")

	c := newConsolidator(f, []llm.Response{
		keepSeparateResponse("staging tidak butuh approval, produksi wajib"),
		// A response with neither text nor a tool call — what a real
		// provider sent here, and what agent.Response.Validate rejects.
		{StopReason: llm.StopEnd},
	})
	report, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 {
		t.Fatalf("outcomes: %+v", report.Outcomes)
	}
	got := report.Outcomes[0]
	if got.Decision != DecisionKeptSeparate {
		t.Fatalf("decision is %q, want kept_separate — an empty closing answer discarded a decision that already happened", got.Decision)
	}
	if got.Reason == "" {
		t.Error("the reason the model gave was lost")
	}

	for _, name := range []string{"deploy-staging", "deploy-produksi"} {
		sk, err := f.skills.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		if sk.State != skill.StateActive {
			t.Errorf("%s is %s; a captured decision must still leave both skills untouched", name, sk.State)
		}
	}
}
