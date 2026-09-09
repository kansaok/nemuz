package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/kansaok/nemuz/internal/tool"
)

// Server serves a set of tools over the plugin protocol.
//
// It exists so nemuz's own built-in tools can be spoken to exactly the way a
// third-party plugin is. That is not symmetry for its own sake: Landlock is a
// thread credential, so confining a tool requires a process dedicated to being
// confined. Running the built-in tools through the same boundary the plugins
// already use means there is one sandboxing story instead of two, and the
// built-in tools are held to the contract everyone else is.
type Server struct {
	// Name identifies the plugin in manifests and journals.
	Name string
	// Version is reported in the manifest.
	Version string
	// Tools are served. Names are used as declared; the host namespaces them.
	Tools *tool.Registry
	// Sandbox, if set, is reported in the manifest as what this process
	// actually achieved — not what it was asked to achieve. See Manifest.Sandbox.
	Sandbox string

	// OnInitialize is called with the host's parameters before the manifest is
	// returned, for servers that need the workspace.
	OnInitialize func(InitializeParams) error

	mu sync.Mutex
}

// Serve reads requests from in and writes replies to out until in closes or the
// host sends shutdown.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Name == "" {
		return errors.New("plugin: server has no name")
	}
	if s.Tools == nil || s.Tools.Len() == 0 {
		return errors.New("plugin: server has no tools")
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	w := bufio.NewWriter(out)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// A malformed line has no id to answer, so it is reported on
			// stderr by the caller's logger and skipped.
			continue
		}
		if req.Method == MethodShutdown {
			return w.Flush()
		}

		resp := s.handle(ctx, req)
		if req.ID == nil {
			continue // a notification expects no reply
		}
		body, err := json.Marshal(resp)
		if err != nil {
			return fmt.Errorf("plugin: encode reply: %w", err)
		}
		if _, err := w.Write(append(body, '\n')); err != nil {
			return fmt.Errorf("plugin: write reply: %w", err)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("plugin: flush reply: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("plugin: read request: %w", err)
	}
	return w.Flush()
}

func (s *Server) handle(ctx context.Context, req Request) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case MethodInitialize:
		var params InitializeParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = &RPCError{Code: CodeInvalidParams, Message: err.Error()}
				return resp
			}
		}
		if params.Protocol != ProtocolVersion {
			resp.Error = &RPCError{
				Code:    CodeInvalidRequest,
				Message: fmt.Sprintf("host speaks protocol %d, this server speaks %d", params.Protocol, ProtocolVersion),
			}
			return resp
		}
		if s.OnInitialize != nil {
			if err := s.OnInitialize(params); err != nil {
				resp.Error = &RPCError{Code: CodeInternalError, Message: err.Error()}
				return resp
			}
		}
		resp.Result = mustMarshal(s.manifest())

	case MethodToolsCall:
		var params CallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &RPCError{Code: CodeInvalidParams, Message: err.Error()}
			return resp
		}
		t, ok := s.Tools.Get(params.Name)
		if !ok {
			resp.Error = &RPCError{Code: CodeInvalidParams, Message: "no tool named " + params.Name}
			return resp
		}
		args := params.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}

		s.mu.Lock()
		result, err := t.Run(ctx, args)
		s.mu.Unlock()
		if err != nil {
			// A Go error means the tool could not run at all, which is the
			// server malfunctioning rather than the tool failing.
			resp.Error = &RPCError{Code: CodeInternalError, Message: err.Error()}
			return resp
		}
		resp.Result = mustMarshal(CallResult{Content: result.Content, IsError: result.IsError})

	default:
		resp.Error = &RPCError{Code: CodeMethodNotFound, Message: "unknown method " + req.Method}
	}
	return resp
}

func (s *Server) manifest() Manifest {
	m := Manifest{Protocol: ProtocolVersion, Name: s.Name, Version: s.Version, Sandbox: s.Sandbox}
	if m.Version == "" {
		m.Version = "0.0.0"
	}
	for _, name := range s.Tools.Names() {
		t, ok := s.Tools.Get(name)
		if !ok {
			continue
		}
		caps := t.Capabilities()
		m.Tools = append(m.Tools, ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
			Capabilities: CapabilitySpec{
				FSRead:  caps.FSRead,
				FSWrite: caps.FSWrite,
				Net:     caps.Net,
				Exec:    caps.Exec,
			},
		})
	}
	return m
}

// mustMarshal encodes a value that cannot fail to encode.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value passed here is a plain struct of strings and bools.
		panic("plugin: unencodable reply: " + err.Error())
	}
	return b
}
