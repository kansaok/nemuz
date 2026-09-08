package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kansaok/nemuz/internal/llm"
)

// captureServer records the request body and headers it received, then replies
// with a canned response. Asserting on what went out matters as much as what
// came back: a wrong encoding is silent until a real provider rejects it.
func captureServer(t *testing.T, reply string) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.body = body
		cap.header = r.Header.Clone()
		cap.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type capture struct {
	body   []byte
	header http.Header
	path   string
}

func (c *capture) decode(t *testing.T, out any) {
	t.Helper()
	if err := json.Unmarshal(c.body, out); err != nil {
		t.Fatalf("could not decode the request the adapter sent: %v\n%s", err, c.body)
	}
}

// toolTurn is a conversation that has already run one tool, so every adapter is
// exercised on the hard part: replaying an assistant tool call and its result.
func toolTurn() llm.Request {
	return llm.Request{
		Model:  "test-model",
		System: "Kamu ringkas.",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "baca README"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "read_file", Args: json.RawMessage(`{"path":"README.md"}`)},
			}},
			{Role: llm.RoleTool, ToolCallID: "c1", Text: "# proyek"},
		},
		Tools: []llm.ToolDef{{
			Name:        "read_file",
			Description: "Read a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
}

// ---------- OpenAI ----------

func TestOpenAIEncodesToolConversation(t *testing.T) {
	srv, cap := captureServer(t, okCompletion)
	p := NewOpenAI("openai", fastConfig(srv.URL))

	if _, err := p.Complete(context.Background(), toolTurn()); err != nil {
		t.Fatal(err)
	}

	var sent oaiRequest
	cap.decode(t, &sent)
	if got := cap.header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization header is %q", got)
	}
	if len(sent.Messages) != 4 {
		t.Fatalf("sent %d messages, want 4 (system, user, assistant, tool)", len(sent.Messages))
	}
	if sent.Messages[0].Role != "system" || sent.Messages[0].Content != "Kamu ringkas." {
		t.Errorf("the system prompt should lead as its own message, got %+v", sent.Messages[0])
	}
	assistant := sent.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("assistant tool call was not encoded: %+v", assistant)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Errorf("arguments must be a JSON string, got %q", assistant.ToolCalls[0].Function.Arguments)
	}
	if sent.Messages[3].Role != "tool" || sent.Messages[3].ToolCallID != "c1" {
		t.Errorf("tool result was not linked to its call: %+v", sent.Messages[3])
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Function.Name != "read_file" {
		t.Errorf("tool definitions were not sent: %+v", sent.Tools)
	}
}

func TestOpenAIDecodesToolCalls(t *testing.T) {
	const reply = `{"model":"gpt-x","choices":[{"message":{"content":"","tool_calls":[
		{"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"a.txt\"}"}}]},
		"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":30,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":16}}}`
	srv, _ := captureServer(t, reply)

	resp, err := NewOpenAI("openai", fastConfig(srv.URL)).Complete(context.Background(), toolTurn())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop reason is %q, want tool_use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("decoded %d tool calls, want 1", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "call_abc" || call.Name != "read_file" {
		t.Errorf("tool call is %+v", call)
	}
	// Whitespace in the wire arguments must not survive into the journal.
	if string(call.Args) != `{"path":"a.txt"}` {
		t.Errorf("arguments were not compacted: %s", call.Args)
	}
	if resp.Usage.CachedTokens != 16 {
		t.Errorf("cached tokens are %d, want 16", resp.Usage.CachedTokens)
	}
}

// ---------- Anthropic ----------

func TestAnthropicEncodesToolConversation(t *testing.T) {
	const reply = `{"model":"claude","content":[{"type":"text","text":"selesai"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":40,"output_tokens":6,"cache_read_input_tokens":12}}`
	srv, cap := captureServer(t, reply)

	resp, err := NewAnthropic(fastConfig(srv.URL)).Complete(context.Background(), toolTurn())
	if err != nil {
		t.Fatal(err)
	}

	var sent antRequest
	cap.decode(t, &sent)
	if cap.header.Get("x-api-key") != "test-key" || cap.header.Get("anthropic-version") == "" {
		t.Errorf("auth or version header missing: %v", cap.header)
	}
	// The system prompt is a field here, not a message.
	if sent.System != "Kamu ringkas." {
		t.Errorf("system prompt is %q, want it at the top level", sent.System)
	}
	if sent.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("max_tokens is %d; the API requires one and the adapter must supply a default", sent.MaxTokens)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("sent %d messages, want 3 (user, assistant, user-with-result)", len(sent.Messages))
	}
	// Tool results ride inside a user message, not a role of their own.
	last := sent.Messages[2]
	if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" {
		t.Fatalf("tool result was not carried as a user tool_result block: %+v", last)
	}
	if last.Content[0].ToolUseID != "c1" {
		t.Errorf("tool_result lost its tool_use_id: %+v", last.Content[0])
	}
	if resp.Usage.CachedTokens != 12 {
		t.Errorf("cache reads are %d, want 12", resp.Usage.CachedTokens)
	}
}

