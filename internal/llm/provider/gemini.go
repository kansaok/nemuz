package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/kansaok/nemuz/internal/llm"
)

// GeminiDefaultBaseURL is the public API root.
const GeminiDefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Gemini speaks the generateContent API.
//
// This is the adapter that proves the IR is a real abstraction rather than a
// thin wrapper around OpenAI's shape. Gemini disagrees on nearly every point:
// the assistant is called "model", messages are "contents" made of "parts",
// tool declarations are nested one level deeper, generation settings live in
// their own object, the model is part of the URL rather than the body, and
// — most consequentially — function calls carry no ids at all.
type Gemini struct {
	tr    transport
	model string
}

// NewGemini builds an adapter.
func NewGemini(cfg Config) *Gemini {
	if cfg.BaseURL == "" {
		cfg.BaseURL = GeminiDefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")

	headers := map[string]string{}
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	if cfg.APIKey != "" {
		// The API also accepts ?key=, but a header keeps the credential out of
		// URLs, which end up in proxy logs and error messages.
		headers["x-goog-api-key"] = cfg.APIKey
	}
	cfg.Headers = headers

	return &Gemini{
		model: cfg.Model,
		tr: transport{
			name: "gemini",
			cfg:  cfg,
			decodeError: func(body []byte) (string, string) {
				return jsonErrorField(body, "error", "status"), jsonErrorField(body, "error", "message")
			},
		},
	}
}

// Name identifies the provider.
func (p *Gemini) Name() string { return "gemini" }

type gemPart struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *gemFunctionCall  `json:"functionCall,omitempty"`
	FunctionResponse *gemFunctionReply `json:"functionResponse,omitempty"`
}

type gemFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gemFunctionReply struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type gemContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []gemPart `json:"parts"`
}

type gemRequest struct {
	SystemInstruction *gemContent          `json:"systemInstruction,omitempty"`
	Contents          []gemContent         `json:"contents"`
	Tools             []gemToolSet         `json:"tools,omitempty"`
	GenerationConfig  *gemGenerationConfig `json:"generationConfig,omitempty"`
}

type gemToolSet struct {
	FunctionDeclarations []gemFunctionDecl `json:"functionDeclarations"`
}

type gemFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type gemGenerationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

type gemResponse struct {
	ModelVersion string `json:"modelVersion"`
	Candidates   []struct {
		Content      gemContent `json:"content"`
		FinishReason string     `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

// Complete performs one model call.
func (p *Gemini) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return llm.Response{}, fmt.Errorf("gemini: no model specified")
	}

	wire := gemRequest{
		Contents: encodeGeminiContents(req.Messages),
		Tools:    encodeGeminiTools(req.Tools),
	}
	if req.System != "" {
		wire.SystemInstruction = &gemContent{Parts: []gemPart{{Text: req.System}}}
	}
	if req.MaxTokens > 0 || req.Temperature != nil {
		wire.GenerationConfig = &gemGenerationConfig{
			MaxOutputTokens: req.MaxTokens,
			Temperature:     req.Temperature,
		}
	}

	path := "/models/" + url.PathEscape(model) + ":generateContent"
	var out gemResponse
	if err := p.tr.postJSON(ctx, path, wire, &out); err != nil {
		return llm.Response{}, err
	}
	return decodeGemini(out)
}

// encodeGeminiContents translates the IR into contents.
//
// Function responses are matched to their calls by name, not by id, so the
// walk keeps an id-to-name map built from the assistant turns it has already
// seen. Consecutive results are merged into one user content for the same
// reason Anthropic needs it: the roles must alternate.
func encodeGeminiContents(messages []llm.Message) []gemContent {
	var out []gemContent
	var pending []gemPart
	names := map[string]string{}

	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, gemContent{Role: "user", Parts: pending})
		pending = nil
	}

	for _, m := range messages {
		switch m.Role {
		case llm.RoleTool:
			name := names[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID
			}
			// The API requires an object here, so string output is wrapped.
			body, _ := json.Marshal(map[string]string{"result": m.Text})
			pending = append(pending, gemPart{
				FunctionResponse: &gemFunctionReply{Name: name, Response: body},
			})
		case llm.RoleAssistant:
			flush()
			parts := make([]gemPart, 0, len(m.ToolCalls)+1)
			if m.Text != "" {
				parts = append(parts, gemPart{Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Name
				args := tc.Args
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				parts = append(parts, gemPart{FunctionCall: &gemFunctionCall{Name: tc.Name, Args: args}})
			}
			out = append(out, gemContent{Role: "model", Parts: parts})
		default:
			flush()
			out = append(out, gemContent{Role: "user", Parts: []gemPart{{Text: m.Text}}})
		}
	}
	flush()
	return out
}

func encodeGeminiTools(defs []llm.ToolDef) []gemToolSet {
	if len(defs) == 0 {
		return nil
	}
	decls := make([]gemFunctionDecl, 0, len(defs))
	for _, d := range defs {
		decls = append(decls, gemFunctionDecl{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Schema,
		})
	}
	return []gemToolSet{{FunctionDeclarations: decls}}
}

func decodeGemini(out gemResponse) (llm.Response, error) {
	if len(out.Candidates) == 0 {
		return llm.Response{}, fmt.Errorf("gemini: response contained no candidates")
	}
	candidate := out.Candidates[0]

	resp := llm.Response{
		Model:      out.ModelVersion,
		StopReason: geminiStopReason(candidate.FinishReason),
		Usage: llm.Usage{
			InputTokens:  out.UsageMetadata.PromptTokenCount,
			OutputTokens: out.UsageMetadata.CandidatesTokenCount,
			CachedTokens: out.UsageMetadata.CachedContentTokenCount,
		},
	}

	var texts []string
	for i, part := range candidate.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			args := part.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			compact, err := compactJSON(args)
			if err != nil {
				return llm.Response{}, fmt.Errorf("gemini: function call %s has unparseable args: %w", part.FunctionCall.Name, err)
			}
			// Gemini issues no ids. Synthesising one from the part's position
			// keeps it stable across runs, which replay requires — a random id
			// would change the journal digest on every replay.
			resp.ToolCalls = append(resp.ToolCalls, llm.ToolCall{
				ID:   fmt.Sprintf("call_%d", i),
				Name: part.FunctionCall.Name,
				Args: compact,
			})
		case part.Text != "":
			texts = append(texts, part.Text)
		}
	}
	resp.Text = strings.Join(texts, "")

	// Gemini reports STOP even when it emitted function calls, so the stop
	// reason is corrected from what the response actually contains.
	if len(resp.ToolCalls) > 0 {
		resp.StopReason = llm.StopToolUse
	}
	return resp, nil
}

func geminiStopReason(reason string) llm.StopReason {
	switch reason {
	case "MAX_TOKENS":
		return llm.StopMaxTokens
	default:
		return llm.StopEnd
	}
}
