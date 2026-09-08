package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/tool"
)

// fixture is one prepared turn environment: a workspace with real files, real
// tools operating on it, and a journal to record into.
type fixture struct {
	dir       string
	journals  string
	blobs     *blob.Store
	tools     *tool.Registry
	workspace *tool.Workspace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()

	work := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(work, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"README.md":       "# proyek\n\nBaris pertama.\n",
		"docs/catatan.md": "catatan penting\n",
	} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ws, err := tool.NewWorkspace(work)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	if err := reg.Register(tool.NewReadFile(ws), tool.NewWriteFile(ws), tool.NewListDir(ws)); err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, journals: filepath.Join(dir, "journal"), blobs: bs, tools: reg, workspace: ws}
}

func (f *fixture) writer(t *testing.T, turnID string) *journal.Writer {
	t.Helper()
	w, err := journal.Create(f.journals, turnID, f.blobs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// script drives the model side: list the workspace, read a file, then answer.
func script(t *testing.T) []llm.Response {
	t.Helper()
	return []llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "list_dir", Args: args(t, map[string]string{"path": "."})},
			},
			Usage: llm.Usage{InputTokens: 120, OutputTokens: 18},
		},
		{
			StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{
				{ID: "c2", Name: "read_file", Args: args(t, map[string]string{"path": "README.md"})},
			},
			Usage: llm.Usage{InputTokens: 160, OutputTokens: 20},
		},
		{
			Text:       "Proyek ini punya README dan satu catatan di docs/.",
			StopReason: llm.StopEnd,
			Usage:      llm.Usage{InputTokens: 210, OutputTokens: 34},
		},
	}
}

func (f *fixture) agent(w *journal.Writer, p llm.Provider) *Agent {
	return &Agent{
		Provider: p,
		Tools:    f.tools,
		Journal:  w,
		Model:    "claude-opus-5",
		System:   "Kamu asisten yang ringkas.",
	}
}

const prompt = "apa isi workspace ini?"

// TestReplayOfRealTurnIsIdentical is the milestone assertion: a turn driven by
// real tools over a real workspace, recorded once and replayed, produces the
// same events down to the digest.
func TestReplayOfRealTurnIsIdentical(t *testing.T) {
	f := newFixture(t)

	recW := f.writer(t, "01RECORD000000000000000000")
	provider := &llm.Static{Responses: script(t), Label: "demo"}
	recorded, err := f.agent(recW, llm.Record(provider, recW)).Run(context.Background(), prompt)
	if err != nil {
		t.Fatalf("recording the turn failed: %v", err)
	}
	if err := recW.Close(); err != nil {
		t.Fatal(err)
	}
	if recorded.ToolCalls != 2 {
		t.Fatalf("the turn made %d tool calls, the script asks for 2", recorded.ToolCalls)
	}
	if recorded.Text == "" {
		t.Fatal("the turn produced no final answer")
	}

	recordedEvents, err := journal.Read(recW.Path())
	if err != nil {
		t.Fatal(err)
	}
	cassette, err := journal.CassetteFrom(recordedEvents, f.blobs)
	if err != nil {
		t.Fatal(err)
	}

	repW := f.writer(t, "01REPLAY000000000000000000")
	replayed, err := f.agent(repW, llm.Replay(cassette, repW)).Run(context.Background(), prompt)
	if err != nil {
		t.Fatalf("replaying the turn failed: %v", err)
	}
	if err := repW.Close(); err != nil {
		t.Fatal(err)
	}

	replayedEvents, err := journal.Read(repW.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Verify(recordedEvents, replayedEvents); err != nil {
		t.Fatalf("replay did not reproduce the recording: %v", err)
	}
	if a, b := journal.Digest(recordedEvents), journal.Digest(replayedEvents); a != b {
		t.Fatalf("digests differ:\n  recorded %s\n  replayed %s", a, b)
	}
	if replayed.Text != recorded.Text {
		t.Errorf("replayed answer differs:\n  recorded %q\n  replayed %q", recorded.Text, replayed.Text)
	}
	if replayed.Usage != recorded.Usage {
		t.Errorf("replayed usage %+v differs from recorded %+v", replayed.Usage, recorded.Usage)
	}
	if cassette.Remaining() != 0 {
		t.Errorf("%d recorded model calls were never replayed", cassette.Remaining())
	}
}

// TestReplayRejectsChangedPrompt proves replay fails loudly and early when the
// run diverges, instead of serving an answer to a question nobody asked.
func TestReplayRejectsChangedPrompt(t *testing.T) {
	f := newFixture(t)

	recW := f.writer(t, "01BASE0000000000000000000A")
	provider := &llm.Static{Responses: script(t)}
	if _, err := f.agent(recW, llm.Record(provider, recW)).Run(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}
	recW.Close()

	cassette, err := journal.LoadCassette(recW.Path(), f.blobs)
	if err != nil {
		t.Fatal(err)
	}

	repW := f.writer(t, "01DIVERGE000000000000000A")
	_, err = f.agent(repW, llm.Replay(cassette, repW)).Run(context.Background(), "pertanyaan yang berbeda")
	if err == nil {
		t.Fatal("replaying with a different prompt was accepted")
	}
	var mismatch *llm.RequestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("want a *llm.RequestMismatch naming the divergence, got %T: %v", err, err)
	}
	if mismatch.Call != 0 {
		t.Errorf("divergence reported at call %d, want the first one", mismatch.Call)
	}
}

