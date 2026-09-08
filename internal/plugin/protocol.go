// Package plugin runs tool plugins as separate processes and speaks JSON-RPC to
// them over stdio.
//
// # Why a separate process
//
// A plugin that panics, leaks, or wedges takes down only itself. It can be given
// its own capability grant and its own resource limits. It can be written in any
// language. And — the reason that matters most here — Landlock is a thread
// credential, so confining tool execution requires a process that exists to be
// confined. The plugin boundary and the sandbox boundary are the same boundary.
//
// This is the shape LSP and Terraform settled on, for the same reasons.
//
// # Framing
//
// Messages are newline-delimited JSON rather than LSP's Content-Length headers.
// A compact JSON-RPC message contains no literal newline, so the delimiter is
// unambiguous, and a transcript stays greppable with the same tools that read
// the journal.
package plugin

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the wire contract this build speaks. A plugin that
// answers with a different major version is refused rather than guessed at.
const ProtocolVersion = 1

// Method names.
const (
	MethodInitialize = "initialize"
	MethodToolsCall  = "tools/call"
	MethodShutdown   = "shutdown"
)

// JSON-RPC error codes. The negative range below -32000 is reserved for
// application errors by the specification.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeToolFailed means the tool ran and failed in a way the model should
	// see, rather than the plugin itself malfunctioning.
	CodeToolFailed = -32000
)

// Request is a JSON-RPC 2.0 request. A request without an ID is a notification
// and expects no reply.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("plugin: rpc error %d: %s", e.Code, e.Message)
}

// InitializeParams is what the host tells a plugin at startup.
type InitializeParams struct {
	// Protocol is the wire version the host speaks.
	Protocol int `json:"protocol"`
	// Host is the host's version string, for plugins that adapt to it.
	Host string `json:"host"`
	// Workspace is the directory the plugin's tools should operate on.
	Workspace string `json:"workspace"`
}

// Manifest is what a plugin answers with: who it is and what it offers.
type Manifest struct {
	Protocol int        `json:"protocol"`
	Name     string     `json:"name"`
	Version  string     `json:"version"`
	Tools    []ToolSpec `json:"tools"`
}

// ToolSpec describes one tool a plugin provides.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	// Capabilities is what the tool says it needs.
	//
	// This is a request, never a grant. The host decides what a plugin
	// actually gets; a plugin cannot widen its own reach by asking for more.
	Capabilities CapabilitySpec `json:"capabilities"`
}

// CapabilitySpec mirrors tool.Capabilities on the wire.
type CapabilitySpec struct {
	FSRead  []string `json:"fsRead,omitempty"`
	FSWrite []string `json:"fsWrite,omitempty"`
	Net     []string `json:"net,omitempty"`
	Exec    []string `json:"exec,omitempty"`
}

// CallParams asks a plugin to run one tool.
type CallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// CallResult is what a tool returns.
type CallResult struct {
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

// Validate checks that a manifest is usable before any of its tools are offered
// to a model.
func (m Manifest) Validate() error {
	if m.Protocol != ProtocolVersion {
		return fmt.Errorf("plugin %q speaks protocol %d, this build speaks %d", m.Name, m.Protocol, ProtocolVersion)
	}
	if m.Name == "" {
		return fmt.Errorf("plugin manifest has no name")
	}
	if len(m.Tools) == 0 {
		return fmt.Errorf("plugin %q offers no tools", m.Name)
	}
	seen := make(map[string]bool, len(m.Tools))
	for i, t := range m.Tools {
		if t.Name == "" {
			return fmt.Errorf("plugin %q: tool %d has no name", m.Name, i)
		}
		if seen[t.Name] {
			return fmt.Errorf("plugin %q declares %q twice", m.Name, t.Name)
		}
		seen[t.Name] = true
		if len(t.Schema) == 0 {
			return fmt.Errorf("plugin %q: tool %q has no argument schema", m.Name, t.Name)
		}
		if !json.Valid(t.Schema) {
			return fmt.Errorf("plugin %q: tool %q has an invalid schema", m.Name, t.Name)
		}
	}
	return nil
}
