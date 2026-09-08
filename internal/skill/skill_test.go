package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedTime() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(fixedTime)
	return s
}

func learned(t *testing.T, s *Store, name string) *Skill {
	t.Helper()
	sk, err := New(name, "Cara "+name, "Langkah pertama.\nLangkah kedua.", ByAgent, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(sk); err != nil {
		t.Fatal(err)
	}
	return sk
}

// ---------- lifecycle invariants ----------

// TestNewSkillsStartQuarantined is the gate's first half: nothing the agent
// writes reaches a model on its own say-so.
func TestNewSkillsStartQuarantined(t *testing.T) {
	s := newStore(t)
	learned(t, s, "baca-log")

	sk, err := s.Load("baca-log")
	if err != nil {
		t.Fatal(err)
	}
	if sk.State != StateQuarantine {
		t.Fatalf("a new skill is %s, want quarantine", sk.State)
	}

	active, err := s.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("a quarantined skill was offered to models: %v", active)
	}
}

// TestPromotionRequiresPassingEvidence is the gate's second half, and the
// difference between nemuz and every framework that learns without proving.
func TestPromotionRequiresPassingEvidence(t *testing.T) {
	s := newStore(t)
	learned(t, s, "baca-log")

	failing := Report{ID: "e1", Skill: "baca-log", MinScenarios: 1, Results: []Result{
		{Scenario: "dasar", Passed: false, Failures: []string{"never called read_file"}},
	}}
	if err := s.Promote("baca-log", failing); err == nil {
		t.Fatal("a skill was promoted on a failing report")
	}

	empty := Report{ID: "e2", Skill: "baca-log", MinScenarios: 1}
	if err := s.Promote("baca-log", empty); err == nil {
		t.Fatal("a skill with no scenarios was promoted; silence is not evidence")
	}

	wrongSkill := Report{ID: "e3", Skill: "skill-lain", MinScenarios: 1, Results: []Result{
		{Scenario: "dasar", Passed: true},
	}}
	if err := s.Promote("baca-log", wrongSkill); err == nil {
		t.Fatal("a report for a different skill was accepted as evidence")
	}

	passing := Report{ID: "e4", Skill: "baca-log", MinScenarios: 1, Results: []Result{
		{Scenario: "dasar", Passed: true},
	}}
	if err := s.Promote("baca-log", passing); err != nil {
		t.Fatalf("a passing report did not promote the skill: %v", err)
	}

	sk, _ := s.Load("baca-log")
	if sk.State != StateActive {
		t.Fatalf("skill is %s after promotion", sk.State)
	}
	if sk.PromotedBy != "e4" {
		t.Errorf("the skill does not record which run promoted it, got %q", sk.PromotedBy)
	}
}

// TestActiveAgentSkillWithoutEvidenceIsInvalid stops the gate being bypassed by
// editing a file: a skill claiming Active with no evidence fails to validate.
func TestActiveAgentSkillWithoutEvidenceIsInvalid(t *testing.T) {
	sk := &Skill{
		Name: "diselundupkan", Description: "d", Body: "b",
		State: StateActive, CreatedBy: ByAgent, CreatedAt: fixedTime(),
	}
	err := sk.Validate()
	if err == nil {
		t.Fatal("an active agent skill with no promotion evidence validated")
	}
	if !strings.Contains(err.Error(), "evidence") {
		t.Errorf("error is %v", err)
	}

	// A person writing a skill by hand is a deliberate act and needs no gate.
	sk.CreatedBy = ByUser
	if err := sk.Validate(); err != nil {
		t.Errorf("a hand-written active skill was rejected: %v", err)
	}
}

func TestArchivingIsReversibleAndReturnsToQuarantine(t *testing.T) {
	s := newStore(t)
	learned(t, s, "usang")
	passing := Report{ID: "e1", Skill: "usang", MinScenarios: 1, Results: []Result{{Scenario: "x", Passed: true}}}
	if err := s.Promote("usang", passing); err != nil {
		t.Fatal(err)
	}

	if err := s.Archive("usang", "tidak dipakai 90 hari"); err != nil {
		t.Fatal(err)
	}
	sk, _ := s.Load("usang")
	if sk.State != StateArchived {
		t.Fatalf("skill is %s", sk.State)
	}
	if sk.Reason == "" {
		t.Error("archiving recorded no reason")
	}

	if err := s.Restore("usang"); err != nil {
		t.Fatal(err)
	}
	sk, _ = s.Load("usang")
	// Restoring to Active would let a retirement be silently undone; whatever
	// made the skill wrong may still be true.
	if sk.State != StateQuarantine {
		t.Fatalf("a restored skill is %s, want quarantine so it must prove itself again", sk.State)
	}
	if sk.PromotedBy != "" {
		t.Error("a restored skill kept its old promotion evidence")
	}
}