func TestUnknownToolIsReportedToTheModel(t *testing.T) {
	f := newFixture(t)
	w := f.writer(t, "01UNKNOWN00000000000000AA")

	provider := &llm.Static{Responses: []llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "delete_everything", Args: args(t, map[string]string{})}},
		},
		{Text: "Baik, tool itu tidak tersedia.", StopReason: llm.StopEnd},
	}}

	out, err := f.agent(w, llm.Record(provider, w)).Run(context.Background(), prompt)
	if err != nil {
		t.Fatalf("an unknown tool aborted the turn instead of being reported: %v", err)
	}
	if out.Steps != 2 {
		t.Errorf("turn took %d steps, want 2 — the model should get a chance to recover", out.Steps)
	}
	w.Close()

	events, err := journal.Read(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for _, e := range events {
		if e.Kind != journal.KindToolResult {
			continue
		}
		var payload struct {
			IsError bool   `json:"is_error"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.IsError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("the journal has no failed tool result for the unknown tool")
	}
}

func TestStepLimitStopsARunawayLoop(t *testing.T) {
	f := newFixture(t)
	w := f.writer(t, "01RUNAWAY00000000000000AA")

	// A model that asks for the same tool forever.
	loop := make([]llm.Response, 6)
	for i := range loop {
		loop[i] = llm.Response{
			StopReason: llm.StopToolUse,
			ToolCalls:  []llm.ToolCall{{ID: "c", Name: "list_dir", Args: args(t, map[string]string{"path": "."})}},
		}
	}
	a := f.agent(w, llm.Record(&llm.Static{Responses: loop}, w))
	a.MaxSteps = 3

	_, err := a.Run(context.Background(), prompt)
	if !errors.Is(err, ErrStepLimit) {
		t.Fatalf("want ErrStepLimit, got %v", err)
	}
}

func TestAgentRefusesIncompleteConfiguration(t *testing.T) {
	f := newFixture(t)
	w := f.writer(t, "01BADCONFIG0000000000000A")

	for name, a := range map[string]*Agent{
		"no provider": {Tools: f.tools, Journal: w, Model: "m"},
		"no tools":    {Provider: &llm.Static{}, Journal: w, Model: "m"},
		"no journal":  {Provider: &llm.Static{}, Tools: f.tools, Model: "m"},
		"no model":    {Provider: &llm.Static{}, Tools: f.tools, Journal: w},
	} {
		if _, err := a.Run(context.Background(), prompt); err == nil {
			t.Errorf("%s: an incomplete agent ran anyway", name)
		}
	}
}
