package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) })

	// Sequential ids keep file names and tie-breaking stable across runs.
	var n int
	s.SetIDSource(func() (string, error) {
		n++
		return fmt.Sprintf("01TEST%020d", n), nil
	})
	return s
}

func remember(t *testing.T, s *Store, text string, kind Kind, tags ...string) *Memory {
	t.Helper()
	m, err := s.Remember(text, kind, "01TURN", tags)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRememberAndLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	saved := remember(t, s, "Skrip deploy ada di scripts/ship.sh", KindProject, "deploy", "Deploy")

	got, err := s.Load(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != saved.Text {
		t.Errorf("text is %q", got.Text)
	}
	if got.Kind != KindProject || got.Source != "01TURN" {
		t.Errorf("metadata is %+v", got)
	}
	// Tags are lowercased and deduplicated so recall does not depend on how
	// the model happened to capitalise them.
	if len(got.Tags) != 1 || got.Tags[0] != "deploy" {
		t.Errorf("tags are %v, want one normalised tag", got.Tags)
	}
}

func TestStoredMemoriesAreHumanReadable(t *testing.T) {
	s := newStore(t)
	m := remember(t, s, "Pengguna lebih suka jawaban singkat", KindUser)

	body, err := os.ReadFile(filepath.Join(s.Root(), m.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.HasPrefix(text, "---\n") || !strings.Contains(text, "kind: user") {
		t.Errorf("frontmatter is missing or wrong:\n%s", text)
	}
	if !strings.Contains(text, "Pengguna lebih suka jawaban singkat") {
		t.Error("the fact itself is not present as plain text")
	}
}

// TestForgetActuallyRemoves states the difference from skills on purpose: a
// wrong fact should stop being there, not linger in an archive.
func TestForgetActuallyRemoves(t *testing.T) {
	s := newStore(t)
	m := remember(t, s, "Ini salah", KindProject)

	if err := s.Forget(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the memory survived being forgotten: %v", err)
	}
	if err := s.Forget(m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("forgetting twice should report not-found, got %v", err)
	}
}

func TestOversizedMemoriesAreRefused(t *testing.T) {
	s := newStore(t)
	_, err := s.Remember(strings.Repeat("x", MaxTextBytes+1), KindProject, "", nil)
	if err == nil {
		t.Fatal("an oversized memory was stored")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("the error should say where long content belongs, got: %v", err)
	}
}

func TestEmptyAndUnknownKindsAreRefused(t *testing.T) {
	s := newStore(t)
	if _, err := s.Remember("   ", KindProject, "", nil); err == nil {
		t.Error("an empty memory was stored")
	}
	if _, err := s.Remember("sesuatu", Kind("aneh"), "", nil); err == nil {
		t.Error("an unknown kind was stored")
	}
}

func TestInvalidIDsCannotEscapeTheStore(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"../../etc/passwd", "short", "", "lowercase0000000000000000a"} {
		if _, err := s.Load(id); err == nil {
			t.Errorf("%q was loadable", id)
		}
		if err := s.Forget(id); err == nil {
			t.Errorf("%q was forgettable", id)
		}
	}
}

// ---------- recall ----------

func TestRecallFindsRelevantMemories(t *testing.T) {
	s := newStore(t)
	remember(t, s, "Skrip deploy ada di scripts/ship.sh", KindProject, "deploy")
	remember(t, s, "Pengguna lebih suka jawaban singkat", KindUser)
	remember(t, s, "Basis data pakai postgres di port 5433", KindProject, "database")

	got, err := s.Recall("bagaimana cara deploy?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("recall returned nothing for a query that clearly matches")
	}
	if !strings.Contains(got[0].Text, "ship.sh") {
		t.Errorf("the deploy memory should rank first, got %q", got[0].Text)
	}
	for _, m := range got {
		if strings.Contains(m.Text, "postgres") {
			t.Error("an unrelated memory was recalled")
		}
	}
}

// TestRecallIsDeterministic is the property the whole design rests on: recalled
// memories go into the system prompt, so a recall that varied between runs
// would make every turn unreplayable for reasons unrelated to the agent.
func TestRecallIsDeterministic(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 20; i++ {
		remember(t, s, fmt.Sprintf("Catatan deploy nomor %d tentang ship", i), KindProject, "deploy")
	}

	first, err := s.Recall("deploy ship", 5)
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 25; run++ {
		got, err := s.Recall("deploy ship", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d memories, first run returned %d", run, len(got), len(first))
		}
		for i := range got {
			if got[i].ID != first[i].ID {
				t.Fatalf("run %d differs at position %d: %s vs %s", run, i, got[i].ID, first[i].ID)
			}
		}
	}
}

func TestPinnedMemoriesAlwaysComeBack(t *testing.T) {
	s := newStore(t)
	pinned := remember(t, s, "Selalu jawab dalam bahasa Indonesia", KindUser)
	if err := s.Pin(pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	remember(t, s, "Sesuatu tentang postgres", KindProject)

	got, err := s.Recall("pertanyaan yang tidak berhubungan sama sekali", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != pinned.ID {
		t.Fatalf("a pinned memory did not lead the recall: %v", got)
	}
}

func TestRecallRespectsTheLimit(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 30; i++ {
		remember(t, s, fmt.Sprintf("Catatan deploy %d", i), KindProject, "deploy")
	}
	got, err := s.Recall("deploy", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("recall returned %d memories, the limit was 4", len(got))
	}
}

// TestStopWordsDoNotMatchEverything guards the failure that makes lexical
// recall useless: without them, every memory matches every question.
func TestStopWordsDoNotMatchEverything(t *testing.T) {
	s := newStore(t)
	remember(t, s, "Ini adalah sesuatu yang tidak berhubungan dengan pertanyaan", KindProject)

	got, err := s.Recall("apa yang ada di dalam ini?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a query of only common words recalled %d memories", len(got))
	}
}

func TestUseCountNudgesRankingWithoutDominating(t *testing.T) {
	s := newStore(t)
	weak := remember(t, s, "Catatan deploy yang jarang dipakai", KindProject)
	strong := remember(t, s, "Skrip deploy ada di scripts/ship.sh untuk produksi", KindProject)

	// Make the weaker match heavily used; the better match should still win.
	for i := 0; i < 50; i++ {
		if err := s.RecordUse(weak.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Recall("deploy ship.sh produksi", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != strong.ID {
		t.Fatalf("use count outweighed relevance: %v", got)
	}
}

func TestRecordUseIgnoresMissingMemories(t *testing.T) {
	s := newStore(t)
	m := remember(t, s, "ada", KindProject)
	if err := s.RecordUse(m.ID, "01GONE"+strings.Repeat("0", 20)); err != nil {
		t.Fatalf("recording a use for a forgotten memory failed the whole call: %v", err)
	}
	got, _ := s.Load(m.ID)
	if got.UseCount != 1 {
		t.Errorf("use count is %d", got.UseCount)
	}
}

func TestPromptRendersNothingForNoMemories(t *testing.T) {
	if Prompt(nil) != "" {
		t.Error("an empty memory set produced prompt text")
	}
	out := Prompt([]*Memory{{Text: "fakta", Kind: KindUser, Tags: []string{"x"}}})
	if !strings.Contains(out, "fakta") || !strings.Contains(out, "(user)") {
		t.Errorf("prompt is %q", out)
	}
}

func TestListIsChronological(t *testing.T) {
	s := newStore(t)
	var want []string
	for i := 0; i < 5; i++ {
		want = append(want, remember(t, s, fmt.Sprintf("fakta %d", i), KindProject).ID)
	}
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("position %d is %s, want %s", i, got[i].ID, want[i])
		}
	}
}
