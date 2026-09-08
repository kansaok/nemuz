// Command plugin-go is a reference nemuz plugin.
//
// It shows the whole contract in one file: read newline-delimited JSON-RPC from
// stdin, answer initialize with a manifest, answer tools/call with a result,
// and exit on shutdown. Anything that can read stdin and write stdout can be a
// nemuz plugin; this happens to be Go.
//
// Two rules are worth copying:
//
//   - Never write anything but JSON-RPC to stdout. Diagnostics go to stderr,
//     which the host captures and shows when something goes wrong.
//   - Declare the narrowest capabilities the tools actually need. The host
//     decides what to grant, and asking for more than you use only makes the
//     refusal more likely.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const protocolVersion = 1

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      *int64    `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// codeToolFailed tells the host the tool ran and failed, as opposed to the
// plugin malfunctioning. The host passes such failures back to the model so it
// can try something else.
const codeToolFailed = -32000

var workspace string

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-go: unreadable request:", err)
			continue
		}
		if req.Method == "shutdown" {
			return
		}
		reply := handle(req)
		if req.ID == nil {
			continue // a notification expects no answer
		}
		body, err := json.Marshal(reply)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plugin-go: could not encode reply:", err)
			continue
		}
		out.Write(append(body, '\n'))
		out.Flush()
	}
}

func handle(req request) response {
	reply := response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		var params struct {
			Protocol  int    `json:"protocol"`
			Workspace string `json:"workspace"`
		}
		_ = json.Unmarshal(req.Params, &params)
		workspace = params.Workspace
		reply.Result = manifest()

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			reply.Error = &rpcError{Code: -32602, Message: err.Error()}
			return reply
		}
		result, err := call(params.Name, params.Arguments)
		if err != nil {
			reply.Error = &rpcError{Code: codeToolFailed, Message: err.Error()}
			return reply
		}
		reply.Result = result

	default:
		reply.Error = &rpcError{Code: -32601, Message: "unknown method " + req.Method}
	}
	return reply
}

func manifest() map[string]any {
	return map[string]any{
		"protocol": protocolVersion,
		"name":     "example",
		"version":  "0.1.0",
		"tools": []map[string]any{
			{
				"name":        "shout",
				"description": "Uppercase a string. Needs nothing from the machine.",
				"schema": json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},` +
					`"required":["text"],"additionalProperties":false}`),
				// No capabilities at all: pure computation. This is the set
				// most tools should be able to declare.
				"capabilities": map[string]any{},
			},
			{
				"name":        "count_lines",
				"description": "Count the lines of a file in the workspace.",
				"schema": json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},` +
					`"required":["path"],"additionalProperties":false}`),
				"capabilities": map[string]any{"fsRead": []string{workspace}},
			},
		},
	}
}

func call(name string, args json.RawMessage) (map[string]any, error) {
	switch name {
	case "shout":
		var a struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		return map[string]any{"content": strings.ToUpper(a.Text)}, nil

	case "count_lines":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %w", err)
		}
		body, err := os.ReadFile(workspace + "/" + a.Path)
		if err != nil {
			// A missing file is the model's problem to solve, so it comes back
			// as a tool failure rather than a crash.
			return nil, fmt.Errorf("cannot read %s: %w", a.Path, err)
		}
		return map[string]any{"content": fmt.Sprintf("%d", strings.Count(string(body), "\n"))}, nil

	default:
		return nil, fmt.Errorf("no tool named %q", name)
	}
}
