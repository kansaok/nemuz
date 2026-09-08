package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kansaok/nemuz/internal/llm"
)

// OpenAIDefaultBaseURL is the public API root.
const OpenAIDefaultBaseURL = "https://api.openai.com/v1"

// OpenAI speaks the Chat Completions API.
//
// This adapter is deliberately the widest one: OpenRouter, Groq, Together,
// DeepSeek, Fireworks, vLLM, LM Studio and Ollama all serve the same shape, so
// pointing BaseURL elsewhere covers most providers people actually use without
// another line of translation code.
type OpenAI struct {
	tr    transport
	model string
	label string
}

// NewOpenAI builds an adapter. Label names the provider in journals and errors,
// which matters when the same code is talking to Groq or a local Ollama.
func NewOpenAI(label string, cfg Config) *OpenAI {
	if cfg.BaseURL == "" {
		cfg.BaseURL = OpenAIDefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if label == "" {
		label = "openai"
	}
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	if cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.APIKey
	}
	cfg.Headers = headers

	return &OpenAI{
		model: cfg.Model,
		label: label,
		tr: transport{
			name: label,
			cfg:  cfg,
			decodeError: func(body []byte) (string, string) {
				return firstJSONField(body, []string{"error", "code"}, []string{"error", "type"}, []string{"code"}),
					firstJSONField(body, []string{"error", "message"}, []string{"message"}, []string{"detail"})
			},
		},
	}
}

// Name identifies the provider.
func (p *OpenAI) Name() string { return p.label }

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON document encoded as a string, which is how the API
	// carries it. It is decoded back into structured JSON at the boundary.
	Arguments string `json:"arguments"`
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
}

type oaiTool struct {
	Type     string        `json:"type"`
	Function oaiToolSchema `json:"function"`
}

type oaiToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Complete performs one model call.
func (p *OpenAI) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return llm.Response{}, fmt.Errorf("%s: no model specified", p.label)
	}

	wire := oaiRequest{
		Model:       model,
		Messages:    p.encodeMessages(req),
		Tools:       p.encodeTools(req.Tools),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}

	var out oaiResponse
	if err := p.tr.postJSON(ctx, "/chat/completions", wire, &out); err != nil {
		return llm.Response{}, err
	}
	return p.decode(out)
}

func (p *OpenAI) encodeMessages(req llm.Request) []oaiMessage {
	msgs := make([]oaiMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleTool:
			msgs = append(msgs, oaiMessage{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    m.Text,
			})
		case llm.RoleAssistant:
			out := oaiMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				args := string(tc.Args)
				if args == "" {
					args = "{}"
				}
				out.ToolCalls = append(out.ToolCalls, oaiToolCall{
					ID:       tc.ID,
					Type:     "function",
					Function: oaiFunction{Name: tc.Name, Arguments: args},
				})
			}
			msgs = append(msgs, out)
		default:
			msgs = append(msgs, oaiMessage{Role: string(m.Role), Content: m.Text})
		}
	}
	return msgs
}

func (p *OpenAI) encodeTools(defs []llm.ToolDef) []oaiTool {
	if len(defs) == 0 {
		return nil
	}
	tools := make([]oaiTool, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, oaiTool{
			Type: "function",
			Function: oaiToolSchema{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Schema,
			},
		})
	}
	return tools
}

func (p *OpenAI) decode(out oaiResponse) (llm.Response, error) {
	if len(out.Choices) == 0 {
		return llm.Response{}, fmt.Errorf("%s: response contained no choices", p.label)
	}
	choice := out.Choices[0]

	resp := llm.Response{
		Text:       choice.Message.Content,
		Model:      out.Model,
		StopReason: openAIStopReason(choice.FinishReason),
		Usage: llm.Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
			CachedTokens: out.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	for i, tc := range choice.Message.ToolCalls {
		args, err := normaliseArgs(tc.Function.Arguments)
		if err != nil {
			return llm.Response{}, fmt.Errorf("%s: tool call %s has unparseable arguments: %w", p.label, tc.Function.Name, err)
		}
		id := tc.ID
		if id == "" {
			// Some compatible servers omit ids. A positional fallback keeps
			// them deterministic, which replay depends on.
			id = fmt.Sprintf("call_%d", i)
		}
		resp.ToolCalls = append(resp.ToolCalls, llm.ToolCall{ID: id, Name: tc.Function.Name, Args: args})
	}
	return resp, nil
}

func openAIStopReason(reason string) llm.StopReason {
	switch reason {
	case "tool_calls", "function_call":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	default:
		return llm.StopEnd
	}
}

// normaliseArgs turns the API's stringified JSON into compact structured JSON.
//
// Compacting matters for more than tidiness: the arguments end up in the
// journal, and whitespace that varied between runs would change the digest.
func normaliseArgs(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`), nil
	}
	var doc any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, err
	}
	compact, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return compact, nil
}
