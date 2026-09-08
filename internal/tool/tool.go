// Package tool defines what an agent can do, and what each of those things is
// allowed to touch.
//
// Every tool declares its capabilities up front. The agent computes the union
// of the tools it was given and hands that to the sandbox, which asks the kernel
// to refuse everything outside it. A tool cannot widen its own reach at run
// time, and a bug in a tool cannot reach past what its declaration allowed —
// the restriction is applied before the model ever speaks.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/kansaok/nemuz/internal/llm"
)

// Capabilities is everything a tool needs permission for.
//
// Filesystem entries are directory paths, applied recursively. Net entries are
// host or scheme://host values. Exec entries are program names.
type Capabilities struct {
	FSRead  []string `json:"fs_read,omitempty"`
	FSWrite []string `json:"fs_write,omitempty"`
	Net     []string `json:"net,omitempty"`
	Exec    []string `json:"exec,omitempty"`
}

// IsZero reports whether the tool needs no permissions at all.
func (c Capabilities) IsZero() bool {
	return len(c.FSRead) == 0 && len(c.FSWrite) == 0 && len(c.Net) == 0 && len(c.Exec) == 0
}

// Union merges capabilities, removing duplicates and sorting each list so the
// result is stable — an unstable union would change the journal digest between
// runs that are otherwise identical.
func Union(caps ...Capabilities) Capabilities {
	var out Capabilities
	out.FSRead = mergeStrings(caps, func(c Capabilities) []string { return c.FSRead })
	out.FSWrite = mergeStrings(caps, func(c Capabilities) []string { return c.FSWrite })
	out.Net = mergeStrings(caps, func(c Capabilities) []string { return c.Net })
	out.Exec = mergeStrings(caps, func(c Capabilities) []string { return c.Exec })
	return out
}

func mergeStrings(caps []Capabilities, pick func(Capabilities) []string) []string {
	seen := map[string]bool{}
	for _, c := range caps {
		for _, v := range pick(c) {
			if v != "" {
				seen[v] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Result is what a tool returns to the model.
//
// A failed tool reports IsError rather than returning a Go error: the model
// should see the failure and decide what to do about it, the same way it sees a
// successful result. Go errors are reserved for failures the model cannot act
// on, such as the sandbox refusing the call.
type Result struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Errorf builds a failed Result.
func Errorf(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Tool is one capability the agent can invoke.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema for the tool's arguments.
	Schema() json.RawMessage
	// Capabilities reports what this tool must be allowed to touch.
	Capabilities() Capabilities
	// Run executes the tool. Returning a Go error aborts the turn; returning
	// a Result with IsError set lets the model recover.
	Run(ctx context.Context, args json.RawMessage) (Result, error)
}

// Registry holds the tools available to a turn.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Tool)}
}

// Register adds t. Registering two tools under one name is a programming error,
// not a run-time condition, because the model would have no way to disambiguate.
func (r *Registry) Register(tools ...Tool) error {
	for _, t := range tools {
		name := t.Name()
		if name == "" {
			return fmt.Errorf("tool: registered a tool with no name")
		}
		if _, dup := r.byName[name]; dup {
			return fmt.Errorf("tool: %q is already registered", name)
		}
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return nil
}

// MustRegister is Register for package-level setup, panicking on conflict.
func (r *Registry) MustRegister(tools ...Tool) {
	if err := r.Register(tools...); err != nil {
		panic(err)
	}
}

// Get returns the tool named name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Len reports how many tools are registered.
func (r *Registry) Len() int { return len(r.order) }

// Names returns the registered names in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Defs renders the registry as tool definitions for a model request.
func (r *Registry) Defs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		defs = append(defs, llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return defs
}

// Capabilities returns the union of every registered tool's needs. This is what
// the sandbox is built from.
func (r *Registry) Capabilities() Capabilities {
	all := make([]Capabilities, 0, len(r.order))
	for _, name := range r.order {
		all = append(all, r.byName[name].Capabilities())
	}
	return Union(all...)
}

// cleanDir normalises a directory path for a capability declaration.
func cleanDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("tool: resolve %q: %w", dir, err)
	}
	return filepath.Clean(abs), nil
}
