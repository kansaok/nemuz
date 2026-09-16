package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
)

func TestPruneJournalsPreviewsThenRemovesOldTurnAndBlob(t *testing.T) {
	paths := config.At(t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	bs, err := blob.Open(paths.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	oldRef := writePruneTurn(t, paths, bs, "old", now.Add(-48*time.Hour))
	newRef := writePruneTurn(t, paths, bs, "new", now.Add(-time.Hour))

	preview, err := pruneJournals(paths, bs, now.Add(-24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(preview.Turns, ","); got != "old" {
		t.Fatalf("preview turns = %q, want old", got)
	}
	if !bs.Has(oldRef) {
		t.Fatal("preview removed the old blob")
	}

	report, err := pruneJournals(paths, bs, now.Add(-24*time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Turns) != 1 || report.Blobs != 1 {
		t.Fatalf("report = %+v, want one turn and blob", report)
	}
	if bs.Has(oldRef) || !bs.Has(newRef) {
		t.Fatalf("blob retention old=%v new=%v", bs.Has(oldRef), bs.Has(newRef))
	}
	turns, err := journal.List(paths.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].ID != "new" {
		t.Fatalf("remaining turns = %+v", turns)
	}
}

func writePruneTurn(t *testing.T, paths config.Paths, bs *blob.Store, id string, at time.Time) blob.Ref {
	t.Helper()
	w, err := journal.Create(paths.Journal, id, bs)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClock(func() time.Time { return at })
	if _, err := w.Append(journal.KindToolResult, map[string]string{"content": id + strings.Repeat("x", journal.SpillThreshold)}); err != nil {
		t.Fatal(err)
	}
	events := w.Events()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Spilled() {
		t.Fatal("test turn did not spill its payload")
	}
	return events[0].Blob
}
