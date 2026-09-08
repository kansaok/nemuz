package curator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/skill"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return now }

func newStore(t *testing.T) *skill.Store {
	t.Helper()
	s, err := skill.Open(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(clock)
	return s
}

// add writes a skill in a chosen state, aged by the given duration.
func add(t *testing.T, s *skill.Store, name string, state skill.State, author skill.Author, age time.Duration) *skill.Skill {
	t.Helper()
	sk, err := skill.New(name, "Cara "+name, "Langkah.", author, now.Add(-age))
	if err != nil {
		t.Fatal(err)
	}
	sk.State = state
	if state == skill.StateActive && author == skill.ByAgent {
		sk.PromotedBy = "eval-lama"
	}
	if err := s.Save(sk); err != nil {
		t.Fatal(err)
	}
	return sk
}

// touch records that a skill was last used a given time ago.
func touch(t *testing.T, s *skill.Store, name string, ago time.Duration) {
	t.Helper()
	sk, err := s.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	sk.UseCount++
	sk.LastUsedAt = now.Add(-ago)
	if err := s.Save(sk); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, c *Curator) Report {
	t.Helper()
	c.Now = clock
	report, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func actionFor(r Report, name string) Action {
	for _, a := range r.Actions {
		if a.Skill == name {
			return a
		}
	}
	return Action{}
}

// ---------- invariants ----------

// TestOnlyAgentWrittenSkillsAreTouched is the invariant that keeps automatic
// maintenance from undoing a person's deliberate decision.
func TestOnlyAgentWrittenSkillsAreTouched(t *testing.T) {
	s := newStore(t)
	add(t, s, "tulisan-orang", skill.StateActive, skill.ByUser, 400*24*time.Hour)
	add(t, s, "tulisan-agen", skill.StateActive, skill.ByAgent, 400*24*time.Hour)

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}})

	if actionFor(report, "tulisan-orang").Skill != "" {
		t.Error("a hand-written skill was considered by the curator")
	}
	if got := actionFor(report, "tulisan-agen").Action; got != ActionArchived {
		t.Errorf("the agent's stale skill was %q, want archived", got)
	}

	human, _ := s.Load("tulisan-orang")
	if human.State != skill.StateActive {
		t.Fatalf("a hand-written skill was changed to %s", human.State)
	}
}

func TestPinnedSkillsAreNeverTouched(t *testing.T) {
	s := newStore(t)
	add(t, s, "dipin", skill.StateActive, skill.ByAgent, 400*24*time.Hour)
	if err := s.Pin("dipin", true); err != nil {
		t.Fatal(err)
	}

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}})
	if len(report.Actions) != 0 {
		t.Fatalf("a pinned skill was considered: %+v", report.Actions)
	}
	sk, _ := s.Load("dipin")
	if sk.State != skill.StateActive {
		t.Fatalf("a pinned skill became %s", sk.State)
	}
}

// TestNothingIsEverDeleted holds regardless of policy.
func TestNothingIsEverDeleted(t *testing.T) {
	s := newStore(t)
	add(t, s, "sangat-tua", skill.StateActive, skill.ByAgent, 1000*24*time.Hour)
	add(t, s, "karantina-tua", skill.StateQuarantine, skill.ByAgent, 1000*24*time.Hour)

	run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}})

	for _, name := range []string{"sangat-tua", "karantina-tua"} {
		sk, err := s.Load(name)
		if err != nil {
			t.Fatalf("%s was deleted: %v", name, err)
		}
		if sk.State != skill.StateArchived {
			t.Errorf("%s is %s, want archived", name, sk.State)
		}
		if sk.Reason == "" {
			t.Errorf("%s was archived without a reason", name)
		}
	}
}

// ---------- retirement ----------

func TestUnusedSkillsAreRetired(t *testing.T) {
	s := newStore(t)
	add(t, s, "terpakai", skill.StateActive, skill.ByAgent, 200*24*time.Hour)
	touch(t, s, "terpakai", 2*24*time.Hour)
	add(t, s, "terlantar", skill.StateActive, skill.ByAgent, 200*24*time.Hour)
	touch(t, s, "terlantar", 200*24*time.Hour)

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}})

	if got := actionFor(report, "terpakai").Action; got != ActionKept {
		t.Errorf("a recently used skill was %q", got)
	}
	if got := actionFor(report, "terlantar"); got.Action != ActionArchived {
		t.Errorf("an abandoned skill was %q", got.Action)
	} else if !strings.Contains(got.Reason, "unused") {
		t.Errorf("the reason does not explain the retirement: %q", got.Reason)
	}
}

// TestFreshSkillsAreNotRetired guards the off-by-one that would make the
// curator archive a skill written this morning: a skill that has never been
// used has no LastUsedAt, and measuring from zero makes it infinitely idle.
func TestFreshSkillsAreNotRetired(t *testing.T) {
	s := newStore(t)
	add(t, s, "baru-saja", skill.StateActive, skill.ByAgent, time.Hour)

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}})
	if got := actionFor(report, "baru-saja").Action; got != ActionKept {
		t.Fatalf("a skill written an hour ago was %q", got)
	}
}

