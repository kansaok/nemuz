// Package journal implements nemuz's append-only turn journal.
//
// Every turn writes each thing that happens — the model request, the response,
// each tool call and its result — to a JSONL file before anything else observes
// it. Payloads larger than SpillThreshold are written to the blob store and
// referenced by hash instead of being inlined.
//
// The journal is what makes replay possible: a recorded turn can be executed
// again against its recorded model responses, producing an identical event
// stream. Everything else that distinguishes nemuz — the eval gate for learned
// skills, regression tests that cost no API calls, the audit trail — is built
// on that one property.
package journal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
)

// Kind identifies what a journal event records.
type Kind string

const (
	KindTurnStart     Kind = "turn.start"
	KindModelRequest  Kind = "model.request"
	KindModelResponse Kind = "model.response"
	KindToolCall      Kind = "tool.call"
	KindToolResult    Kind = "tool.result"
	KindNote          Kind = "note"
	KindError         Kind = "error"
	KindTurnEnd       Kind = "turn.end"
)

// SpillThreshold is the payload size above which content moves to the blob
// store. Below it, inlining keeps the journal readable with plain `cat`.
const SpillThreshold = 4 << 10 // 4 KiB

// Event is one record in a turn's journal.
//
// Exactly one of Payload and Blob is set. Blob refs are themselves content
// hashes, so a spilled payload contributes to the turn digest just as precisely
// as an inline one.
type Event struct {
	Seq     int64           `json:"seq"`
	TurnID  string          `json:"turn"`
	Kind    Kind            `json:"kind"`
	TS      time.Time       `json:"ts"`
	Size    int             `json:"size"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Blob    blob.Ref        `json:"blob,omitempty"`
}

// Spilled reports whether the event's payload lives in the blob store.
func (e Event) Spilled() bool { return e.Blob != "" }

// Content returns the event's payload, fetching it from bs when spilled.
func (e Event) Content(bs *blob.Store) ([]byte, error) {
	if !e.Spilled() {
		return e.Payload, nil
	}
	if bs == nil {
		return nil, fmt.Errorf("journal: event %d is spilled but no blob store given", e.Seq)
	}
	return bs.GetBytes(e.Blob)
}

// digestInto folds the event's identity into h.
//
// Wall-clock time and payload size are deliberately excluded: two runs of the
// same turn differ in timing but are semantically identical, and a digest that
// changed with the clock would be useless for verifying a replay.
func (e Event) digestInto(h interface{ Write([]byte) (int, error) }) {
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(e.Seq))
	h.Write(seq[:])
	h.Write([]byte(e.Kind))
	h.Write([]byte{0})
	if e.Spilled() {
		// The ref is the SHA-256 of the content, so hashing it is equivalent
		// to hashing the content itself.
		h.Write([]byte("blob:"))
		h.Write([]byte(e.Blob))
	} else {
		h.Write([]byte("inline:"))
		h.Write(e.Payload)
	}
	h.Write([]byte{0})
}

// Digest returns a stable hash over a sequence of events.
//
// Two runs of the same turn produce the same digest if and only if they made
// the same decisions in the same order. This is the assertion that replay tests
// are built on.
func Digest(events []Event) string {
	h := sha256.New()
	for _, e := range events {
		e.digestInto(h)
	}
	return hex.EncodeToString(h.Sum(nil))
}
