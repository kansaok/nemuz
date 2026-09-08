package review

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/memory"
	"github.com/kansaok/nemuz/internal/skill"
)

type fixture struct {
	dir      string
	memories *memory.Store
	skills   *skill.Store
	blobs    *blob.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	fixed := func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }

	mem, err := memory.Open(filepath.Join(dir, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	mem.SetClock(fixed)
	var n int
	mem.SetIDSource(func() (string, error) {
		n++
		return fmt.Sprintf("01TEST%020d", n), nil
	})

	sk, err := skill.Open(filepath.Join(dir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	sk.SetClock(fixed)

	bs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, memories: mem, skills: sk, blobs: bs}
}

func (f *fixture) reviewer(responses []llm.Response) *Reviewer {
	return &Reviewer{
		Provider:   &llm.Static{Responses: responses, Label: "review"},
		Model:      "claude-haiku-4-5-20251001",
		Memories:   f.memories,
		Skills:     f.skills,
		JournalDir: filepath.Join(f.dir, "journal"),
		Blobs:      f.blobs,
	}
}

func call(t *testing.T, id, name string, args any) llm.ToolCall {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return llm.ToolCall{ID: id, Name: name, Args: raw}
}

func goodTurn() Input {
	return Input{
		TurnID:    "01TURN000000000000000000AA",
		Prompt:    "bagaimana cara deploy proyek ini ke staging?",
		Answer:    "Jalankan scripts/ship.sh dengan argumen staging.",
		ToolsUsed: []string{"read_file", "list_dir"},
	}
}

// TestReviewSavesWhatItLearned is the happy path: the reviewer reads a finished
// turn and keeps the durable part of it.
func TestReviewSavesWhatItLearned(t *testing.T) {
	f := newFixture(t)
	r := f.reviewer([]llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{call(t, "c1", "remember", map[string]any{
				"text": "Deploy ke staging dijalankan lewat scripts/ship.sh staging",
				"kind": "project",
				"tags": []string{"deploy"},
			})},
		},
		{Text: "Saya simpan satu fakta tentang deploy.", StopReason: llm.StopEnd},
	})

	got, err := r.Review(context.Background(), goodTurn())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reviewed {
		t.Fatalf("the turn was skipped: %s", got.Skipped)
	}
	if len(got.Remembered) != 1 {
		t.Fatalf("remembered %v", got.Remembered)
	}

	stored, err := f.memories.Load(got.Remembered[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Text, "ship.sh") {
		t.Errorf("stored %q", stored.Text)
	}
	// A fact must be traceable to the conversation that produced it.
	if stored.Source != goodTurn().TurnID {
		t.Errorf("source is %q, want the reviewed turn", stored.Source)
	}
}

// TestDraftedSkillsAreQuarantined is the point where nemuz differs from every
// framework that learns without proving: the reviewer can propose, never enact.
func TestDraftedSkillsAreQuarantined(t *testing.T) {
	f := newFixture(t)
	r := f.reviewer([]llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{call(t, "c1", "draft_skill", map[string]any{
				"name":        "deploy-ke-staging",
				"description": "Cara men-deploy proyek ini ke staging.",
				"body":        "Jalankan `scripts/ship.sh staging` dari akar proyek.",
			})},
		},
		{Text: "Saya usulkan satu skill.", StopReason: llm.StopEnd},
	})

	got, err := r.Review(context.Background(), goodTurn())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Drafted) != 1 {
		t.Fatalf("drafted %v", got.Drafted)
	}

	sk, err := f.skills.Load("deploy-ke-staging")
	if err != nil {
		t.Fatal(err)
	}
	if sk.State != skill.StateQuarantine {
		t.Fatalf("a drafted skill is %s; a review must not be able to activate one", sk.State)
	}
	if sk.CreatedBy != skill.ByAgent {
		t.Errorf("author is %q", sk.CreatedBy)
	}

	// And it must not reach a model.
	active, err := f.skills.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("a freshly drafted skill was already active: %v", active)
	}
}

// TestReviewerCannotTouchAnythingElse is the safety property, and it is
// structural rather than a matter of the prompt asking nicely.
func TestReviewerCannotTouchAnythingElse(t *testing.T) {
	f := newFixture(t)
	reg, _, _, err := Registry(f.memories, f.skills, "01TURN")
	if err != nil {
		t.Fatal(err)
	}

	names := reg.Names()
	if len(names) != 2 {
		t.Fatalf("the reviewer has %d tools: %v", len(names), names)
	}
	for _, forbidden := range []string{"read_file", "write_file", "list_dir", "exec"} {
		if _, ok := reg.Get(forbidden); ok {
			t.Errorf("the reviewer can call %s", forbidden)
		}
	}
	if !reg.Capabilities().IsZero() {
		t.Errorf("the reviewer's tools ask for %+v; they should need nothing", reg.Capabilities())
	}
}