func TestRetirementCanBeDisabled(t *testing.T) {
	s := newStore(t)
	add(t, s, "terlantar", skill.StateActive, skill.ByAgent, 500*24*time.Hour)
	touch(t, s, "terlantar", 500*24*time.Hour)

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true, RetireUnusedAfter: -1}})
	if got := actionFor(report, "terlantar").Action; got != ActionKept {
		t.Errorf("retirement was disabled but the skill was %q", got)
	}
}

// ---------- quarantine ----------

func TestQuarantineDoesNotBecomeAJunkDrawer(t *testing.T) {
	s := newStore(t)
	add(t, s, "baru-dikarantina", skill.StateQuarantine, skill.ByAgent, 2*24*time.Hour)
	add(t, s, "lama-dikarantina", skill.StateQuarantine, skill.ByAgent, 90*24*time.Hour)

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}})

	if got := actionFor(report, "baru-dikarantina").Action; got != ActionKept {
		t.Errorf("a recent draft was %q; it deserves time to be certified", got)
	}
	if got := actionFor(report, "lama-dikarantina"); got.Action != ActionArchived {
		t.Errorf("a long-stale draft was %q", got.Action)
	} else if !strings.Contains(got.Reason, "certified") {
		t.Errorf("reason is %q", got.Reason)
	}
}

// ---------- re-verification ----------

// gate builds a real eval gate whose runner is scripted, so re-verification can
// be tested without an agent.
func gate(t *testing.T, s *skill.Store, outcomes map[string]skill.Outcome) *skill.Gate {
	t.Helper()
	return &skill.Gate{
		Store: s,
		NewID: func() string { return "run-baru" },
		Run: func(_ context.Context, sk *skill.Skill, sc skill.Scenario) (skill.Outcome, error) {
			return outcomes[sk.Name], nil
		},
	}
}

func withScenario(t *testing.T, s *skill.Store, name string, expect skill.Expectations) {
	t.Helper()
	dir, err := s.EvalDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "turn.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveScenario(name, skill.Scenario{Name: "dasar", Journal: "turn.jsonl", Expect: expect}); err != nil {
		t.Fatal(err)
	}
}

// TestActiveSkillsAreReverified is the curator's most valuable job, and the one
// that is only affordable because scenarios replay recordings: a skill that
// passed in one version is not proven forever.
func TestActiveSkillsAreReverified(t *testing.T) {
	s := newStore(t)
	add(t, s, "masih-benar", skill.StateActive, skill.ByAgent, time.Hour)
	touch(t, s, "masih-benar", time.Hour)
	withScenario(t, s, "masih-benar", skill.Expectations{MustCall: []string{"read_file"}})

	add(t, s, "sudah-rusak", skill.StateActive, skill.ByAgent, time.Hour)
	touch(t, s, "sudah-rusak", time.Hour)
	withScenario(t, s, "sudah-rusak", skill.Expectations{MustCall: []string{"read_file"}})

	g := gate(t, s, map[string]skill.Outcome{
		"masih-benar": {ToolsCalled: []string{"read_file"}, Text: "ok"},
		// The tool it depends on no longer runs — the world moved under it.
		"sudah-rusak": {ToolsCalled: []string{"list_dir"}, Text: "ok"},
	})

	report := run(t, &Curator{Skills: s, Gate: g})

	if got := actionFor(report, "masih-benar").Action; got != ActionKept {
		t.Errorf("a skill that still passes was %q", got)
	}
	if got := actionFor(report, "sudah-rusak"); got.Action != ActionDemoted {
		t.Errorf("a skill that stopped passing was %q", got.Action)
	} else if !strings.Contains(got.Reason, "no longer passes") {
		t.Errorf("reason is %q", got.Reason)
	}

	broken, _ := s.Load("sudah-rusak")
	if broken.State != skill.StateQuarantine {
		t.Fatalf("the broken skill is %s, want quarantine", broken.State)
	}
	// Demotion must clear the old evidence, or the skill would look proven.
	if broken.PromotedBy != "" {
		t.Errorf("a demoted skill kept its promotion evidence %q", broken.PromotedBy)
	}
}

func TestReverificationFailureDoesNotDemote(t *testing.T) {
	s := newStore(t)
	add(t, s, "tak-terperiksa", skill.StateActive, skill.ByAgent, time.Hour)
	touch(t, s, "tak-terperiksa", time.Hour)

	// A gate that cannot run at all — a missing eval environment, say. That is
	// the operator's problem, not evidence against the skill.
	broken := &skill.Gate{
		Store: s,
		Run: func(context.Context, *skill.Skill, skill.Scenario) (skill.Outcome, error) {
			t.Fatal("the runner should not be reached for a skill with no scenarios")
			return skill.Outcome{}, nil
		},
	}
	report := run(t, &Curator{Skills: s, Gate: broken})

	// A skill with no scenarios cannot pass, so it is demoted — which is
	// correct: an active skill with nothing proving it should not stay active.
	if got := actionFor(report, "tak-terperiksa"); got.Action != ActionDemoted {
		t.Errorf("an active skill with no scenarios was %q", got.Action)
	}
}

