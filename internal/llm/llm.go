// Package llm defines the intermediate representation every provider is
// translated into, and the seam that replay substitutes.
//
// Providers disagree about almost everything — how tool calls are shaped, where
// system prompts go, what a stop reason is called. Translating each of them into
// one IR at the edge means the agent loop, the journal, and the tools never have
// to know which provider is behind them. It also means a recorded turn can be
// replayed against a different provider entirely.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Role identifies who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a model's request to run one tool.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Message is one entry in a conversation.
//
// A tool result carries ToolCallID so providers that pair results with calls by
// id can reconstruct the link, and those that rely on ordering can ignore it.
type Message struct {
	Role       Role       `json:"role"`
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

// ToolDef describes a tool to the model.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Request is one model call.
type Request struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// StopReason says why the model stopped generating.
type StopReason string

const (
	StopEnd       StopReason = "end"       // finished its answer
	StopToolUse   StopReason = "tool_use"  // wants tools run
	StopMaxTokens StopReason = "max_tokens"
)

// Usage reports token consumption, for budgets and cost accounting.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// Response is one model reply, already normalised.
type Response struct {
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	StopReason StopReason `json:"stop_reason"`
	Usage      Usage      `json:"usage"`
	Model      string     `json:"model,omitempty"`
}

// WantsTools reports whether the loop should run tools and call again.
func (r Response) WantsTools() bool { return len(r.ToolCalls) > 0 }

// Provider turns a Request into a Response.
//
// This is the only interface an adapter must satisfy, and the only one replay
// substitutes. Everything above it is provider-agnostic.
type Provider interface {
	// Name identifies the provider in journals and errors.
	Name() string
	// Complete performs one model call.
	Complete(ctx context.Context, req Request) (Response, error)
}

// ErrNoResponse means a provider returned neither text nor tool calls, which
// would stall the agent loop.
var ErrNoResponse = errors.New("llm: provider returned an empty response")

// Validate reports whether a response can drive the loop forward.
func (r Response) Validate() error {
	if r.Text == "" && len(r.ToolCalls) == 0 {
		return ErrNoResponse
	}
	for i, tc := range r.ToolCalls {
		if tc.Name == "" {
			return fmt.Errorf("llm: tool call %d has no name", i)
		}
		if tc.ID == "" {
			return fmt.Errorf("llm: tool call %d (%s) has no id", i, tc.Name)
		}
	}
	return nil
}
