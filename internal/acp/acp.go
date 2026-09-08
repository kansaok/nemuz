// Package acp serves nemuz over the Agent Client Protocol, so editors that
// speak ACP — Zed among them — can drive a nemuz agent directly.
//
// ACP is JSON-RPC 2.0 over stdio, newline delimited, the same shape nemuz
// already uses for plugins. The method names and message bodies here follow the
// published v1 schema rather than guesswork:
// https://github.com/agentclientprotocol/agent-client-protocol
//
// # What the editor sees
//
// The agent loop produces an answer whole rather than token by token, so the
// answer arrives as one agent_message_chunk rather than a stream of them. What
// the editor does get, and would not from a plain wrapper, is the tool calls the
// turn actually made — read back from the journal once the turn is done, in the
// order they happened.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/journal"
)

// ProtocolVersion is the ACP version this implementation speaks.
const ProtocolVersion = 1

// Agent methods, as named by the v1 schema.
const (
	methodInitialize    = "initialize"
	methodSessionNew    = "session/new"
	methodSessionPrompt = "session/cancel"
)

// Method names. Kept as one block so a schema change is a single diff.
const (
	MethodInitialize    = "initialize"
	MethodSessionNew    = "session/new"
	MethodSessionPrompt = "session/prompt"
	MethodSessionCancel = "session/cancel"
	MethodSessionUpdate = "session/update"
)

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Runner is the part of an agent this package needs.
type Runner interface {
	Run(ctx context.Context, prompt string) (agent.Outcome, error)
}

// EventSource is an optional capability. A Runner that can report what a turn
// did lets the editor show the tool calls rather than only the final answer.
type EventSource interface {
	Events(turnID string) ([]journal.Event, error)
}

// Options configures a Server.
type Options struct {
	// Runner executes turns. Required.
	Runner Runner
	// Workspace is the directory this agent is bound to. A session opened for
	// a different directory is refused rather than silently answered from the
	// wrong place.
	Workspace string
	// Name and Version identify the agent to the editor.
	Name    string
	Version string
}

// Server speaks ACP on behalf of one agent.
type Server struct {
	opts Options
	out  *json.Encoder

	mu       sync.Mutex
	writeMu  sync.Mutex
	sessions map[string]struct{}
	nextID   int
	cancels  map[string]context.CancelFunc
	// inflight tracks prompts still running, so Serve does not return while
	// one of them is still writing to the output stream.
	inflight sync.WaitGroup
}

// NewServer prepares a server.
func NewServer(opts Options) (*Server, error) {
	if opts.Runner == nil {
		return nil, errors.New("acp: Options.Runner is required")
	}
	if opts.Name == "" {
		opts.Name = "nemuz"
	}
	return &Server{
		opts:     opts,
		sessions: map[string]struct{}{},
		cancels:  map[string]context.CancelFunc{},
	}, nil
}

// Serve reads requests until in closes.
//
// A prompt can take minutes, and a cancellation for it arrives on this same
// stream. The read loop therefore must not be sitting inside the prompt when
// that notification shows up, so prompts run on their own goroutine and
// everything else — which is fast — stays inline.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.reply(response{JSONRPC: "2.0", Error: &rpcError{Code: codeParseError, Message: err.Error()}})
			continue
		}
		s.dispatch(ctx, req)
	}
	// A prompt may still be running when the client closes its end. Waiting
	// keeps a half-written notification from racing the caller closing out.
	s.inflight.Wait()
	return sc.Err()
}

// maxLine caps one message. A prompt larger than this is a file, and belongs in
// the workspace where the agent can read it.
const maxLine = 8 << 20

func (s *Server) dispatch(ctx context.Context, req request) {
	switch req.Method {
	case MethodSessionCancel:
		s.cancel(req)
		return
	case MethodSessionPrompt:
		// The one slow method. Running it here would block the read loop, and
		// the cancellation for this very prompt would sit unread behind it.
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			s.respond(ctx, req)
		}()
		return
	}
	s.respond(ctx, req)
}

// respond handles one request and writes its reply.
func (s *Server) respond(ctx context.Context, req request) {
	result, rpcErr := s.handle(ctx, req)
	if req.ID == nil {
		return // a notification expects no reply
	}
	if rpcErr != nil {
		s.reply(response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
		return
	}
	body, err := json.Marshal(result)
	if err != nil {
		s.reply(response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: codeInternalError, Message: err.Error()}})
		return
	}
	s.reply(response{JSONRPC: "2.0", ID: req.ID, Result: body})
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case MethodInitialize:
		return s.initialize(req)
	case MethodSessionNew:
		return s.newSession(req)
	case MethodSessionPrompt:
		return s.prompt(ctx, req)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unsupported method " + req.Method}
	}
}

