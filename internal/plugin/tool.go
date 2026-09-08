package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kansaok/nemuz/internal/tool"
)

// Policy decides what a plugin's tool is actually allowed to touch.
//
// A plugin's declared capabilities are a request. This is where the request is
// answered. Keeping the decision on the host side is the whole point: a plugin
// that could grant itself access would make the capability model decorative.
type Policy interface {
	// Review returns the capabilities to grant, or an error to refuse the
	// tool entirely. The returned set may be narrower than what was asked for.
	Review(pluginName string, spec ToolSpec) (tool.Capabilities, error)
}

// WorkspacePolicy is the default: filesystem access is clamped to the
// workspace, and network and process execution are denied unless the operator
// listed them.
//
// A plugin asking for "/" gets the workspace. A plugin asking for the network
// gets nothing back unless the operator said so — and it is told which host it
// asked for, so the refusal is actionable rather than mysterious.
type WorkspacePolicy struct {
	// Workspace is the only directory tree plugin tools may touch.
	Workspace string
	// AllowNet lists network destinations the operator has approved.
	AllowNet []string
	// AllowExec lists programs the operator has approved.
	AllowExec []string
	// DenyWrite refuses write access to every plugin, for read-only sessions.
	DenyWrite bool
}

// Review implements Policy.
func (p WorkspacePolicy) Review(pluginName string, spec ToolSpec) (tool.Capabilities, error) {
	granted := tool.Capabilities{}

	if len(spec.Capabilities.FSRead) > 0 || len(spec.Capabilities.FSWrite) > 0 {
		if p.Workspace == "" {
			return tool.Capabilities{}, fmt.Errorf("plugin %s: tool %q wants filesystem access but no workspace is configured", pluginName, spec.Name)
		}
	}
	if len(spec.Capabilities.FSRead) > 0 {
		granted.FSRead = []string{p.Workspace}
	}
	if len(spec.Capabilities.FSWrite) > 0 {
		if p.DenyWrite {
			return tool.Capabilities{}, fmt.Errorf("plugin %s: tool %q wants to write, but this session is read-only", pluginName, spec.Name)
		}
		granted.FSRead = []string{p.Workspace}
		granted.FSWrite = []string{p.Workspace}
	}

	for _, want := range spec.Capabilities.Net {
		if !allowed(want, p.AllowNet) {
			return tool.Capabilities{}, fmt.Errorf("plugin %s: tool %q wants network access to %q, which is not allowed", pluginName, spec.Name, want)
		}
		granted.Net = append(granted.Net, want)
	}
	for _, want := range spec.Capabilities.Exec {
		if !allowed(want, p.AllowExec) {
			return tool.Capabilities{}, fmt.Errorf("plugin %s: tool %q wants to run %q, which is not allowed", pluginName, spec.Name, want)
		}
		granted.Exec = append(granted.Exec, want)
	}

	return tool.Union(granted), nil
}

func allowed(want string, list []string) bool {
	for _, v := range list {
		if v == want || v == "*" {
			return true
		}
	}
	return false
}

// Tools adapts the plugin's declared tools into registry tools, applying policy.
//
// Names are prefixed with the plugin's own name so two plugins offering a
// "search" cannot collide, and so an operator reading a journal can see which
// plugin ran.
func (c *Client) Tools(policy Policy) ([]tool.Tool, error) {
	if policy == nil {
		return nil, fmt.Errorf("plugin %s: no policy given; capabilities must be reviewed", c.manifest.Name)
	}
	out := make([]tool.Tool, 0, len(c.manifest.Tools))
	for _, spec := range c.manifest.Tools {
		granted, err := policy.Review(c.manifest.Name, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, &pluginTool{
			client:  c,
			spec:    spec,
			granted: granted,
			name:    qualify(c.manifest.Name, spec.Name),
		})
	}
	return out, nil
}

// qualify namespaces a plugin's tool name.
func qualify(plugin, toolName string) string {
	plugin = strings.TrimSpace(plugin)
	if plugin == "" {
		return toolName
	}
	return plugin + "__" + toolName
}

// pluginTool is one of a plugin's tools, seen from the host.
type pluginTool struct {
	client  *Client
	spec    ToolSpec
	granted tool.Capabilities
	name    string
}

func (t *pluginTool) Name() string                    { return t.name }
func (t *pluginTool) Description() string             { return t.spec.Description }
func (t *pluginTool) Schema() json.RawMessage         { return t.spec.Schema }
func (t *pluginTool) Capabilities() tool.Capabilities { return t.granted }

// Run forwards the call to the plugin process.
func (t *pluginTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	result, err := t.client.Call(ctx, t.spec.Name, args)
	if err != nil {
		// A transport or protocol failure is not something the model can act
		// on, so it aborts the turn rather than being fed back as a result.
		return tool.Result{}, err
	}
	return tool.Result{Content: result.Content, IsError: result.IsError}, nil
}

// CleanWorkspace normalises a workspace path for a policy.
func CleanWorkspace(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("plugin: resolve workspace %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("plugin: workspace %s: %w", abs, err)
	}
	return resolved, nil
}
