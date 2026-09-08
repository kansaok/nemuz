package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/metrics"
)

// stubRunner records what it was asked and returns a fixed outcome.
type stubRunner struct {
	prompt  string
	outcome agent.Outcome
	err     error
	calls   int
}

func (s *stubRunner) Run(_ context.Context, prompt string) (agent.Outcome, error) {
	s.calls++
	s.prompt = prompt
	return s.outcome, s.err
}

func newServer(t *testing.T, runner Runner, key string) *httptest.Server {
	t.Helper()
	timeNow = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { timeNow = time.Now })

	s, err := NewServer(Options{Runner: runner, Model: "nemuz-test", APIKey: key, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("could not decode the response: %v", err)
	}
}

// ---------- the shape clients expect ----------

func TestAnswersAChatCompletionRequest(t *testing.T) {
	runner := &stubRunner{outcome: agent.Outcome{
		Text:   "Proyek ini punya README.",
		TurnID: "01TURN000000000000000000AA",
		Usage:  llm.Usage{InputTokens: 42, OutputTokens: 9},
	}}
	srv := newServer(t, runner, "rahasia")

	resp := post(t, srv, "/v1/chat/completions", "rahasia",
		`{"model":"nemuz-test","messages":[{"role":"user","content":"apa isi workspace?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status is %d", resp.StatusCode)
	}

	var got completionResponse
	decode(t, resp, &got)
	if got.Object != "chat.completion" || len(got.Choices) != 1 {
		t.Fatalf("response is %+v", got)
	}
	if got.Choices[0].Message.Content != "Proyek ini punya README." {
		t.Errorf("content is %q", got.Choices[0].Message.Content)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 51 {
		t.Errorf("usage is %+v", got.Usage)
	}
	if runner.prompt != "apa isi workspace?" {
		t.Errorf("the agent was asked %q", runner.prompt)
	}
}

// TestTheCompletionIDIsTheTurnID is what makes this endpoint different from
// every other OpenAI-shaped proxy: the id it hands back can be replayed.
func TestTheCompletionIDIsTheTurnID(t *testing.T) {
	const turn = "01REPLAYABLE00000000000AA"
	srv := newServer(t, &stubRunner{outcome: agent.Outcome{Text: "ok", TurnID: turn}}, "")

	resp := post(t, srv, "/v1/chat/completions", "",
		`{"messages":[{"role":"user","content":"halo"}]}`)
	var got completionResponse
	decode(t, resp, &got)
	if got.ID != turn {
		t.Fatalf("completion id is %q, want the turn id %q", got.ID, turn)
	}
}

func TestSystemMessagesAreFoldedIntoThePrompt(t *testing.T) {
	runner := &stubRunner{outcome: agent.Outcome{Text: "ok", TurnID: "01A"}}
	srv := newServer(t, runner, "")

	post(t, srv, "/v1/chat/completions", "", `{"messages":[
		{"role":"system","content":"Jawab dalam bahasa Indonesia."},
		{"role":"user","content":"pertanyaan lama"},
		{"role":"assistant","content":"jawaban lama"},
		{"role":"user","content":"pertanyaan terbaru"}
	]}`)

	if !strings.Contains(runner.prompt, "Jawab dalam bahasa Indonesia.") {
		t.Errorf("the system message was dropped: %q", runner.prompt)
	}
	// Earlier turns are the agent's own history, which it already has.
	if !strings.HasSuffix(runner.prompt, "pertanyaan terbaru") {
		t.Errorf("the newest user message should be the prompt, got %q", runner.prompt)
	}
	if strings.Contains(runner.prompt, "jawaban lama") {
		t.Errorf("an old assistant turn was replayed into the prompt: %q", runner.prompt)
	}
}

func TestContentPartsAreAccepted(t *testing.T) {
	runner := &stubRunner{outcome: agent.Outcome{Text: "ok", TurnID: "01A"}}
	srv := newServer(t, runner, "")

	resp := post(t, srv, "/v1/chat/completions", "", `{"messages":[
		{"role":"user","content":[{"type":"text","text":"bagian pertama "},{"type":"text","text":"dan kedua"}]}
	]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status is %d", resp.StatusCode)
	}
	if runner.prompt != "bagian pertama dan kedua" {
		t.Errorf("prompt is %q", runner.prompt)
	}
}

func TestRequestWithoutAUserMessageIsRejected(t *testing.T) {
	srv := newServer(t, &stubRunner{}, "")
	resp := post(t, srv, "/v1/chat/completions", "", `{"messages":[{"role":"system","content":"halo"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status is %d", resp.StatusCode)
	}
}

func TestAgentFailureNamesTheRecordedTurn(t *testing.T) {
	srv := newServer(t, &stubRunner{
		err:     errors.New("provider is down"),
		outcome: agent.Outcome{TurnID: "01GAGAL0000000000000000AA"},
	}, "")

	resp := post(t, srv, "/v1/chat/completions", "", `{"messages":[{"role":"user","content":"halo"}]}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status is %d", resp.StatusCode)
	}
	var body struct {
		Error struct{ Message string } `json:"error"`
	}
	decode(t, resp, &body)
	// A failed turn is still recorded, so the id is the useful part of the error.
	if !strings.Contains(body.Error.Message, "01GAGAL") {
		t.Errorf("the error does not name the recorded turn: %q", body.Error.Message)
	}
}

// ---------- streaming ----------

func TestStreamingProducesTheEventsClientsExpect(t *testing.T) {
	srv := newServer(t, &stubRunner{outcome: agent.Outcome{Text: "halo dunia", TurnID: "01STREAM00000000000000AA"}}, "")

	resp := post(t, srv, "/v1/chat/completions", "",
		`{"stream":true,"messages":[{"role":"user","content":"halo"}]}`)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type is %q", ct)
	}

	var chunks []completionResponse
	var sawDone bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == sc.Text() || line == "" {
			continue
		}
		if line == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk completionResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %q", line)
		}
		chunks = append(chunks, chunk)
	}

	if !sawDone {
		t.Error("the stream never sent [DONE]")
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want role, content and finish", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("the first chunk should carry the role, got %+v", chunks[0].Choices[0].Delta)
	}
	if chunks[1].Choices[0].Delta.Content != "halo dunia" {
		t.Errorf("content chunk is %+v", chunks[1].Choices[0].Delta)
	}
	if chunks[2].Choices[0].FinishReason == nil || *chunks[2].Choices[0].FinishReason != "stop" {
		t.Errorf("the last chunk should finish the stream: %+v", chunks[2].Choices[0])
	}
	for i, c := range chunks {
		if c.ID != "01STREAM00000000000000AA" {
			t.Errorf("chunk %d has id %q", i, c.ID)
		}
		// A zeroed usage object in a chunk reads as a real measurement of
		// nothing, so it must be absent rather than empty.
		if c.Usage != nil {
			t.Errorf("chunk %d carries a usage object: %+v", i, c.Usage)
		}
	}
}