func TestDraftCannotOverwriteAnExistingSkill(t *testing.T) {
	f := newFixture(t)
	existing, err := skill.New("sudah-ada", "Skill yang sudah terbukti", "isi", skill.ByUser, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	existing.State = skill.StateActive
	if err := f.skills.Save(existing); err != nil {
		t.Fatal(err)
	}

	draft := NewDraftSkillTool(f.skills)
	res, err := draft.Run(context.Background(), json.RawMessage(
		`{"name":"sudah-ada","description":"versi lain","body":"isi berbeda"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a background review overwrote an existing skill")
	}

	after, _ := f.skills.Load("sudah-ada")
	if after.Body != "isi" || after.State != skill.StateActive {
		t.Fatalf("the existing skill was modified: %+v", after)
	}
}

// TestFailedTurnsAreNotReviewed keeps the agent from learning from its own
// mistakes in the literal, unhelpful sense.
func TestFailedTurnsAreNotReviewed(t *testing.T) {
	f := newFixture(t)
	r := f.reviewer([]llm.Response{{Text: "seharusnya tidak dipanggil", StopReason: llm.StopEnd}})

	in := goodTurn()
	in.Errored = true
	got, err := r.Review(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reviewed {
		t.Fatal("a failed turn was reviewed")
	}
	if !strings.Contains(got.Skipped, "error") {
		t.Errorf("skip reason is %q", got.Skipped)
	}
}

// ---------- novelty ----------

func TestRepeatedTurnShapesAreReviewedOnce(t *testing.T) {
	f := newFixture(t)
	ledger, err := OpenLedger(filepath.Join(f.dir, "reviewed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &llm.Static{Responses: []llm.Response{
		{Text: "Tidak ada yang perlu disimpan.", StopReason: llm.StopEnd},
	}}
	r := f.reviewer(nil)
	r.Provider = provider
	r.Ledger = ledger

	first, err := r.Review(context.Background(), goodTurn())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Reviewed {
		t.Fatalf("the first turn was skipped: %s", first.Skipped)
	}

	// The same question, phrased identically, with the same tools.
	second, err := r.Review(context.Background(), goodTurn())
	if err != nil {
		t.Fatal(err)
	}
	if second.Reviewed {
		t.Fatal("an identical turn shape was reviewed a second time")
	}
	if provider.Calls() != 1 {
		t.Errorf("the provider was called %d times; the repeat should have cost nothing", provider.Calls())
	}
}

func TestDifferentTurnShapesAreBothReviewed(t *testing.T) {
	f := newFixture(t)
	ledger, _ := OpenLedger(filepath.Join(f.dir, "reviewed.txt"))
	r := f.reviewer([]llm.Response{
		{Text: "tidak ada", StopReason: llm.StopEnd},
		{Text: "tidak ada", StopReason: llm.StopEnd},
	})
	r.Ledger = ledger

	if got, err := r.Review(context.Background(), goodTurn()); err != nil || !got.Reviewed {
		t.Fatalf("first: %v %+v", err, got)
	}
	other := goodTurn()
	other.TurnID = "01TURN000000000000000000BB"
	other.Prompt = "bagaimana cara menjalankan tes basis data?"

	got, err := r.Review(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reviewed {
		t.Fatalf("a genuinely different turn was skipped: %s", got.Skipped)
	}
}

func TestFingerprintIgnoresTheAnswer(t *testing.T) {
	a := goodTurn()
	b := goodTurn()
	b.Answer = "jawaban yang sama sekali berbeda dan jauh lebih panjang"

	if a.Fingerprint() != b.Fingerprint() {
		t.Error("the answer changed the fingerprint; two identical questions have nothing new to teach")
	}

	c := goodTurn()
	c.ToolsUsed = []string{"write_file"}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("different tools produced the same fingerprint")
	}
}

func TestFingerprintIgnoresWordOrder(t *testing.T) {
	a := goodTurn()
	b := goodTurn()
	b.ToolsUsed = []string{"list_dir", "read_file"} // same set, other order
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("tool order changed the fingerprint")
	}
}

func TestLedgerForgetsTheOldest(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(filepath.Join(dir, "reviewed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	l.SetSize(3)

	for i := 0; i < 5; i++ {
		if err := l.Record(fmt.Sprintf("shape-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if l.Len() != 3 {
		t.Fatalf("ledger holds %d entries, want 3", l.Len())
	}
	if seen, _ := l.Seen("shape-0"); seen {
		t.Error("the oldest shape was not forgotten")
	}
	if seen, _ := l.Seen("shape-4"); !seen {
		t.Error("the newest shape is missing")
	}
}

func TestLedgerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviewed.txt")
	first, _ := OpenLedger(path)
	if err := first.Record("shape-a"); err != nil {
		t.Fatal(err)
	}

	second, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if seen, _ := second.Seen("shape-a"); !seen {
		t.Fatal("the ledger did not survive being reopened")
	}
}

func TestReviewerRefusesIncompleteConfiguration(t *testing.T) {
	f := newFixture(t)
	for name, r := range map[string]*Reviewer{
		"no provider": {Model: "m", Memories: f.memories, Skills: f.skills, JournalDir: f.dir},
		"no model":    {Provider: &llm.Static{}, Memories: f.memories, Skills: f.skills, JournalDir: f.dir},
		"no memories": {Provider: &llm.Static{}, Model: "m", Skills: f.skills, JournalDir: f.dir},
		"no skills":   {Provider: &llm.Static{}, Model: "m", Memories: f.memories, JournalDir: f.dir},
		"no journal":  {Provider: &llm.Static{}, Model: "m", Memories: f.memories, Skills: f.skills},
	} {
		if _, err := r.Review(context.Background(), goodTurn()); err == nil {
			t.Errorf("%s: an incomplete reviewer ran anyway", name)
		}
	}
}
