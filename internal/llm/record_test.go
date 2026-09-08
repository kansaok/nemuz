package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/journal"
)

func newWriter(t *testing.T, id string) (*journal.Writer, *blob.Store) {
	t.Helper()
	dir := t.TempDir()
	bs, err := blob.Open(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	w, err := journal.Create(dir+"/journal", id, bs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, bs
}

func sampleRequest() Request {
	return Request{
		Model:    "claude-opus-5",
		System:   "ringkas",
		Messages: []Message{{Role: RoleUser, Text: "halo"}},
	}
}

// TestRecorderAndPlayerJournalIdentically is the property replay depends on:
// the two must write the same events, or a replayed turn could never match its
// recording no matter how correct the loop was.
func TestRecorderAndPlayerJournalIdentically(t *testing.T) {
	recW, bs := newWriter(t, "01RECORD000000000000000000")
	want := Response{Text: "hai", StopReason: StopEnd, Usage: Usage{InputTokens: 9, OutputTokens: 3}}

	rec := Record(&Static{Responses: []Response{want}}, recW)
	got, err := rec.Complete(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorder altered the response: %+v", got)
	}
	if err := recW.Close(); err != nil {
		t.Fatal(err)
	}

	recorded, err := journal.Read(recW.Path())
	if err != nil {
		t.Fatal(err)
	}
	cassette, err := journal.CassetteFrom(recorded, bs)
	if err != nil {
		t.Fatal(err)
	}

	repW, _ := newWriter(t, "01REPLAY000000000000000000")
	replayed, err := Replay(cassette, repW).Complete(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, want) {
		t.Fatalf("player altered the response: %+v", replayed)
	}
	if err := repW.Close(); err != nil {
		t.Fatal(err)
	}

	replayedEvents, err := journal.Read(repW.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Verify(recorded, replayedEvents); err != nil {
		t.Fatalf("recorder and player wrote different events: %v", err)
	}
}

func TestPlayerRejectsChangedRequest(t *testing.T) {
	recW, bs := newWriter(t, "01BASE0000000000000000000A")
	rec := Record(&Static{Responses: []Response{{Text: "hai", StopReason: StopEnd}}}, recW)
	if _, err := rec.Complete(context.Background(), sampleRequest()); err != nil {
		t.Fatal(err)
	}
	recW.Close()

	recorded, _ := journal.Read(recW.Path())
	cassette, err := journal.CassetteFrom(recorded, bs)
	if err != nil {
		t.Fatal(err)
	}

	changed := sampleRequest()
	changed.Messages[0].Text = "pertanyaan lain"

	repW, _ := newWriter(t, "01DIVERGE000000000000000A")
	_, err = Replay(cassette, repW).Complete(context.Background(), changed)
	var mismatch *RequestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("want *RequestMismatch, got %T: %v", err, err)
	}
}

// TestLenientPlayerAllowsChangedRequest covers deliberate divergence: replaying
// a recorded turn against a modified prompt to see what the model would face.
func TestLenientPlayerAllowsChangedRequest(t *testing.T) {
	recW, bs := newWriter(t, "01LENIENT00000000000000AA")
	rec := Record(&Static{Responses: []Response{{Text: "hai", StopReason: StopEnd}}}, recW)
	if _, err := rec.Complete(context.Background(), sampleRequest()); err != nil {
		t.Fatal(err)
	}
	recW.Close()

	recorded, _ := journal.Read(recW.Path())
	cassette, _ := journal.CassetteFrom(recorded, bs)

	changed := sampleRequest()
	changed.Messages[0].Text = "pertanyaan lain"

	repW, _ := newWriter(t, "01LENIENTB0000000000000AA")
	resp, err := Replay(cassette, repW).Lenient().Complete(context.Background(), changed)
	if err != nil {
		t.Fatalf("lenient replay rejected a changed request: %v", err)
	}
	if resp.Text != "hai" {
		t.Errorf("got %q", resp.Text)
	}
}

func TestRecorderJournalsProviderFailures(t *testing.T) {
	w, _ := newWriter(t, "01FAILURE00000000000000AA")
	// An empty script fails on the first call.
	_, err := Record(&Static{}, w).Complete(context.Background(), sampleRequest())
	if !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("want the provider error to surface, got %v", err)
	}
	w.Close()

	events, err := journal.Read(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []journal.Kind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []journal.Kind{journal.KindModelRequest, journal.KindError}
	if len(kinds) != 2 || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Fatalf("journal holds %v, want %v — a failed turn must still be inspectable", kinds, want)
	}
}

func TestValidateRejectsUnusableResponses(t *testing.T) {
	if err := (Response{}).Validate(); !errors.Is(err, ErrNoResponse) {
		t.Errorf("an empty response was accepted: %v", err)
	}
	missingID := Response{ToolCalls: []ToolCall{{Name: "read", Args: json.RawMessage(`{}`)}}}
	if err := missingID.Validate(); err == nil {
		t.Error("a tool call with no id was accepted")
	}
	missingName := Response{ToolCalls: []ToolCall{{ID: "c1", Args: json.RawMessage(`{}`)}}}
	if err := missingName.Validate(); err == nil {
		t.Error("a tool call with no name was accepted")
	}
	ok := Response{Text: "hai", StopReason: StopEnd}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid response was rejected: %v", err)
	}
}

func TestWantsToolsDrivesTheLoop(t *testing.T) {
	if (Response{Text: "selesai", StopReason: StopEnd}).WantsTools() {
		t.Error("a plain answer reported that it wants tools")
	}
	withTools := Response{ToolCalls: []ToolCall{{ID: "c1", Name: "read"}}, StopReason: StopToolUse}
	if !withTools.WantsTools() {
		t.Error("a response with tool calls reported that it does not want tools")
	}
}
