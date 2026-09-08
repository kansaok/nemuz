package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The point of running plugins as processes is that they can be written in any
// language. These tests drive the JavaScript SDK from the Go host to prove that
// is true in practice, not just in principle.

func nodePlugin(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	script, err := filepath.Abs("../../examples/plugin-ts/plugin.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the example plugin is missing: %v", err)
	}

	c, err := Start(context.Background(), Options{
		Command:     []string{"node", script},
		Workspace:   t.TempDir(),
		HostVersion: "test",
	})
	if err != nil {
		t.Fatalf("the JavaScript plugin did not start: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestJavaScriptPluginSpeaksTheSameProtocol(t *testing.T) {
	c := nodePlugin(t)

	m := c.Manifest()
	if m.Name != "example" {
		t.Errorf("plugin name is %q", m.Name)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("the JavaScript SDK produced an invalid manifest: %v", err)
	}

	res, err := c.Call(context.Background(), "shout", json.RawMessage(`{"text":"halo dari go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "HALO DARI GO" {
		t.Errorf("got %q", res.Content)
	}
}

// TestGoAndJavaScriptPluginsAgree is the real assertion: the two reference
// implementations, written independently in different languages, are
// interchangeable from the host's point of view.
func TestGoAndJavaScriptPluginsAgree(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	workspace := t.TempDir()
	goClient := startExample(t, workspace)
	jsClient := nodePlugin(t)

	for _, c := range []*Client{goClient, jsClient} {
		m := c.Manifest()
		if m.Name != "example" {
			t.Errorf("%s: name is %q", m.Name, m.Name)
		}
		if len(m.Tools) != 2 {
			t.Errorf("%s: declares %d tools, want 2", m.Name, len(m.Tools))
		}

		res, err := c.Call(context.Background(), "shout", json.RawMessage(`{"text":"sama"}`))
		if err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if res.Content != "SAMA" {
			t.Errorf("%s: shout returned %q", m.Name, res.Content)
		}

		res, err = c.Call(context.Background(), "count_lines", json.RawMessage(`{"path":"hilang.txt"}`))
		if err != nil {
			t.Fatalf("%s: a missing file aborted the call: %v", m.Name, err)
		}
		if !res.IsError {
			t.Errorf("%s: a missing file was reported as success", m.Name)
		}
	}
}

// TestJavaScriptConsoleLogDoesNotCorruptTheStream covers the failure mode the
// SDK exists to prevent: stdout carries the protocol, and one stray log line
// would break every plugin that ships with debug output left in.
func TestJavaScriptConsoleLogDoesNotCorruptTheStream(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	sdk, err := filepath.Abs("../../sdk/typescript/index.mjs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "chatty.mjs")
	source := `import { definePlugin } from ` + strconv(sdk) + `;
console.log("a chatty plugin logging on startup");
definePlugin({
  name: "chatty",
  tools: [{
    name: "ping",
    schema: { type: "object" },
    run: () => { console.log("logging mid-call"); return "pong"; },
  }],
});
`
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Start(context.Background(), Options{Command: []string{"node", script}, Workspace: dir})
	if err != nil {
		t.Fatalf("console.log broke the handshake: %v", err)
	}
	defer c.Close()

	res, err := c.Call(context.Background(), "ping", nil)
	if err != nil {
		t.Fatalf("console.log during a call broke the stream: %v", err)
	}
	if res.Content != "pong" {
		t.Errorf("got %q", res.Content)
	}
	if tail := c.stderrTail(); !strings.Contains(tail, "chatty plugin logging") {
		t.Errorf("the log line should have been redirected to stderr, got: %s", tail)
	}
}

// strconv renders a path as a JS string literal.
func strconv(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
