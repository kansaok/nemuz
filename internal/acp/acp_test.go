package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/journal"
)

// These tests drive the server the way an editor would: newline-delimited
// JSON-RPC in, newline-delimited JSON-RPC out. The message shapes come from the
// published v1 schema, so a test passing here means the bytes on the wire are
// the ones an ACP client expects.

type stubRunner struct {
	outcome agent.Outcome
	err     error
	events  []journal.Event
	prompt  string
	block   chan struct{} // when set, Run waits on it or on ctx
}

func (s *stubRunner) Run(ctx context.Context, prompt string) (agent.Outcome, error) {
	s.prompt = prompt
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return agent.Outcome{}, ctx.Err()
		}
	}
	return s.outcome, s.err
}

func (s *stubRunner) Events(string) ([]journal.Event, error) { return s.events, nil }

// session drives a server over pipes and collects everything it emits.
type session struct {
	t    *testing.T
	in   io.WriteCloser
	msgs chan response
	wg   sync.WaitGroup
}

func newSession(t *testing.T, runner Runner, workspace string) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	srv, err := NewServer(Options{Runner: runner, Workspace: workspace, Name: "nemuz", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	s := &session{t: t, in: inW, msgs: make(chan response, 32)}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		_ = srv.Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	go func() {
		defer s.wg.Done()
		dec := json.NewDecoder(outR)
		for {
			var msg response
			if err := dec.Decode(&msg); err != nil {
				close(s.msgs)
				return
			}
			s.msgs <- msg
		}
	}()
	t.Cleanup(func() { inW.Close(); s.wg.Wait() })
	return s
}

func (s *session) send(method string, id any, params any) {
	s.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(body, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// await returns the next message, failing if none arrives.
func (s *session) await() response {
	s.t.Helper()
	select {
	case msg, ok := <-s.msgs:
		if !ok {
			s.t.Fatal("the server closed its output before replying")
		}
		return msg
	case <-time.After(10 * time.Second):
		s.t.Fatal("timed out waiting for the server")
		return response{}
	}
}

// awaitResult returns the next reply that carries a result, decoding it.
func (s *session) awaitResult(out any) response {
	s.t.Helper()
	for {
		msg := s.await()
		if msg.Method != "" {
			continue // a notification; not what this call is waiting for
		}
		if msg.Error != nil {
			return msg
		}
		if out != nil {
			if err := json.Unmarshal(msg.Result, out); err != nil {
				s.t.Fatalf("could not decode the result: %v", err)
			}
		}
		return msg
	}
}

// handshake performs initialize and session/new, returning the session id.
func (s *session) handshake(cwd string) string {
	s.t.Helper()
	s.send(MethodInitialize, 1, map[string]any{"protocolVersion": ProtocolVersion})
	var init initializeResponse
	s.awaitResult(&init)
	if init.ProtocolVersion != ProtocolVersion {
		s.t.Fatalf("agent answered protocol v%d", init.ProtocolVersion)
	}

	s.send(MethodSessionNew, 2, map[string]any{"cwd": cwd, "mcpServers": []any{}})
	var created newSessionResponse
	if msg := s.awaitResult(&created); msg.Error != nil {
		s.t.Fatalf("session/new failed: %s", msg.Error.Message)
	}
	return created.SessionID
}

func textPrompt(text string) []map[string]string {
	return []map[string]string{{"type": "text", "text": text}}
}

// ---------- handshake ----------

func TestInitializeAnnouncesTheProtocolAndAgent(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	s.send(MethodInitialize, 1, map[string]any{"protocolVersion": ProtocolVersion})

	var got initializeResponse
	s.awaitResult(&got)
	if got.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol version is %d", got.ProtocolVersion)
	}
	if got.AgentInfo == nil || got.AgentInfo.Name != "nemuz" {
		t.Errorf("agent info is %+v", got.AgentInfo)
	}
	// Claiming loadSession would make an editor offer to resume a session
	// that does not survive the process.
	if got.AgentCapabilities.LoadSession {
		t.Error("the agent claims it can load sessions, which it cannot")
	}
}

func TestMismatchedProtocolVersionIsRefused(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	s.send(MethodInitialize, 1, map[string]any{"protocolVersion": 99})

	msg := s.awaitResult(nil)
	if msg.Error == nil {
		t.Fatal("a client asking for a future protocol version was accepted")
	}
	if !strings.Contains(msg.Error.Message, "v99") {
		t.Errorf("the error should name the version asked for: %q", msg.Error.Message)
	}
}

// TestSessionForAnotherDirectoryIsRefused matters more than it looks: the
// agent's tools are bound to one workspace, and answering confidently about the
// wrong project is worse than refusing.
func TestSessionForAnotherDirectoryIsRefused(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	s.send(MethodInitialize, 1, map[string]any{"protocolVersion": ProtocolVersion})
	s.awaitResult(nil)

	s.send(MethodSessionNew, 2, map[string]any{"cwd": "/proyek-lain", "mcpServers": []any{}})
	msg := s.awaitResult(nil)
	if msg.Error == nil {
		t.Fatal("a session rooted elsewhere was accepted")
	}
	if !strings.Contains(msg.Error.Message, "/work") {
		t.Errorf("the error should say where the agent is bound: %q", msg.Error.Message)
	}
}

func TestSubdirectorySessionsAreAccepted(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	if id := s.handshake("/work/sub/dir"); id == "" {
		t.Fatal("a session inside the workspace was refused")
	}
}

// ---------- prompting ----------

func TestPromptRunsATurnAndStreamsTheAnswer(t *testing.T) {
	runner := &stubRunner{outcome: agent.Outcome{Text: "Halo dari nemuz.", TurnID: "01TURN"}}
	s := newSession(t, runner, "/work")
	id := s.handshake("/work")

	s.send(MethodSessionPrompt, 3, map[string]any{"sessionId": id, "prompt": textPrompt("apa kabar?")})

	// The answer arrives as a session/update notification before the reply.
	notif := s.await()
	if notif.Method != MethodSessionUpdate {
		t.Fatalf("expected a session/update, got %+v", notif)
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string    `json:"sessionUpdate"`
			Content       textBlock `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(notif.Params, &update); err != nil {
		t.Fatal(err)
	}
	if update.SessionID != id {
		t.Errorf("the update names session %q", update.SessionID)
	}
	if update.Update.SessionUpdate != "agent_message_chunk" {
		t.Errorf("update kind is %q", update.Update.SessionUpdate)
	}
	if update.Update.Content.Type != "text" || update.Update.Content.Text != "Halo dari nemuz." {
		t.Errorf("content is %+v", update.Update.Content)
	}

	var reply promptResponse
	s.awaitResult(&reply)
	if reply.StopReason != StopEndTurn {
		t.Errorf("stop reason is %q", reply.StopReason)
	}
	if runner.prompt != "apa kabar?" {
		t.Errorf("the agent was asked %q", runner.prompt)
	}
}

// TestToolCallsAreReportedBeforeTheAnswer is what an editor gets from nemuz
// that a plain wrapper would not: the tools the turn actually ran, in order.
func TestToolCallsAreReportedBeforeTheAnswer(t *testing.T) {
	runner := &stubRunner{
		outcome: agent.Outcome{Text: "selesai", TurnID: "01TURN"},
		events: []journal.Event{
			{Kind: journal.KindToolCall, Payload: json.RawMessage(`{"id":"c1","name":"list_dir"}`)},
			{Kind: journal.KindToolResult, Payload: json.RawMessage(`{"id":"c1","name":"list_dir"}`)},
			{Kind: journal.KindToolCall, Payload: json.RawMessage(`{"id":"c2","name":"read_file"}`)},
		},
	}
	s := newSession(t, runner, "/work")
	id := s.handshake("/work")
	s.send(MethodSessionPrompt, 3, map[string]any{"sessionId": id, "prompt": textPrompt("kerjakan")})

	var kinds []string
	var titles []string
	for i := 0; i < 3; i++ {
		notif := s.await()
		var update struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Title         string `json:"title"`
			} `json:"update"`
		}
		if err := json.Unmarshal(notif.Params, &update); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, update.Update.SessionUpdate)
		titles = append(titles, update.Update.Title)
	}

	want := []string{"tool_call", "tool_call", "agent_message_chunk"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("update %d is %q, want %q — tools come before the answer", i, kinds[i], want[i])
		}
	}
	if titles[0] != "list_dir" || titles[1] != "read_file" {
		t.Errorf("tool calls are %v, want them in the order they happened", titles[:2])
	}
}

func TestPromptForAnUnknownSessionIsRefused(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	s.send(MethodInitialize, 1, map[string]any{"protocolVersion": ProtocolVersion})
	s.awaitResult(nil)

	s.send(MethodSessionPrompt, 2, map[string]any{"sessionId": "tidak-ada", "prompt": textPrompt("halo")})
	msg := s.awaitResult(nil)
	if msg.Error == nil {
		t.Fatal("a prompt for an unknown session was accepted")
	}
}

func TestPromptWithNoTextIsRefused(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	id := s.handshake("/work")

	// An image-only prompt: nothing this agent can act on.
	s.send(MethodSessionPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "image", "data": "..."}},
	})
	msg := s.awaitResult(nil)
	if msg.Error == nil {
		t.Fatal("a prompt with no text was accepted")
	}
}

func TestMultipleTextBlocksAreJoined(t *testing.T) {
	runner := &stubRunner{outcome: agent.Outcome{Text: "ok", TurnID: "01A"}}
	s := newSession(t, runner, "/work")
	id := s.handshake("/work")

	s.send(MethodSessionPrompt, 3, map[string]any{"sessionId": id, "prompt": []map[string]string{
		{"type": "text", "text": "baris pertama"},
		{"type": "image", "data": "diabaikan"},
		{"type": "text", "text": "baris kedua"},
	}})
	s.await()
	s.awaitResult(nil)

	if runner.prompt != "baris pertama\nbaris kedua" {
		t.Errorf("prompt is %q", runner.prompt)
	}
}

// TestCancelStopsATurnInFlight checks the notification is handled while a
// prompt is still running, rather than queued behind it.
func TestCancelStopsATurnInFlight(t *testing.T) {
	runner := &stubRunner{block: make(chan struct{})}
	s := newSession(t, runner, "/work")
	id := s.handshake("/work")

	s.send(MethodSessionPrompt, 3, map[string]any{"sessionId": id, "prompt": textPrompt("tugas panjang")})
	time.Sleep(50 * time.Millisecond)
	s.send(MethodSessionCancel, nil, map[string]any{"sessionId": id})

	var reply promptResponse
	msg := s.awaitResult(&reply)
	if msg.Error != nil {
		t.Fatalf("cancelling produced an error instead of a stop reason: %s", msg.Error.Message)
	}
	if reply.StopReason != StopCancelled {
		t.Fatalf("stop reason is %q, want cancelled", reply.StopReason)
	}
}

func TestAgentFailureBecomesAnError(t *testing.T) {
	s := newSession(t, &stubRunner{err: errors.New("provider is down")}, "/work")
	id := s.handshake("/work")

	s.send(MethodSessionPrompt, 3, map[string]any{"sessionId": id, "prompt": textPrompt("halo")})
	msg := s.awaitResult(nil)
	if msg.Error == nil || !strings.Contains(msg.Error.Message, "provider is down") {
		t.Fatalf("error is %+v", msg.Error)
	}
}

func TestUnknownMethodIsReportedNotIgnored(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	s.send("session/telepathy", 1, map[string]any{})

	msg := s.awaitResult(nil)
	if msg.Error == nil || msg.Error.Code != codeMethodNotFound {
		t.Fatalf("error is %+v", msg.Error)
	}
}

func TestMalformedLineDoesNotKillTheServer(t *testing.T) {
	s := newSession(t, &stubRunner{}, "/work")
	if _, err := s.in.Write([]byte("bukan json\n")); err != nil {
		t.Fatal(err)
	}
	if msg := s.await(); msg.Error == nil || msg.Error.Code != codeParseError {
		t.Fatalf("error is %+v", msg.Error)
	}

	// The connection must still work afterwards.
	if id := s.handshake("/work"); id == "" {
		t.Fatal("the server stopped serving after one bad line")
	}
}

func TestServerRequiresARunner(t *testing.T) {
	if _, err := NewServer(Options{}); err == nil {
		t.Fatal("a server with no runner was created")
	}
}