func TestPinnedSkillsResistAutomaticCuration(t *testing.T) {
	s := newStore(t)
	learned(t, s, "penting")
	if err := s.Pin("penting", true); err != nil {
		t.Fatal(err)
	}

	sk, _ := s.Load("penting")
	if sk.Curatable() {
		t.Error("a pinned skill reported itself as curatable")
	}
	if err := s.Archive("penting", "coba"); err == nil {
		t.Fatal("a pinned skill was archived")
	}
}

func TestHandWrittenSkillsAreNeverCurated(t *testing.T) {
	sk, err := New("manual", "Ditulis orang", "isi", ByUser, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if sk.Curatable() {
		t.Error("a hand-written skill reported itself as curatable")
	}
}

func TestStoreOffersNoWayToDelete(t *testing.T) {
	// Archiving is the strongest removal in the API. This is checked by
	// reading the surface rather than by reflection: if a Delete method is
	// ever added, this test is where the argument has to be had.
	s := newStore(t)
	learned(t, s, "apa-pun")
	if err := s.Archive("apa-pun", "selesai"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("apa-pun"); err != nil {
		t.Fatalf("an archived skill became unreadable: %v", err)
	}
}

// ---------- storage ----------

func TestRoundTripsThroughDiskUnchanged(t *testing.T) {
	s := newStore(t)
	original := learned(t, s, "kompleks")
	original.Related = []string{"lain", "satu-lagi"}
	original.UseCount = 7
	if err := s.Save(original); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load("kompleks")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != original.Description || got.Body != original.Body {
		t.Errorf("content changed on disk: %+v", got)
	}
	if got.UseCount != 7 || len(got.Related) != 2 {
		t.Errorf("bookkeeping was lost: %+v", got)
	}
}

// TestSkillsAreHumanReadableOnDisk matters because the format is the interface
// a person uses to review, edit, and diff what the agent taught itself.
func TestSkillsAreHumanReadableOnDisk(t *testing.T) {
	s := newStore(t)
	learned(t, s, "terbaca")

	body, err := os.ReadFile(filepath.Join(s.Root(), "terbaca", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.HasPrefix(text, "---\n") {
		t.Error("the file does not open with YAML frontmatter")
	}
	if !strings.Contains(text, "state: quarantine") {
		t.Error("the state is not visible in the frontmatter")
	}
	if !strings.Contains(text, "Langkah pertama.") {
		t.Error("the instructions are not present as plain Markdown")
	}
}

func TestInvalidNamesAreRefused(t *testing.T) {
	s := newStore(t)
	for _, name := range []string{"../escape", "Upper", "with space", "", "a/b", "dot.dot"} {
		if ValidName(name) {
			t.Errorf("%q was accepted as a skill name", name)
		}
		if _, err := s.Load(name); err == nil {
			t.Errorf("%q was loadable", name)
		}
	}
	for _, name := range []string{"baca-log", "a", "satu-dua-tiga"} {
		if !ValidName(name) {
			t.Errorf("%q was rejected", name)
		}
	}
}

func TestMissingSkillReportsNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.Load("tidak-ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRecordUseDrivesCuration(t *testing.T) {
	s := newStore(t)
	learned(t, s, "dipakai")

	for i := 0; i < 3; i++ {
		if err := s.RecordUse("dipakai"); err != nil {
			t.Fatal(err)
		}
	}
	sk, _ := s.Load("dipakai")
	if sk.UseCount != 3 {
		t.Errorf("use count is %d, want 3", sk.UseCount)
	}
	if sk.LastUsedAt.IsZero() {
		t.Error("last used was not recorded")
	}
}

// ---------- the gate ----------

// fakeRunner answers scenarios from a table, so the gate's logic is tested
// without an agent, a provider, or a journal.
func fakeRunner(outcomes map[string]Outcome, fail map[string]error) Runner {
	return func(_ context.Context, _ *Skill, sc Scenario) (Outcome, error) {
		if err, ok := fail[sc.Name]; ok {
			return Outcome{}, err
		}
		return outcomes[sc.Name], nil
	}
}

func withScenarios(t *testing.T, s *Store, name string, scenarios ...Scenario) {
	t.Helper()
	for _, sc := range scenarios {
		if sc.Journal == "" {
			sc.Journal = "turn.jsonl"
		}
		if err := s.SaveScenario(name, sc); err != nil {
			t.Fatal(err)
		}
	}
	dir, _ := s.EvalDir(name)
	if err := os.WriteFile(filepath.Join(dir, "turn.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGateCertifiesASkillThatBehaves(t *testing.T) {
	s := newStore(t)
	learned(t, s, "baca-log")
	withScenarios(t, s, "baca-log", Scenario{
		Name:   "membaca-berkas",
		Expect: Expectations{MustCall: []string{"read_file"}, MustContain: []string{"ERROR"}, MustSucceed: true},
	})

	gate := &Gate{
		Store: s,
		NewID: func() string { return "run-1" },
		Run: fakeRunner(map[string]Outcome{
			"membaca-berkas": {Text: "Ada 3 baris ERROR.", ToolsCalled: []string{"read_file"}, Steps: 2},
		}, nil),
	}

	report, err := gate.Certify(context.Background(), "baca-log")
	if err != nil {
		t.Fatalf("a well-behaved skill was not certified: %v\n%s", err, report.Summary())
	}
	sk, _ := s.Load("baca-log")
	if sk.State != StateActive {
		t.Fatalf("skill is %s after certification", sk.State)
	}
	if sk.PromotedBy != "run-1" {
		t.Errorf("promotion evidence is %q", sk.PromotedBy)
	}
}

// TestGateHoldsBackASkillThatMisbehaves is the test that gives the gate meaning.
func TestGateHoldsBackASkillThatMisbehaves(t *testing.T) {
	s := newStore(t)
	learned(t, s, "berbahaya")
	withScenarios(t, s, "berbahaya",
		Scenario{Name: "jangan-menulis", Expect: Expectations{MustNotCall: []string{"write_file"}}},
		Scenario{Name: "harus-ringkas", Expect: Expectations{MaxSteps: 3}},
	)

	gate := &Gate{
		Store: s,
		Run: fakeRunner(map[string]Outcome{
			"jangan-menulis": {ToolsCalled: []string{"read_file", "write_file"}, Steps: 2},
			"harus-ringkas":  {ToolsCalled: []string{"read_file"}, Steps: 9},
		}, nil),
	}

	report, err := gate.Certify(context.Background(), "berbahaya")
	if err == nil {
		t.Fatal("a misbehaving skill was certified")
	}
	if report.Passed() {
		t.Fatal("the report claims a pass")
	}
	sk, _ := s.Load("berbahaya")
	if sk.State != StateQuarantine {
		t.Fatalf("a failing skill is %s, want quarantine", sk.State)
	}

	// The report must say what went wrong, not merely that something did.
	joined := strings.Join(append(report.Results[0].Failures, report.Results[1].Failures...), " | ")
	if !strings.Contains(joined, "write_file") {
		t.Errorf("the forbidden tool is not named: %s", joined)
	}
	if !strings.Contains(joined, "9 steps") {
		t.Errorf("the step overrun is not explained: %s", joined)
	}
}

// TestBrokenScenarioCountsAsFailure closes the obvious way past the gate:
// break the recording and the skill sails through.
func TestBrokenScenarioCountsAsFailure(t *testing.T) {
	s := newStore(t)
	learned(t, s, "rusak")
	withScenarios(t, s, "rusak", Scenario{Name: "jurnal-hilang", Expect: Expectations{MustSucceed: true}})

	gate := &Gate{
		Store: s,
		Run:   fakeRunner(nil, map[string]error{"jurnal-hilang": errors.New("recording is missing")}),
	}

	report, _ := gate.Evaluate(context.Background(), "rusak")
	if report.Passed() {
		t.Fatal("a scenario that could not run was treated as a pass")
	}
	if !strings.Contains(strings.Join(report.Results[0].Failures, " "), "could not run") {
		t.Errorf("failures are %v", report.Results[0].Failures)
	}
}

func TestSkillWithoutScenariosCannotPass(t *testing.T) {
	s := newStore(t)
	learned(t, s, "tanpa-uji")

	gate := &Gate{Store: s, Run: fakeRunner(nil, nil)}
	report, err := gate.Certify(context.Background(), "tanpa-uji")
	if err == nil {
		t.Fatal("a skill with no scenarios was certified")
	}
	if !strings.Contains(report.Summary(), "no eval scenarios") {
		t.Errorf("summary is %q", report.Summary())
	}
}

func TestMinScenariosIsEnforced(t *testing.T) {
	s := newStore(t)
	learned(t, s, "satu-saja")
	withScenarios(t, s, "satu-saja", Scenario{Name: "dasar", Expect: Expectations{MustSucceed: true}})

	gate := &Gate{
		Store:        s,
		MinScenarios: 3,
		Run:          fakeRunner(map[string]Outcome{"dasar": {Text: "ok"}}, nil),
	}
	report, err := gate.Certify(context.Background(), "satu-saja")
	if err == nil {
		t.Fatal("one scenario satisfied a bar of three")
	}
	if !strings.Contains(report.Summary(), "3 are required") {
		t.Errorf("summary is %q", report.Summary())
	}
}

// TestScenarioJournalCannotEscapeItsSkill guards a path-traversal route into
// arbitrary files on the machine.
func TestScenarioJournalCannotEscapeItsSkill(t *testing.T) {
	s := newStore(t)
	learned(t, s, "nakal")
	dir, _ := s.EvalDir("nakal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"name":"x","journal":"../../../../etc/passwd"}`, `{"name":"y","journal":"/etc/passwd"}`} {
		if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Scenarios("nakal"); err == nil {
			t.Fatalf("a scenario pointing outside the skill was accepted: %s", bad)
		}
	}
}
