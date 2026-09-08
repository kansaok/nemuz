package llm

import (
	"context"
	"errors"
	"fmt"
)

// Static is a Provider that returns a fixed script of responses.
//
// It exists so the agent loop, the tools, and the journal can be exercised
// end-to-end without a network or an API key — including in CI, where egress is
// blocked precisely to prove the test suite does not need it.
type Static struct {
	// Responses are returned in order, one per call.
	Responses []Response
	// Label distinguishes several static providers in a journal.
	Label string

	at int
}

// ErrScriptExhausted means the loop called the model more times than the script
// provides for.
var ErrScriptExhausted = errors.New("llm: static script exhausted")

// Name identifies the provider.
func (s *Static) Name() string {
	if s.Label == "" {
		return "static"
	}
	return "static:" + s.Label
}

// Calls reports how many times Complete has been called.
func (s *Static) Calls() int { return s.at }

// Complete returns the next scripted response.
func (s *Static) Complete(_ context.Context, _ Request) (Response, error) {
	if s.at >= len(s.Responses) {
		return Response{}, fmt.Errorf("%w after %d calls", ErrScriptExhausted, s.at)
	}
	r := s.Responses[s.at]
	s.at++
	return r, nil
}

// Reset rewinds the script.
func (s *Static) Reset() { s.at = 0 }