func TestAnthropicMergesConsecutiveToolResults(t *testing.T) {
	const reply = `{"model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
	srv, cap := captureServer(t, reply)

	req := llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "kerjakan"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "a", Name: "read_file", Args: json.RawMessage(`{}`)},
				{ID: "b", Name: "list_dir", Args: json.RawMessage(`{}`)},
			}},
			{Role: llm.RoleTool, ToolCallID: "a", Text: "isi"},
			{Role: llm.RoleTool, ToolCallID: "b", Text: "daftar"},
		},
	}
	if _, err := NewAnthropic(fastConfig(srv.URL)).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var sent antRequest
	cap.decode(t, &sent)
	if len(sent.Messages) != 3 {
		t.Fatalf("sent %d messages; two results must merge into one user turn, got %+v", len(sent.Messages), sent.Messages)
	}
	if len(sent.Messages[2].Content) != 2 {
		t.Errorf("the merged message holds %d blocks, want 2", len(sent.Messages[2].Content))
	}
}

func TestAnthropicDecodesToolUse(t *testing.T) {
	const reply = `{"model":"claude","content":[
		{"type":"text","text":"Saya baca dulu."},
		{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":50,"output_tokens":20}}`
	srv, _ := captureServer(t, reply)

	resp, err := NewAnthropic(fastConfig(srv.URL)).Complete(context.Background(), toolTurn())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Saya baca dulu." {
		t.Errorf("text is %q", resp.Text)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop reason is %q", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("tool calls decoded as %+v", resp.ToolCalls)
	}
	if string(resp.ToolCalls[0].Args) != `{"path":"a.txt"}` {
		t.Errorf("args are %s", resp.ToolCalls[0].Args)
	}
}

// ---------- Gemini ----------

func TestGeminiEncodesToolConversation(t *testing.T) {
	const reply = `{"modelVersion":"gemini-x","candidates":[{"content":{"role":"model",
		"parts":[{"text":"selesai"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":60,"candidatesTokenCount":9,"cachedContentTokenCount":4}}`
	srv, cap := captureServer(t, reply)

	if _, err := NewGemini(fastConfig(srv.URL)).Complete(context.Background(), toolTurn()); err != nil {
		t.Fatal(err)
	}

	if cap.header.Get("x-goog-api-key") != "test-key" {
		t.Errorf("the key should travel in a header, not the URL: %v", cap.header)
	}
	if cap.path != "/models/test-model:generateContent" {
		t.Errorf("path is %q; the model belongs in the URL for this API", cap.path)
	}

	var sent gemRequest
	cap.decode(t, &sent)
	if sent.SystemInstruction == nil || sent.SystemInstruction.Parts[0].Text != "Kamu ringkas." {
		t.Errorf("system instruction is %+v", sent.SystemInstruction)
	}
	if len(sent.Contents) != 3 {
		t.Fatalf("sent %d contents, want 3", len(sent.Contents))
	}
	if sent.Contents[1].Role != "model" {
		t.Errorf("the assistant role must be called \"model\", got %q", sent.Contents[1].Role)
	}
	reply3 := sent.Contents[2].Parts[0].FunctionResponse
	if reply3 == nil {
		t.Fatal("the tool result was not encoded as a functionResponse")
	}
	// Gemini matches results to calls by name, so the adapter must have
	// remembered which call id belonged to which function.
	if reply3.Name != "read_file" {
		t.Errorf("functionResponse names %q; it must name the function, not the call id", reply3.Name)
	}
	if string(reply3.Response) != `{"result":"# proyek"}` {
		t.Errorf("response payload is %s; the API requires an object", reply3.Response)
	}
	if len(sent.Tools) != 1 || len(sent.Tools[0].FunctionDeclarations) != 1 {
		t.Errorf("function declarations are nested one level deeper: %+v", sent.Tools)
	}
}

// TestGeminiSynthesisesStableToolCallIDs covers the one place Gemini's shape
// could break replay: it issues no ids, so the adapter must invent ones that
// are identical on every run.
func TestGeminiSynthesisesStableToolCallIDs(t *testing.T) {
	const reply = `{"modelVersion":"gemini-x","candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"read_file","args":{"path":"a.txt"}}},
		{"functionCall":{"name":"list_dir","args":{"path":"."}}}]},"finishReason":"STOP"}]}`
	srv, _ := captureServer(t, reply)
	p := NewGemini(fastConfig(srv.URL))

	var first []llm.ToolCall
	for run := 0; run < 3; run++ {
		resp, err := p.Complete(context.Background(), toolTurn())
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.ToolCalls) != 2 {
			t.Fatalf("decoded %d calls, want 2", len(resp.ToolCalls))
		}
		// STOP plus function calls still means the loop must run tools.
		if resp.StopReason != llm.StopToolUse {
			t.Errorf("stop reason is %q; function calls mean tool use even when Gemini says STOP", resp.StopReason)
		}
		if run == 0 {
			first = resp.ToolCalls
			continue
		}
		for i := range resp.ToolCalls {
			if resp.ToolCalls[i].ID != first[i].ID {
				t.Fatalf("call %d got id %q on run %d but %q on the first run — replay would break",
					i, resp.ToolCalls[i].ID, run, first[i].ID)
			}
		}
	}
	if first[0].ID == first[1].ID {
		t.Error("two calls in one response share an id")
	}
}

func TestAdaptersRejectMissingModel(t *testing.T) {
	srv, _ := captureServer(t, okCompletion)
	cfg := Config{BaseURL: srv.URL, APIKey: "k"}

	providers := map[string]llm.Provider{
		"openai":    NewOpenAI("openai", cfg),
		"anthropic": NewAnthropic(cfg),
		"gemini":    NewGemini(cfg),
	}
	for name, p := range providers {
		if _, err := p.Complete(context.Background(), llm.Request{
			Messages: []llm.Message{{Role: llm.RoleUser, Text: "halo"}},
		}); err == nil {
			t.Errorf("%s: a request with no model was sent anyway", name)
		}
	}
}
