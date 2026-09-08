package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
)

// Writer appends events for a single turn to a JSONL file.
//
// A Writer is safe for concurrent use: tool calls run in parallel, and their
// results must land in the journal without interleaving.
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	bs     *blob.Store
	turnID string
	seq    int64
	events []Event
	closed bool

	// now is swappable so tests can produce fixed timestamps.
	now func() time.Time
}

// Create opens a journal file for turnID under dir, failing if one already
// exists. A turn is written exactly once; overwriting would destroy the record
// the journal exists to keep.
func Create(dir, turnID string, bs *blob.Store) (*Writer, error) {
	if turnID == "" {
		return nil, errors.New("journal: empty turn id")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: create dir: %w", err)
	}
	path := filepath.Join(dir, turnID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: create %s: %w", path, err)
	}
	return &Writer{
		f:      f,
		bw:     bufio.NewWriter(f),
		bs:     bs,
		turnID: turnID,
		now:    time.Now,
	}, nil
}

// TurnID returns the turn this writer records.
func (w *Writer) TurnID() string { return w.turnID }

// Path returns the journal file's location.
func (w *Writer) Path() string { return w.f.Name() }

// SetClock replaces the writer's time source. Tests use it to make journals
// byte-comparable; production code should not call it.
func (w *Writer) SetClock(now func() time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = now
}

// Append records one event and returns it as written.
//
// Payloads at or above SpillThreshold move to the blob store; the event then
// carries the content hash instead of the bytes.
func (w *Writer) Append(kind Kind, payload any) (Event, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("journal: marshal %s payload: %w", kind, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Event{}, errors.New("journal: writer is closed")
	}

	w.seq++
	ev := Event{
		Seq:    w.seq,
		TurnID: w.turnID,
		Kind:   kind,
		TS:     w.now().UTC(),
		Size:   len(body),
	}

	if len(body) >= SpillThreshold && w.bs != nil {
		ref, err := w.bs.PutBytes(body)
		if err != nil {
			return Event{}, fmt.Errorf("journal: spill %s payload: %w", kind, err)
		}
		ev.Blob = ref
	} else {
		ev.Payload = body
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("journal: marshal event: %w", err)
	}
	if _, err := w.bw.Write(append(line, '\n')); err != nil {
		return Event{}, fmt.Errorf("journal: append: %w", err)
	}
	w.events = append(w.events, ev)
	return ev, nil
}

// Events returns the events written so far.
func (w *Writer) Events() []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Event, len(w.events))
	copy(out, w.events)
	return out
}

// Digest returns the hash of everything written so far.
func (w *Writer) Digest() string { return Digest(w.Events()) }

// Sync flushes buffered events to disk. Call it at turn boundaries so a crash
// loses at most the turn in flight.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sync()
}

func (w *Writer) sync() error {
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("journal: flush: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("journal: sync: %w", err)
	}
	return nil
}

// Close flushes and closes the journal file. It is safe to call twice.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.sync(); err != nil {
		w.f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("journal: close: %w", err)
	}
	return nil
}
