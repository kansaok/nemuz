package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kansaok/nemuz/internal/llm"
)

const (
	// AnthropicDefaultBaseURL is the public API root.
	AnthropicDefaultBaseURL = "https://api.anthropic.com/v1"
	// anthropicVersion is the API version header this adapter is written against.
	anthropicVersion = "2023-06-01"
	// anthropicDefaultMaxTokens is used when a request does not set a limit.
	// The Messages API requires one, unlike the OpenAI shape.
	anthropicDefaultMaxTokens = 4096
)

// Anthropic speaks the Messages API.
//
// It differs from the OpenAI shape in three ways that matter, and each is
// handled below: the system prompt is a top-level field rather than a message,
// content is a list of typed blocks rather than a string, and tool results are
// carried inside a user message rather than in a role of their own.
type Anthropic struct {
	tr    transport
	model string
}

// NewAnthropic builds an adapter.
func NewAnthropic(cfg Config) *Anthropic {
	if cfg.BaseURL == "" {
		cfg.BaseURL = AnthropicDefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")

	headers := map[string]string{"anthropic-version": anthropicVersion}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	if cfg.APIKey != "" {
		headers["x-api-key"] = cfg.APIKey
	}
	cfg.Headers = headers

	return &Anthropic{
		model: cfg.Model,
		tr: transport{
			name: "anthropic",
			cfg:  cfg,
			decodeError: func(body []byte) (string, string) {
				return jsonErrorField(body, "error", "type"), jsonErrorField(body, "error", "message")
			},
		},
	}
}

// Name identifies the provider.
func (p *Anthropic) Name() string { return "anthropic" }

type antBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result block
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antRequest struct {
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	System      string       `json:"system,omitempty"`
	Messages    []antMessage `json:"messages"`
	Tools       []antTool    `json:"tools,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
}

type antResponse struct {
	Model      string     `json:"model"`
	Content    []antBlock `json:"content"`
	StopReason string     `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Complete performs one model call.
func (p *Anthropic) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return llm.Response{}, fmt.Errorf("anthropic: no model specified")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	wire := antRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		System:      req.System,
		Messages:    encodeAnthropicMessages(req.Messages),
		Tools:       encodeAnthropicTools(req.Tools),
		Temperature: req.Temperature,
	}

	var out antResponse
	if err := p.tr.postJSON(ctx, "/messages", wire, &out); err != nil {
		return llm.Response{}, err
	}
	return decodeAnthropic(out)
}

// encodeAnthropicMessages translates the IR into Messages-API shape.
//
// Consecutive tool results are gathered into a single user message: the API
// requires every tool_use block to be answered in the next user turn, and
// emitting one message per result would interleave user turns illegally.
func encodeAnthropicMessages(messages []llm.Message) []antMessage {
	var out []antMessage
	var pendingResults []antBlock

	flush := func() {
		if len(pendingResults) == 0 {
			return
		}
		out = append(out, antMessage{Role: "user", Content: pendingResults})
		pendingResults = nil
	}

	for _, m := range messages {
		switch m.Role {
		case llm.RoleTool:
			pendingResults = append(pendingResults, antBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Text,
				IsError:   m.IsError,
			})
		case llm.RoleAssistant:
			flush()
			blocks := make([]antBlock, 0, len(m.ToolCalls)+1)
			if m.Text != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Args
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			out = append(out, antMessage{Role: "assistant", Content: blocks})
		default:
			flush()
			out = append(out, antMessage{
				Role:    "user",
				Content: []antBlock{{Type: "text", Text: m.Text}},
			})
		}
	}
	flush()
	return out
}

func encodeAnthropicTools(defs []llm.ToolDef) []antTool {
	if len(defs) == 0 {
		return nil
	}
	tools := make([]antTool, 0, len(defs))
	for _, d := range defs {
		schema := d.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, antTool{Name: d.Name, Description: d.Description, InputSchema: schema})
	}
	return tools
}

func decodeAnthropic(out antResponse) (llm.Response, error) {
	resp := llm.Response{
		Model:      out.Model,
		StopReason: anthropicStopReason(out.StopReason),
		Usage: llm.Usage{
			InputTokens:  out.Usage.InputTokens,
			OutputTokens: out.Usage.OutputTokens,
			CachedTokens: out.Usage.CacheReadInputTokens,
		},
	}

	var texts []string
	for _, block := range out.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "tool_use":
			args := block.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			compact, err := compactJSON(args)
			if err != nil {
				return llm.Response{}, fmt.Errorf("anthropic: tool call %s has unparseable input: %w", block.Name, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, llm.ToolCall{ID: block.ID, Name: block.Name, Args: compact})
		}
	}
	resp.Text = strings.Join(texts, "")
	return resp, nil
}

func anthropicStopReason(reason string) llm.StopReason {
	switch reason {
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens":
		return llm.StopMaxTokens
	default:
		return llm.StopEnd
	}
}

// compactJSON removes insignificant whitespace so identical arguments always
// journal identically, whatever the provider sent on the wire.
func compactJSON(raw json.RawMessage) (json.RawMessage, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}
