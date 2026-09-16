package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
)

func TestRedactTurnReplacesSpilledValueAndMarksTurn(t *testing.T) {
	paths := config.At(t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	bs, err := blob.Open(paths.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	w, err := journal.Create(paths.Journal, "redact", bs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(journal.KindToolResult, map[string]string{"token": "secret", "body": strings.Repeat("x", journal.SpillThreshold)}); err != nil {
		t.Fatal(err)
	}
	old := w.Events()[0].Blob
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	turn, err := journal.Find(paths.Journal, "redact")
	if err != nil {
		t.Fatal(err)
	}
	n, err := redactTurn(turn, bs, []string{"token"})
	if err != nil || n != 1 {
		t.Fatalf("redact = %d, %v", n, err)
	}
	if err := os.WriteFile(journal.RedactionPath(turn.Path), []byte("redacted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !journal.IsRedacted(turn) {
		t.Fatal("missing redaction marker")
	}
	events, err := journal.Read(turn.Path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := events[0].Content(bs)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "[REDACTED]" || strings.Contains(string(body), "secret") {
		t.Fatalf("redacted payload = %s", body)
	}
	if events[0].Blob == old {
		t.Fatal("redaction retained original blob reference")
	}
	if _, err := os.Stat(filepath.Join(paths.Journal, "redact.jsonl")); err != nil {
		t.Fatal(err)
	}
}
