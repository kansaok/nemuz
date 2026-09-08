package journal

import (
	"errors"
	"fmt"

	"github.com/kansaok/nemuz/internal/blob"
)

// Exchange is one recorded model call: what was asked, and what came back.
type Exchange struct {
	// Request is the recorded model.request payload. It is nil for turns
	// recorded before requests were journaled.
	Request []byte
	// Response is the recorded model.response payload, byte for byte.
	Response []byte
}

// Cassette serves the model exchanges recorded in a turn, in order.
//
// Replaying a turn means running the real agent loop with its real tools while
// the model adapter draws from a cassette instead of calling a provider. The
// loop cannot tell the difference, so any divergence in the resulting event
// stream is caused by nemuz itself — not by the model answering differently
// this time. That is what makes a failure reproducible.
type Cassette struct {
	turnID    string
	exchanges []Exchange
	cursor    int
}

// ErrCassetteExhausted means the replayed run asked the model more times than
// the recording did — the run diverged.
var ErrCassetteExhausted = errors.New("journal: cassette exhausted")

// LoadCassette reads the recorded exchanges out of a journal file.
func LoadCassette(path string, bs *blob.Store) (*Cassette, error) {
	events, err := Read(path)
	if err != nil {
		return nil, err
	}
	return CassetteFrom(events, bs)
}

// CassetteFrom builds a cassette from an already-parsed event stream.
//
// Requests and responses are paired by position: each response belongs to the
// most recent unmatched request. A response with no preceding request is kept
// with a nil Request rather than rejected, so journals written by older code
// remain replayable.
func CassetteFrom(events []Event, bs *blob.Store) (*Cassette, error) {
	c := &Cassette{}
	var pending []byte
	for _, ev := range events {
		if c.turnID == "" {
			c.turnID = ev.TurnID
		}
		switch ev.Kind {
		case KindModelRequest:
			body, err := ev.Content(bs)
			if err != nil {
				return nil, fmt.Errorf("journal: load request at seq %d: %w", ev.Seq, err)
			}
			pending = body
		case KindModelResponse:
			body, err := ev.Content(bs)
			if err != nil {
				return nil, fmt.Errorf("journal: load response at seq %d: %w", ev.Seq, err)
			}
			c.exchanges = append(c.exchanges, Exchange{Request: pending, Response: body})
			pending = nil
		}
	}
	if len(c.exchanges) == 0 {
		return nil, fmt.Errorf("journal: turn %s recorded no model responses", c.turnID)
	}
	return c, nil
}

// TurnID returns the turn the cassette was recorded from.
func (c *Cassette) TurnID() string { return c.turnID }

// Len returns how many exchanges were recorded.
func (c *Cassette) Len() int { return len(c.exchanges) }

// Remaining returns how many exchanges have not been served yet.
func (c *Cassette) Remaining() int { return len(c.exchanges) - c.cursor }

// Position returns the index of the next exchange, for error messages.
func (c *Cassette) Position() int { return c.cursor }

// Next returns the next recorded exchange.
func (c *Cassette) Next() (Exchange, error) {
	if c.cursor >= len(c.exchanges) {
		return Exchange{}, fmt.Errorf("%w: turn %s recorded %d model calls, replay wanted more",
			ErrCassetteExhausted, c.turnID, len(c.exchanges))
	}
	ex := c.exchanges[c.cursor]
	c.cursor++
	return ex, nil
}

// Rewind returns the cassette to its start so it can be played again.
func (c *Cassette) Rewind() { c.cursor = 0 }

// Divergence describes the first point at which a replay stopped matching its
// recording.
type Divergence struct {
	Seq      int64
	Reason   string
	Recorded string
	Replayed string
}

func (d *Divergence) Error() string {
	return fmt.Sprintf("replay diverged at event %d: %s\n  recorded: %s\n  replayed: %s",
		d.Seq, d.Reason, d.Recorded, d.Replayed)
}

// Verify compares a replayed event stream against its recording and reports the
// first divergence, if any.
//
// It returns a *Divergence rather than a bare error so callers can show the
// operator exactly which event went wrong instead of only that something did.
func Verify(recorded, replayed []Event) error {
	n := len(recorded)
	if len(replayed) < n {
		n = len(replayed)
	}
	for i := 0; i < n; i++ {
		r, p := recorded[i], replayed[i]
		if r.Kind != p.Kind {
			return &Divergence{
				Seq: r.Seq, Reason: "different event kind",
				Recorded: string(r.Kind), Replayed: string(p.Kind),
			}
		}
		if rd, pd := digestOne(r), digestOne(p); rd != pd {
			return &Divergence{
				Seq: r.Seq, Reason: fmt.Sprintf("%s payload differs", r.Kind),
				Recorded: rd, Replayed: pd,
			}
		}
	}
	switch {
	case len(replayed) < len(recorded):
		next := recorded[len(replayed)]
		return &Divergence{
			Seq: next.Seq, Reason: "replay stopped early",
			Recorded: string(next.Kind), Replayed: "(nothing)",
		}
	case len(replayed) > len(recorded):
		extra := replayed[len(recorded)]
		return &Divergence{
			Seq: extra.Seq, Reason: "replay produced extra events",
			Recorded: "(nothing)", Replayed: string(extra.Kind),
		}
	}
	return nil
}

func digestOne(e Event) string { return Digest([]Event{e})[:16] }
