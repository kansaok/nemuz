package journal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
)

// This file exercises the property nemuz is built around: a recorded turn can
// be run again against its recording and produce an identical event stream.
//
// The agent below is deliberately tiny, but it has the shape of the real one:
// it asks a model, may call a tool, feeds the result back, and stops when the
// model says it is done. Everything it does passes through the journal.

type reply struct {
	Tool string `json:"tool,omitempty"`
	Arg  string `json:"arg,omitempty"`
	Say  string `json:"say,omitempty"`
	Done bool   `json:"done"`
}

// model is the seam that replay swaps out. In production this is an HTTP call
// to a provider; under replay it is a cassette.
type model interface {
	complete(prompt string) ([]byte, error)
}

// scriptedModel stands in for a live provider during recording.
type scriptedModel struct {
	script []reply
	at     int
}

func (m *scriptedModel) complete(string) ([]byte, error) {
	if m.at >= len(m.script) {
		return nil, errors.New("script exhausted")
	}
	r := m.script[m.at]
	m.at++
	return json.Marshal(r)
}

// cassetteModel replays recorded responses instead of calling a provider.
type cassetteModel struct{ c *Cassette }

func (m *cassetteModel) complete(string) ([]byte, error) {
	ex, err := m.c.Next()
	if err != nil {
		return nil, err
	}
	return ex.Response, nil
}

// tools is the fake toolbox. upper is deterministic; clock is not, and is used
// to prove that Verify actually catches divergence.
var tools = map[string]func(string) string{
	"upper": strings.ToUpper,
	"clock": func(string) string { return time.Now().Format(time.RFC3339Nano) },
}

// runTurn is the shared agent loop. Recording and replaying differ only in
// which model is passed in — the journal, the tools, and the control flow are
// identical, so any difference in the output is nemuz's own doing.
func runTurn(t *testing.T, dir, turnID string, bs *blob.Store, m model) []Event {
	t.Helper()

	w, err := Create(dir, turnID, bs)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetClock(fixedClock())

	if _, err := w.Append(KindTurnStart, map[string]string{"prompt": "kerjakan"}); err != nil {
		t.Fatal(err)
	}

	prompt := "kerjakan"
	for step := 0; step < 10; step++ {
		if _, err := w.Append(KindModelRequest, map[string]string{"prompt": prompt}); err != nil {
			t.Fatal(err)
		}
		raw, err := m.complete(prompt)
		if err != nil {
			t.Fatalf("model call at step %d: %v", step, err)
		}
		// The response is journaled exactly as received. This is the byte
		// stream a later replay will be fed.
		if _, err := w.Append(KindModelResponse, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}

		var r reply
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		if r.Done {
			break
		}
		if _, err := w.Append(KindToolCall, map[string]string{"tool": r.Tool, "arg": r.Arg}); err != nil {
			t.Fatal(err)
		}
		out := tools[r.Tool](r.Arg)
		if _, err := w.Append(KindToolResult, map[string]string{"tool": r.Tool, "out": out}); err != nil {
			t.Fatal(err)
		}
		prompt = out
	}

	if _, err := w.Append(KindTurnEnd, map[string]bool{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.Events()
}

func newStore(t *testing.T) (string, *blob.Store) {
	t.Helper()
	dir := t.TempDir()
	bs, err := blob.Open(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	return dir + "/journal", bs
}

// TestReplayReproducesRecordedTurn is the headline claim: record once, replay
// later, get exactly the same events.
func TestReplayReproducesRecordedTurn(t *testing.T) {
	dir, bs := newStore(t)
	script := []reply{
		{Tool: "upper", Arg: "halo dunia"},
		{Tool: "upper", Arg: "sekali lagi"},
		{Say: "selesai", Done: true},
	}

	recorded := runTurn(t, dir, "01RECORD000000000000000000", bs, &scriptedModel{script: script})

	cass, err := CassetteFrom(recorded, bs)
	if err != nil {
		t.Fatal(err)
	}
	if cass.Len() != len(script) {
		t.Fatalf("cassette holds %d exchanges, recording made %d model calls", cass.Len(), len(script))
	}
	for i, ex := range cass.exchanges {
		if ex.Request == nil {
			t.Errorf("exchange %d lost its recorded request", i)
		}
	}

	replayed := runTurn(t, dir, "01REPLAY000000000000000000", bs, &cassetteModel{c: cass})

	if err := Verify(recorded, replayed); err != nil {
		t.Fatalf("replay did not reproduce the recording: %v", err)
	}
	if Digest(recorded) != Digest(replayed) {
		t.Fatal("digests differ even though Verify passed — the two disagree")
	}
	if cass.Remaining() != 0 {
		t.Errorf("%d recorded exchanges were never used by the replay", cass.Remaining())
	}
}

// TestVerifyCatchesNondeterministicTool guards against the failure mode that
// makes replay worthless: a green result that does not actually prove anything.
func TestVerifyCatchesNondeterministicTool(t *testing.T) {
	dir, bs := newStore(t)
	script := []reply{
		{Tool: "clock", Arg: ""},
		{Say: "selesai", Done: true},
	}

	recorded := runTurn(t, dir, "01CLOCKA00000000000000000A", bs, &scriptedModel{script: script})
	cass, err := CassetteFrom(recorded, bs)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	replayed := runTurn(t, dir, "01CLOCKB00000000000000000B", bs, &cassetteModel{c: cass})

	err = Verify(recorded, replayed)
	if err == nil {
		t.Fatal("Verify passed a turn whose tool returned a different value")
	}
	var d *Divergence
	if !errors.As(err, &d) {
		t.Fatalf("want a *Divergence naming the bad event, got %T: %v", err, err)
	}
	if d.Reason != "tool.result payload differs" {
		t.Errorf("divergence blamed %q; the clock tool result is what changed", d.Reason)
	}
}

func TestVerifyCatchesShortAndLongReplays(t *testing.T) {
	full := []Event{
		{Seq: 1, Kind: KindTurnStart, Payload: json.RawMessage(`{}`)},
		{Seq: 2, Kind: KindModelResponse, Payload: json.RawMessage(`{"done":true}`)},
		{Seq: 3, Kind: KindTurnEnd, Payload: json.RawMessage(`{}`)},
	}

	if err := Verify(full, full[:2]); err == nil {
		t.Error("a replay that stopped early was accepted")
	}
	if err := Verify(full[:2], full); err == nil {
		t.Error("a replay that produced extra events was accepted")
	}
	if err := Verify(full, full); err != nil {
		t.Errorf("an identical replay was rejected: %v", err)
	}
}

func TestCassetteExhaustionIsReported(t *testing.T) {
	c := &Cassette{turnID: "01X", exchanges: []Exchange{{Response: []byte(`{"done":true}`)}}}

	if _, err := c.Next(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Next()
	if !errors.Is(err, ErrCassetteExhausted) {
		t.Fatalf("want ErrCassetteExhausted when the replay asks for more, got %v", err)
	}

	c.Rewind()
	if c.Remaining() != 1 {
		t.Errorf("after Rewind the cassette has %d exchanges left, want 1", c.Remaining())
	}
}

func TestCassetteRejectsTurnWithoutResponses(t *testing.T) {
	events := []Event{{Seq: 1, Kind: KindTurnStart, Payload: json.RawMessage(`{}`)}}

	if _, err := CassetteFrom(events, nil); err == nil {
		t.Fatal("a turn with no model responses produced a usable cassette")
	}
}