// ---------- authentication ----------

func TestRequestsWithoutTheKeyAreRefused(t *testing.T) {
	srv := newServer(t, &stubRunner{}, "rahasia")

	for name, key := range map[string]string{"no key": "", "wrong key": "salah"} {
		resp := post(t, srv, "/v1/chat/completions", key, `{"messages":[{"role":"user","content":"halo"}]}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status is %d, want 401", name, resp.StatusCode)
		}
	}
}

// TestUnauthenticatedServerMustBeLoopbackOnly is the mistake worth making
// impossible: an agent endpoint with no key, reachable off the machine, is a
// remote shell with extra steps.
func TestUnauthenticatedServerMustBeLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8642", ":8642", "192.168.1.5:8642", "[::]:8642"} {
		_, err := NewServer(Options{Runner: &stubRunner{}, Model: "m", Addr: addr})
		if !errors.Is(err, ErrKeyRequired) {
			t.Errorf("%s was accepted without a key: %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:8642", "localhost:8642", "[::1]:8642"} {
		if _, err := NewServer(Options{Runner: &stubRunner{}, Model: "m", Addr: addr}); err != nil {
			t.Errorf("%s was refused even though it is loopback: %v", addr, err)
		}
	}
	// A key makes any address acceptable.
	if _, err := NewServer(Options{Runner: &stubRunner{}, Model: "m", Addr: "0.0.0.0:8642", APIKey: "k"}); err != nil {
		t.Errorf("a keyed public listener was refused: %v", err)
	}
}

func TestHealthNeedsNoKey(t *testing.T) {
	srv := newServer(t, &stubRunner{}, "rahasia")
	resp, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status is %d; health checks must not need credentials", resp.StatusCode)
	}
}

func TestModelsListsTheConfiguredModel(t *testing.T) {
	srv := newServer(t, &stubRunner{}, "")
	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got modelList
	decode(t, resp, &got)
	if len(got.Data) != 1 || got.Data[0].ID != "nemuz-test" {
		t.Fatalf("models are %+v", got)
	}
}

func TestServerRefusesIncompleteOptions(t *testing.T) {
	for name, opts := range map[string]Options{
		"no runner": {Model: "m", Addr: "127.0.0.1:0"},
		"no model":  {Runner: &stubRunner{}, Addr: "127.0.0.1:0"},
	} {
		if _, err := NewServer(opts); err == nil {
			t.Errorf("%s: an incomplete server was created", name)
		}
	}
}

// ---------- metrics ----------

func newMeteredServer(t *testing.T, runner Runner) (*httptest.Server, *metrics.Metrics) {
	t.Helper()
	timeNow = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { timeNow = time.Now })

	m := metrics.New()
	s, err := NewServer(Options{Runner: runner, Model: "nemuz-test", Addr: "127.0.0.1:0", Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, m
}

func scrape(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type is %q; Prometheus expects text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestTurnsAndResponsesAreMeasured(t *testing.T) {
	runner := &stubRunner{outcome: agent.Outcome{
		Text: "ok", TurnID: "01A", Steps: 2, ToolCalls: 1,
		Usage: llm.Usage{InputTokens: 120, OutputTokens: 30, CachedTokens: 64},
	}}
	srv, _ := newMeteredServer(t, runner)

	post(t, srv, "/v1/chat/completions", "", `{"messages":[{"role":"user","content":"halo"}]}`)

	body := scrape(t, srv)
	for _, want := range []string{
		`nemuz_turns_total{outcome="ok"} 1`,
		`nemuz_tool_calls_total 1`,
		`nemuz_turn_steps_total 2`,
		`nemuz_tokens_total{kind="input"} 120`,
		`nemuz_tokens_total{kind="cached"} 64`,
		`nemuz_http_responses_total{status="200"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from:\n%s", want, body)
		}
	}
}

func TestFailedTurnsAreCountedSeparately(t *testing.T) {
	srv, _ := newMeteredServer(t, &stubRunner{err: errors.New("provider is down")})
	post(t, srv, "/v1/chat/completions", "", `{"messages":[{"role":"user","content":"halo"}]}`)

	body := scrape(t, srv)
	if !strings.Contains(body, `nemuz_turns_total{outcome="error"} 1`) {
		t.Errorf("a failed turn was not counted as an error:\n%s", body)
	}
	if !strings.Contains(body, `nemuz_http_responses_total{status="500"} 1`) {
		t.Errorf("the 500 was not counted:\n%s", body)
	}
}

// TestMetricsAreAbsentWhenNotEnabled keeps an endpoint that always reads zero
// from looking like a working one.
func TestMetricsAreAbsentWhenNotEnabled(t *testing.T) {
	srv := newServer(t, &stubRunner{}, "")
	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status is %d, want 404 when metrics are off", resp.StatusCode)
	}
}

// TestStreamingStillFlushesWhenMeasured guards the wrapper: a recorder that
// swallowed Flush would turn every streamed answer into one blob at the end.
func TestStreamingStillFlushesWhenMeasured(t *testing.T) {
	srv, _ := newMeteredServer(t, &stubRunner{outcome: agent.Outcome{Text: "halo", TurnID: "01A"}})

	resp := post(t, srv, "/v1/chat/completions", "",
		`{"stream":true,"messages":[{"role":"user","content":"halo"}]}`)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type is %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("the stream did not complete:\n%s", body)
	}
}
