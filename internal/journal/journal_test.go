package journal

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
)

type modelResponse struct {
	Text string `json:"text"`
}

// fixedClock makes journals byte-comparable across runs.
func fixedClock() func() time.Time {
	t := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func newTestWriter(t *testing.T) (*Writer, *blob.Store) {
	t.Helper()
	dir := t.TempDir()
	bs, err := blob.Open(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	w, err := Create(dir+"/journal", "01TESTTURN0000000000000000", bs)
	if err != nil {
		t.Fatal(err)
	}
	w.SetClock(fixedClock())
	t.Cleanup(func() { w.Close() })
	return w, bs
}

func TestWriteReadRoundTrip(t *testing.T) {
	w, _ := newTestWriter(t)

	if _, err := w.Append(KindTurnStart, map[string]string{"prompt": "halo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(KindModelResponse, modelResponse{Text: "hai"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(KindTurnEnd, map[string]int{"tools": 0}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Read(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d events, wrote 3", len(got))
	}
	for i, want := range []Kind{KindTurnStart, KindModelResponse, KindTurnEnd} {
		if got[i].Kind != want {
			t.Errorf("event %d: kind %q, want %q", i, got[i].Kind, want)
		}
		if got[i].Seq != int64(i+1) {
			t.Errorf("event %d: seq %d, want %d", i, got[i].Seq, i+1)
		}
	}
	if Digest(got) != w.Digest() {
		t.Fatal("digest changed between writing and reading back")
	}
}

func TestLargePayloadsSpillToBlobStore(t *testing.T) {
	w, bs := newTestWriter(t)
	big := strings.Repeat("x", SpillThreshold*2)

	ev, err := w.Append(KindModelResponse, modelResponse{Text: big})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Spilled() {
		t.Fatalf("payload of %d bytes stayed inline; threshold is %d", ev.Size, SpillThreshold)
	}
	if len(ev.Payload) != 0 {
		t.Error("spilled event still carries an inline payload")
	}

	body, err := ev.Content(bs)
	if err != nil {
		t.Fatal(err)
	}
	var got modelResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != big {
		t.Error("spilled content did not survive the round trip")
	}
}

func TestSmallPayloadsStayInline(t *testing.T) {
	w, _ := newTestWriter(t)

	ev, err := w.Append(KindNote, map[string]string{"msg": "singkat"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Spilled() {
		t.Fatal("a small payload was spilled to the blob store")
	}
}

// TestDigestIgnoresWallClock is the assertion replay depends on: two runs of
// the same turn differ in timing, and that must not count as a difference.
func TestDigestIgnoresWallClock(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: KindTurnStart, TS: time.Now(), Payload: json.RawMessage(`{"a":1}`)},
		{Seq: 2, Kind: KindTurnEnd, TS: time.Now(), Payload: json.RawMessage(`{"b":2}`)},
	}
	later := make([]Event, len(events))
	copy(later, events)
	for i := range later {
		later[i].TS = later[i].TS.Add(72 * time.Hour)
		later[i].Size = 999
	}

	if Digest(events) != Digest(later) {
		t.Fatal("digest changed when only timestamps and sizes changed")
	}
}

func TestDigestDetectsChangedPayload(t *testing.T) {
	base := []Event{{Seq: 1, Kind: KindToolCall, Payload: json.RawMessage(`{"tool":"read"}`)}}
	altered := []Event{{Seq: 1, Kind: KindToolCall, Payload: json.RawMessage(`{"tool":"write"}`)}}

	if Digest(base) == Digest(altered) {
		t.Fatal("digest did not change when the payload changed")
	}
}

func TestReadRejectsSequenceGap(t *testing.T) {
	corrupt := `{"seq":1,"kind":"turn.start","payload":{}}
{"seq":3,"kind":"turn.end","payload":{}}`

	_, err := ReadFrom(strings.NewReader(corrupt), "corrupt.jsonl")
	if err == nil {
		t.Fatal("a journal with a missing event was accepted")
	}
	if !strings.Contains(err.Error(), "sequence gap") {
		t.Fatalf("error should name the sequence gap, got: %v", err)
	}
}

func TestCreateRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, "01SAMETURN0000000000000000", nil)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	if _, err := Create(dir, "01SAMETURN0000000000000000", nil); err == nil {
		t.Fatal("an existing turn was silently reopened for overwriting")
	}
}

func TestTurnIDsSortChronologically(t *testing.T) {
	var ids []string
	for i := 0; i < 50; i++ {
		id, err := NewTurnID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 26 {
			t.Fatalf("turn id %q is %d chars, want 26", id, len(id))
		}
		ids = append(ids, id)
		time.Sleep(time.Millisecond)
	}

	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ids do not sort in creation order at index %d", i)
		}
	}

	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate turn id %q", id)
		}
		seen[id] = true
	}
}

func TestFindAcceptsUnambiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"01AAAA0000000000000000000A", "01BBBB0000000000000000000B"} {
		w, err := Create(dir, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
	}

	got, err := Find(dir, "01AA")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "01AAAA0000000000000000000A" {
		t.Fatalf("found %q, want the 01AA turn", got.ID)
	}
	if _, err := Find(dir, "01"); err == nil {
		t.Fatal("an ambiguous prefix was accepted")
	}
	if _, err := Find(dir, "99"); err == nil {
		t.Fatal("a prefix matching nothing was accepted")
	}
}