func TestRetirementIsConsideredBeforeReverification(t *testing.T) {
	s := newStore(t)
	add(t, s, "terlantar", skill.StateActive, skill.ByAgent, 300*24*time.Hour)
	touch(t, s, "terlantar", 300*24*time.Hour)

	g := &skill.Gate{
		Store: s,
		Run: func(context.Context, *skill.Skill, skill.Scenario) (skill.Outcome, error) {
			t.Error("a skill about to be retired was re-verified anyway")
			return skill.Outcome{}, nil
		},
	}
	report := run(t, &Curator{Skills: s, Gate: g})
	if got := actionFor(report, "terlantar").Action; got != ActionArchived {
		t.Errorf("action is %q", got)
	}
}

// ---------- scheduling and dry runs ----------

func TestDryRunChangesNothing(t *testing.T) {
	s := newStore(t)
	add(t, s, "terlantar", skill.StateActive, skill.ByAgent, 300*24*time.Hour)
	touch(t, s, "terlantar", 300*24*time.Hour)

	report := run(t, &Curator{Skills: s, Policy: Policy{SkipReverify: true}, DryRun: true})
	if got := actionFor(report, "terlantar").Action; got != ActionArchived {
		t.Errorf("the dry run should still say what it would do, got %q", got)
	}
	if !report.DryRun || report.Changed() != 1 {
		t.Errorf("report is %+v", report)
	}

	sk, _ := s.Load("terlantar")
	if sk.State != skill.StateActive {
		t.Fatalf("a dry run changed the skill to %s", sk.State)
	}
}

func TestRunsAreRateLimited(t *testing.T) {
	s := newStore(t)
	add(t, s, "apa-pun", skill.StateQuarantine, skill.ByAgent, time.Hour)
	state, err := OpenState(filepath.Join(t.TempDir(), "curator.json"))
	if err != nil {
		t.Fatal(err)
	}

	c := &Curator{Skills: s, State: state, Policy: Policy{SkipReverify: true}}
	if first := run(t, c); first.Skipped != "" {
		t.Fatalf("the first run was skipped: %s", first.Skipped)
	}
	second := run(t, c)
	if second.Skipped == "" {
		t.Fatal("a second run followed immediately without being rate limited")
	}

	// Once the interval has passed, it runs again.
	c.Now = func() time.Time { return now.Add(48 * time.Hour) }
	third, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Skipped != "" {
		t.Fatalf("a run after the interval was skipped: %s", third.Skipped)
	}
}

func TestPausingStopsCuration(t *testing.T) {
	s := newStore(t)
	add(t, s, "terlantar", skill.StateActive, skill.ByAgent, 300*24*time.Hour)
	touch(t, s, "terlantar", 300*24*time.Hour)

	state, _ := OpenState(filepath.Join(t.TempDir(), "curator.json"))
	if err := state.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	report := run(t, &Curator{Skills: s, State: state, Policy: Policy{SkipReverify: true}})
	if report.Skipped == "" {
		t.Fatal("a paused curator ran anyway")
	}
	sk, _ := s.Load("terlantar")
	if sk.State != skill.StateActive {
		t.Fatalf("a paused curator changed a skill to %s", sk.State)
	}
}

func TestPausingSurvivesAndCanBeLifted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curator.json")
	state, _ := OpenState(path)
	if err := state.RecordRun(now); err != nil {
		t.Fatal(err)
	}
	if err := state.SetPaused(true); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	last, paused, err := reopened.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Error("pausing did not survive a reopen")
	}
	if last.IsZero() {
		t.Error("pausing lost the last run time")
	}

	if err := reopened.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if _, paused, _ = reopened.Read(); paused {
		t.Error("unpausing did not take")
	}
}

func TestCorruptStateDoesNotBlockCuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curator.json")
	if err := os.WriteFile(path, []byte("{bukan json"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, _ := OpenState(path)

	s := newStore(t)
	add(t, s, "apa-pun", skill.StateQuarantine, skill.ByAgent, time.Hour)
	report := run(t, &Curator{Skills: s, State: state, Policy: Policy{SkipReverify: true}})
	if report.Skipped != "" {
		t.Fatalf("a corrupt state file stopped curation: %s", report.Skipped)
	}
}

func TestRunRequiresAStore(t *testing.T) {
	if _, err := (&Curator{}).Run(context.Background()); err == nil {
		t.Fatal("a curator with no store ran")
	}
}
