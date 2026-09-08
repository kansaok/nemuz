package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
)

// The request and response shapes below are OpenAI's, not nemuz's. They exist
// so an existing client needs no changes; the IR nemuz actually works in lives
// in internal/llm.

type completionRequest struct {
	Model    string              `json:"model"`
	Messages []completionMessage `json:"messages"`
	Stream   bool                `json:"stream"`
}

type completionMessage struct {
	Role string `json:"role"`
	// Content is a string in the common case, but the API also allows an
	// array of typed parts. Both are accepted; see text().
	Content json.RawMessage `json:"content"`
}

// text extracts a message's text, accepting both shapes the API allows.
func (m completionMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var plain string
	if err := json.Unmarshal(m.Content, &plain); err == nil {
		return plain
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// prompt reduces a conversation to the turn nemuz should run.
//
// nemuz keeps its own history in the journal and its own facts in memory, so a
// client resending the whole conversation is describing state the agent already
// has. Taking the last user message is therefore right rather than lossy — and
// any earlier system message is folded in, because that is the one thing a
// client can say that the agent could not already know.
func (r completionRequest) prompt() (string, error) {
	var systemParts []string
	last := ""

	for _, m := range r.Messages {
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			if text := strings.TrimSpace(m.text()); text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			last = m.text()
		}
	}

	if strings.TrimSpace(last) == "" {
		return "", errors.New("the request has no user message to answer")
	}
	if len(systemParts) == 0 {
		return last, nil
	}
	return strings.Join(systemParts, "\n\n") + "\n\n" + last, nil
}

type completionResponse struct {
	// ID is the turn id, so any answer can be replayed with
	// `nemuz replay <id>`.
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	// Usage is omitted from streaming chunks, where a zeroed object would
	// read as a real measurement of nothing.
	Usage *completionUsage `json:"usage,omitempty"`
}

type completionChoice struct {
	Index        int              `json:"index"`
	Message      *responseMessage `json:"message,omitempty"`
	Delta        *responseMessage `json:"delta,omitempty"`
	FinishReason *string          `json:"finish_reason"`
}

type responseMessage struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type modelList struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// nowUnix is swappable so tests get stable timestamps.
var nowUnix = func() int64 { return timeNow().Unix() }

func buildCompletion(out agent.Outcome, model string) completionResponse {
	stop := "stop"
	return completionResponse{
		ID:      out.TurnID,
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      &responseMessage{Role: "assistant", Content: out.Text},
			FinishReason: &stop,
		}},
		Usage: &completionUsage{
			PromptTokens:     out.Usage.InputTokens,
			CompletionTokens: out.Usage.OutputTokens,
			TotalTokens:      out.Usage.InputTokens + out.Usage.OutputTokens,
		},
	}
}

// streamCompletion sends a finished answer in the server-sent-event form
// clients expect for `stream: true`.
//
// The answer is already complete by the time this runs, so it goes out as one
// content chunk. Clients work; nothing arrives gradually, and the /health
// endpoint says so rather than letting anyone infer otherwise.
func (s *Server) streamCompletion(w http.ResponseWriter, out agent.Outcome) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusOK, buildCompletion(out, s.opts.Model))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	created := nowUnix()
	base := completionResponse{
		ID: out.TurnID, Object: "chat.completion.chunk",
		Created: created, Model: s.opts.Model,
	}

	// The role arrives first, then the content, then the terminator. Clients
	// rely on that order even when everything is available at once.
	role := base
	role.Choices = []completionChoice{{Index: 0, Delta: &responseMessage{Role: "assistant"}}}
	sendEvent(w, flusher, role)

	content := base
	content.Choices = []completionChoice{{Index: 0, Delta: &responseMessage{Content: out.Text}}}
	sendEvent(w, flusher, content)

	stop := "stop"
	final := base
	final.Choices = []completionChoice{{Index: 0, Delta: &responseMessage{}, FinishReason: &stop}}
	sendEvent(w, flusher, final)

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func sendEvent(w http.ResponseWriter, flusher http.Flusher, chunk completionResponse) {
	body, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", body)
	flusher.Flush()
}

// timeNow is the clock, named so tests can replace it.
var timeNow = time.Now
