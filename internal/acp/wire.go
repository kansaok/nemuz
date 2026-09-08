package acp

import "encoding/json"

// The shapes below follow the published ACP v1 schema. Field names and the
// string constants inside them are part of the wire contract, so they are
// written out rather than derived.

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// Method and Params are set only for notifications the agent sends to the
	// client, which share this envelope.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// ---- initialize ----

type initializeRequest struct {
	ProtocolVersion int `json:"protocolVersion"`
}

type initializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         *agentInfo        `json:"agentInfo,omitempty"`
}

type agentCapabilities struct {
	// LoadSession is false: sessions live for the length of the process, and
	// claiming otherwise would make an editor offer to resume one that is gone.
	LoadSession bool `json:"loadSession"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ---- session/new ----

type newSessionRequest struct {
	Cwd string `json:"cwd"`
}

type newSessionResponse struct {
	SessionID string `json:"sessionId"`
}

// ---- session/prompt ----

type promptRequest struct {
	SessionID string `json:"sessionId"`
	// Prompt is a list of content blocks. They stay raw because only text is
	// acted on, and a block this agent cannot use should be skipped rather
	// than half-decoded.
	Prompt []json.RawMessage `json:"prompt"`
}

// Stop reasons, from the schema's StopReason enum.
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

type promptResponse struct {
	StopReason string `json:"stopReason"`
}

type cancelNotification struct {
	SessionID string `json:"sessionId"`
}

// ---- session/update ----

type sessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

type messageChunk struct {
	SessionUpdate string    `json:"sessionUpdate"`
	Content       textBlock `json:"content"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCallUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallID    string `json:"toolCallId"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Kind          string `json:"kind"`
}
