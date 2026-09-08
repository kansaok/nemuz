package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/kansaok/nemuz/internal/journal"
)

// Recorder wraps a Provider and journals every exchange it performs.
//
// Recording happens here rather than inside each adapter so that every
// provider — including ones written by third parties — is journaled the same
// way, and so an adapter cannot forget to do it.
type Recorder struct {
	inner Provider
	j     *journal.Writer
}

// Record returns p wrapped so that its calls are written to j.
func Record(p Provider, j *journal.Writer) *Recorder {
	return &Recorder{inner: p, j: j}
}

// Name reports the wrapped provider's name.
func (r *Recorder) Name() string { return r.inner.Name() }

// Complete journals the request, performs the call, and journals the result.
//
// A failed call is journaled too: a turn that ended in a provider error is
// still a turn worth being able to inspect.
func (r *Recorder) Complete(ctx context.Context, req Request) (Response, error) {
	if _, err := r.j.Append(journal.KindModelRequest, req); err != nil {
		return Response{}, err
	}
	resp, err := r.inner.Complete(ctx, req)
	if err != nil {
		if _, jerr := r.j.Append(journal.KindError, map[string]string{
			"stage":    "model.complete",
			"provider": r.inner.Name(),
			"error":    err.Error(),
		}); jerr != nil {
			return Response{}, jerr
		}
		return Response{}, err
	}
	if _, err := r.j.Append(journal.KindModelResponse, resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Player is a Provider that serves recorded exchanges instead of calling out.
//
// It journals exactly what Recorder journals, using the recorded response bytes
// verbatim. That is what lets a replayed turn produce an event stream identical
// to the original.
type Player struct {
	cassette *journal.Cassette
	j        *journal.Writer
	strict   bool
}

// Replay returns a Provider that plays c back into j.
//
// It is strict by default: if the replayed run builds a different request than
// the recording did, it stops at that point and says so, rather than serving a
// response that belongs to a different question.
func Replay(c *journal.Cassette, j *journal.Writer) *Player {
	return &Player{cassette: c, j: j, strict: true}
}

// Lenient disables request matching. Use it when replaying a turn against
// deliberately changed inputs — a prompt experiment, say — where divergence is
// the point rather than a bug.
func (p *Player) Lenient() *Player {
	p.strict = false
	return p
}

// Name identifies the player in journals and errors.
func (p *Player) Name() string { return "replay:" + p.cassette.TurnID() }

// Complete serves the next recorded response.
func (p *Player) Complete(_ context.Context, req Request) (Response, error) {
	if _, err := p.j.Append(journal.KindModelRequest, req); err != nil {
		return Response{}, err
	}

	pos := p.cassette.Position()
	ex, err := p.cassette.Next()
	if err != nil {
		return Response{}, err
	}

	if p.strict && ex.Request != nil {
		got, err := json.Marshal(req)
		if err != nil {
			return Response{}, fmt.Errorf("llm: marshal replayed request: %w", err)
		}
		if !bytes.Equal(got, ex.Request) {
			return Response{}, &RequestMismatch{Call: pos, Recorded: ex.Request, Replayed: got}
		}
	}

	// The recorded bytes are appended as-is. Re-encoding a decoded Response
	// would silently drop any field this build does not know about, and the
	// journal would no longer match the recording it came from.
	if _, err := p.j.Append(journal.KindModelResponse, json.RawMessage(ex.Response)); err != nil {
		return Response{}, err
	}

	var resp Response
	if err := json.Unmarshal(ex.Response, &resp); err != nil {
		return Response{}, fmt.Errorf("llm: decode recorded response %d: %w", pos, err)
	}
	return resp, nil
}

// RequestMismatch reports that a replay asked the model something the recording
// never asked.
type RequestMismatch struct {
	Call     int
	Recorded []byte
	Replayed []byte
}

func (e *RequestMismatch) Error() string {
	return fmt.Sprintf("llm: replay diverged before model call %d — the request differs from the recording\n  recorded: %s\n  replayed: %s",
		e.Call+1, truncate(e.Recorded, 200), truncate(e.Replayed, 200))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
