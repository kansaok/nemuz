package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
)

func newIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func turn(id, prompt, answer string, in, out int) Turn {
	return Turn{
		ID: id, StartedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		Model: "claude-opus-5", Prompt: prompt, Answer: answer,
		Steps: 2, ToolCalls: 1, InputTokens: in, OutputTokens: out,
	}
}

// ---------- search ----------

func TestSearchFindsTurnsByWordsInEitherField(t *testing.T) {
	ix := newIndex(t)
	if err := ix.Ingest(turn("01A", "bagaimana cara deploy ke staging?",
		"Jalankan scripts/ship.sh staging.", 100, 20), nil); err != nil {
		t.Fatal(err)
	}
	if err := ix.Ingest(turn("01B", "apa isi basis data?",
		"Postgres di port 5433.", 90, 15), nil); err != nil {
		t.Fatal(err)
	}

	// A word from the question.
	hits, err := ix.Search("deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Turn.ID != "01A" {
		t.Fatalf("searching the prompt returned %+v", hits)
	}

	// A word that only appears in the answer.
	hits, err = ix.Search("postgres", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Turn.ID != "01B" {
		t.Fatalf("searching the answer returned %+v", hits)
	}
}

func TestSearchRequiresEveryWord(t *testing.T) {
	ix := newIndex(t)
	ix.Ingest(turn("01A", "deploy ke staging", "ship.sh", 1, 1), nil)
	ix.Ingest(turn("01B", "deploy ke produksi", "release.sh", 1, 1), nil)

	hits, err := ix.Search("deploy staging", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Turn.ID != "01A" {
		t.Fatalf("a two-word search should narrow, got %+v", hits)
	}
}

// TestSearchTakesQuestionsNotQueryLanguage is the difference between a feature
// people use and one they give up on: FTS5's own syntax turns an ordinary
// question's punctuation into a parse error.
func TestSearchTakesQuestionsNotQueryLanguage(t *testing.T) {
	ix := newIndex(t)
	ix.Ingest(turn("01A", "what's the deploy script?", "scripts/ship.sh", 1, 1), nil)

	for _, query := range []string{
		"what's the deploy script?",
		`deploy "script"`,
		"deploy AND staging OR NEAR",
		"deploy-script",
		"(deploy)",
		"*",
		"deploy^",
	} {
		if _, err := ix.Search(query, 10); err != nil {
			t.Errorf("query %q failed to parse: %v", query, err)
		}
	}
}

func TestSnippetMarksTheMatch(t *testing.T) {
	ix := newIndex(t)
	ix.Ingest(turn("01A", "bagaimana cara deploy ke staging?", "Jalankan ship.sh.", 1, 1), nil)

	hits, err := ix.Search("deploy", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits are %+v (%v)", hits, err)
	}
	if !strings.Contains(hits[0].Snippet, "«deploy»") {
		t.Errorf("the snippet does not mark the match: %q", hits[0].Snippet)
	}
}

func TestSearchWithNoWordsReturnsNothing(t *testing.T) {
	ix := newIndex(t)
	ix.Ingest(turn("01A", "sesuatu", "apa saja", 1, 1), nil)

	hits, err := ix.Search("   ", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("an empty query matched %d turns", len(hits))
	}
}

// ---------- ingestion ----------

// TestReindexingReplacesRatherThanDuplicates is what makes a rebuild safe to
// run whenever, including twice by accident.
func TestReindexingReplacesRatherThanDuplicates(t *testing.T) {
	ix := newIndex(t)
	for i := 0; i < 3; i++ {
		if err := ix.Ingest(turn("01A", "deploy", "ship.sh", 10, 2),
			[]ToolCall{{Name: "read_file"}}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := ix.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the turn was indexed %d times", n)
	}
	hits, _ := ix.Search("deploy", 10)
	if len(hits) != 1 {
		t.Fatalf("search returned %d copies", len(hits))
	}
	report, _ := ix.Usage(time.Time{})
	if len(report.Tools) != 1 || report.Tools[0].Calls != 1 {
		t.Fatalf("tool calls were duplicated: %+v", report.Tools)
	}
}

// ---------- usage ----------

func TestUsageAggregatesByModelAndTool(t *testing.T) {
	ix := newIndex(t)
	a := turn("01A", "satu", "jawab", 100, 20)
	a.CachedTokens = 64
	ix.Ingest(a, []ToolCall{{Name: "read_file"}, {Name: "list_dir"}})

	b := turn("01B", "dua", "jawab", 200, 30)
	b.Model = "gpt-5"
	ix.Ingest(b, []ToolCall{{Name: "read_file", IsError: true}})

	report, err := ix.Usage(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Turns != 2 {
		t.Errorf("turns are %d", report.Turns)
	}
	if len(report.Models) != 2 {
		t.Fatalf("models are %+v", report.Models)
	}
	// Sorted by total tokens, largest first.
	if report.Models[0].Model != "gpt-5" || report.Models[0].Total() != 230 {
		t.Errorf("first model is %+v", report.Models[0])
	}

	in, out, cached := report.Totals()
	if in != 300 || out != 50 || cached != 64 {
		t.Errorf("totals are in=%d out=%d cached=%d", in, out, cached)
	}

	if len(report.Tools) != 2 || report.Tools[0].Name != "read_file" || report.Tools[0].Calls != 2 {
		t.Fatalf("tools are %+v", report.Tools)
	}
	if report.Tools[0].Failed != 1 {
		t.Errorf("failed calls are %d, want 1", report.Tools[0].Failed)
	}
}

// TestReviewTurnsAreCountedSeparately matters for reading a bill: a background
// review is nemuz spending tokens on its own behalf, not work anyone asked for.
func TestReviewTurnsAreCountedSeparately(t *testing.T) {
	ix := newIndex(t)
	ix.Ingest(turn("01A", "pertanyaan", "jawaban", 100, 20), nil)
	review := turn("01B", "review", "disimpan", 50, 10)
	review.Role = "review"
	ix.Ingest(review, nil)

	report, _ := ix.Usage(time.Time{})
	if report.Turns != 2 || report.Reviews != 1 {
		t.Fatalf("turns=%d reviews=%d", report.Turns, report.Reviews)
	}
}

func TestUsageRespectsTheCutoff(t *testing.T) {
	ix := newIndex(t)
	old := turn("01OLD", "lama", "jawab", 100, 20)
	old.StartedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ix.Ingest(old, nil)
	ix.Ingest(turn("01NEW", "baru", "jawab", 50, 10), nil)

	report, err := ix.Usage(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Turns != 1 {
		t.Fatalf("the cutoff included %d turns, want 1", report.Turns)
	}
	if in, _, _ := report.Totals(); in != 50 {
		t.Errorf("input tokens are %d, want only the recent turn's 50", in)
	}
}

func TestErroredTurnsAreCounted(t *testing.T) {
	ix := newIndex(t)
	bad := turn("01A", "gagal", "", 10, 0)
	bad.Errored = true
	ix.Ingest(bad, nil)
	ix.Ingest(turn("01B", "berhasil", "ok", 10, 5), nil)

	report, _ := ix.Usage(time.Time{})
	if report.Errored != 1 {
		t.Errorf("errored turns are %d", report.Errored)
	}
}

// ---------- rebuild from the journal ----------

// writeTurn records a real turn, so the rebuild path is exercised against
// journals the rest of nemuz actually produces.
func writeTurn(t *testing.T, dir string, bs *blob.Store, id, prompt, answer string, toolName string) {
	t.Helper()
	w, err := journal.Create(dir, id, bs)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	must := func(_ journal.Event, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(w.Append(journal.KindTurnStart, map[string]any{
		"prompt": prompt, "model": "claude-opus-5",
		"env": map[string]string{"sandbox": "landlock-v1"},
	}))
	if toolName != "" {
		must(w.Append(journal.KindToolCall, map[string]any{"id": "c1", "name": toolName}))
		must(w.Append(journal.KindToolResult, map[string]any{"id": "c1", "name": toolName, "is_error": false}))
	}
	must(w.Append(journal.KindModelResponse, map[string]any{
		"text":  answer,
		"usage": map[string]int{"input_tokens": 120, "output_tokens": 30, "cached_tokens": 16},
	}))
	must(w.Append(journal.KindTurnEnd, map[string]any{"steps": 2, "tool_calls": 1}))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildReadsEverythingFromTheJournal(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	bs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	writeTurn(t, journalDir, bs, "01AAAA0000000000000000000A", "bagaimana cara deploy?", "Jalankan ship.sh.", "read_file")
	writeTurn(t, journalDir, bs, "01BBBB0000000000000000000B", "apa isi basis data?", "Postgres.", "list_dir")

	ix := newIndex(t)
	indexed, skipped, err := ix.Rebuild(context.Background(), journalDir, bs)
	if err != nil {
		t.Fatal(err)
	}
	if indexed != 2 || len(skipped) != 0 {
		t.Fatalf("indexed %d, skipped %v", indexed, skipped)
	}

	hits, err := ix.Search("deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search after rebuild returned %d hits", len(hits))
	}
	got := hits[0].Turn
	if got.Model != "claude-opus-5" || got.Sandbox != "landlock-v1" {
		t.Errorf("metadata was lost: %+v", got)
	}
	if got.InputTokens != 120 || got.CachedTokens != 16 {
		t.Errorf("usage was lost: %+v", got)
	}
	if got.Answer != "Jalankan ship.sh." {
		t.Errorf("answer is %q", got.Answer)
	}

	report, _ := ix.Usage(time.Time{})
	if len(report.Tools) != 2 {
		t.Errorf("tools are %+v", report.Tools)
	}
}

// TestRebuildSkipsBrokenJournalsAndSaysWhich keeps one damaged file from
// costing the whole history.
func TestRebuildSkipsBrokenJournalsAndSaysWhich(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	bs, _ := blob.Open(filepath.Join(dir, "blobs"))
	writeTurn(t, journalDir, bs, "01GOOD0000000000000000000A", "pertanyaan", "jawaban", "")

	// A journal with a sequence gap: the reader refuses it, as it should.
	broken := "{\"seq\":1,\"kind\":\"turn.start\",\"payload\":{}}\n{\"seq\":9,\"kind\":\"turn.end\",\"payload\":{}}\n"
	if err := os.WriteFile(filepath.Join(journalDir, "01BAD00000000000000000000B.jsonl"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := newIndex(t)
	indexed, skipped, err := ix.Rebuild(context.Background(), journalDir, bs)
	if err != nil {
		t.Fatalf("one broken journal aborted the whole rebuild: %v", err)
	}
	if indexed != 1 {
		t.Errorf("indexed %d turns, want the one good journal", indexed)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "01BAD") {
		t.Fatalf("the broken journal was not named: %v", skipped)
	}
}

func TestRebuildReplacesWhatWasThere(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	bs, _ := blob.Open(filepath.Join(dir, "blobs"))
	writeTurn(t, journalDir, bs, "01AAAA0000000000000000000A", "pertanyaan", "jawaban", "")

	ix := newIndex(t)
	// A turn that is not in the journal at all: a rebuild must clear it.
	ix.Ingest(turn("01HANTU", "hantu", "sisa lama", 1, 1), nil)

	if _, _, err := ix.Rebuild(context.Background(), journalDir, bs); err != nil {
		t.Fatal(err)
	}
	if has, _ := ix.Has("01HANTU"); has {
		t.Fatal("a turn with no journal survived the rebuild")
	}
	n, _ := ix.Count()
	if n != 1 {
		t.Fatalf("the index holds %d turns after rebuild", n)
	}
}

// ---------- schema ----------

// TestAnOldSchemaIsDroppedNotMigrated is safe precisely because the index is
// derived: there is nothing in it to lose.
func TestAnOldSchemaIsDroppedNotMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ix.Ingest(turn("01A", "sesuatu", "jawab", 1, 1), nil)
	if _, err := ix.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("an index with an old schema could not be opened: %v", err)
	}
	defer reopened.Close()

	n, err := reopened.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the old schema was kept: %d turns survived", n)
	}
}

func TestIndexSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	ix, _ := Open(path)
	ix.Ingest(turn("01A", "deploy staging", "ship.sh", 1, 1), nil)
	ix.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	hits, err := reopened.Search("deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("the index did not survive being reopened: %d hits", len(hits))
	}
}

func TestRemoveDeletesTheIndexAndItsSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	ix, _ := Open(path)
	ix.Ingest(turn("01A", "sesuatu", "jawab", 1, 1), nil)
	ix.Close()

	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Errorf("%s survived removal", path+suffix)
		}
	}
	// Removing an absent index is not an error.
	if err := Remove(path); err != nil {
		t.Errorf("removing twice failed: %v", err)
	}
}

func TestRecentIsNewestFirst(t *testing.T) {
	ix := newIndex(t)
	for i, id := range []string{"01A", "01B", "01C"} {
		tn := turn(id, "pertanyaan "+id, "jawaban", 1, 1)
		tn.StartedAt = time.Date(2026, 9, 8, 12, i, 0, 0, time.UTC)
		ix.Ingest(tn, nil)
	}

	got, err := ix.Recent(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "01C" || got[1].ID != "01B" {
		t.Fatalf("recent turns are %+v", got)
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	tn := Turn{Prompt: "  baris pertama\n\n  baris kedua  "}
	if got := tn.OneLine(100); got != "baris pertama baris kedua" {
		t.Errorf("got %q", got)
	}
	if got := tn.OneLine(10); got != "baris pert…" {
		t.Errorf("got %q", got)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("an index with no path was opened")
	}
}

// TestSearchExcludesTheAgentsOwnReviews keeps history searchable: every
// reviewed turn has a review quoting it back, so including them would roughly
// double the results while adding nothing the original turn did not say.
func TestSearchExcludesTheAgentsOwnReviews(t *testing.T) {
	ix := newIndex(t)
	ix.Ingest(turn("01A", "bagaimana cara deploy?", "Jalankan ship.sh.", 1, 1), nil)
	review := turn("01B", "The user asked: bagaimana cara deploy?", "Tidak ada yang disimpan.", 1, 1)
	review.Role = "review"
	ix.Ingest(review, nil)

	hits, err := ix.Search("deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Turn.ID != "01A" {
		t.Fatalf("search returned %d hits, want only the conversation: %+v", len(hits), hits)
	}

	all, err := ix.SearchAll("deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("SearchAll returned %d hits, want both", len(all))
	}
}