func (s *Server) initialize(req request) (any, *rpcError) {
	var params initializeRequest
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
	}
	if params.ProtocolVersion != 0 && params.ProtocolVersion != ProtocolVersion {
		return nil, &rpcError{
			Code:    codeInvalidRequest,
			Message: fmt.Sprintf("this agent speaks ACP v%d, the client asked for v%d", ProtocolVersion, params.ProtocolVersion),
		}
	}
	return initializeResponse{
		ProtocolVersion: ProtocolVersion,
		// loadSession is false: sessions live for the process, and claiming
		// otherwise would make an editor offer to resume something gone.
		AgentCapabilities: agentCapabilities{LoadSession: false},
		AgentInfo:         &agentInfo{Name: s.opts.Name, Version: s.opts.Version},
	}, nil
}

func (s *Server) newSession(req request) (any, *rpcError) {
	var params newSessionRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if err := s.checkWorkspace(params.Cwd); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("nemuz-%d", s.nextID)
	s.sessions[id] = struct{}{}
	s.mu.Unlock()

	return newSessionResponse{SessionID: id}, nil
}

// checkWorkspace refuses a session rooted somewhere this agent cannot serve.
//
// The agent's tools are bound to one workspace. Accepting a session for another
// directory and answering from the configured one would be worse than refusing:
// the editor would show confident answers about the wrong project.
func (s *Server) checkWorkspace(cwd string) error {
	if s.opts.Workspace == "" || cwd == "" || cwd == s.opts.Workspace {
		return nil
	}
	if strings.HasPrefix(cwd, s.opts.Workspace+"/") {
		return nil
	}
	return fmt.Errorf("this agent is bound to %s; restart nemuz in %s to work there",
		s.opts.Workspace, cwd)
}

func (s *Server) prompt(ctx context.Context, req request) (any, *rpcError) {
	var params promptRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	_, known := s.sessions[params.SessionID]
	s.mu.Unlock()
	if !known {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown session " + params.SessionID}
	}

	text := promptText(params.Prompt)
	if strings.TrimSpace(text) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "the prompt has no text content"}
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[params.SessionID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.cancels, params.SessionID)
		s.mu.Unlock()
	}()

	out, err := s.opts.Runner.Run(runCtx, text)
	if err != nil {
		if errors.Is(runCtx.Err(), context.Canceled) {
			return promptResponse{StopReason: StopCancelled}, nil
		}
		return nil, &rpcError{Code: codeInternalError, Message: err.Error()}
	}

	// The tool calls go out before the answer, which is the order they
	// happened in, so the editor's transcript reads correctly.
	s.reportToolCalls(params.SessionID, out.TurnID)
	if out.Text != "" {
		s.notifyMessage(params.SessionID, out.Text)
	}
	return promptResponse{StopReason: StopEndTurn}, nil
}

func (s *Server) cancel(req request) {
	var params cancelNotification
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancels[params.SessionID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// reportToolCalls tells the editor what the turn actually did.
//
// The updates are sent after the turn rather than during it, because the loop
// produces its answer whole. The order is still the true one, which is what a
// reader of the transcript needs.
func (s *Server) reportToolCalls(sessionID, turnID string) {
	source, ok := s.opts.Runner.(EventSource)
	if !ok || turnID == "" {
		return
	}
	events, err := source.Events(turnID)
	if err != nil {
		return
	}

	for _, e := range events {
		if e.Kind != journal.KindToolCall {
			continue
		}
		var payload struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.Name == "" {
			continue
		}
		s.notify(MethodSessionUpdate, sessionNotification{
			SessionID: sessionID,
			Update: toolCallUpdate{
				SessionUpdate: "tool_call",
				ToolCallID:    payload.ID,
				Title:         payload.Name,
				Status:        "completed",
				Kind:          "other",
			},
		})
	}
}

func (s *Server) notifyMessage(sessionID, text string) {
	s.notify(MethodSessionUpdate, sessionNotification{
		SessionID: sessionID,
		Update: messageChunk{
			SessionUpdate: "agent_message_chunk",
			Content:       textBlock{Type: "text", Text: text},
		},
	})
}

func (s *Server) notify(method string, params any) {
	body, err := json.Marshal(params)
	if err != nil {
		return
	}
	s.reply(response{JSONRPC: "2.0", Method: method, Params: body})
}

// reply serialises one message. Notifications are emitted from the same
// goroutine as replies, so writes are serialised to keep the stream valid.
func (s *Server) reply(msg response) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.out.Encode(msg)
}

// promptText flattens an ACP prompt into the text nemuz runs.
//
// Non-text blocks — images, audio, embedded resources — are skipped rather than
// described, because inventing a textual stand-in for an image the agent cannot
// see would put words in the user's mouth.
func promptText(blocks []json.RawMessage) string {
	var parts []string
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
