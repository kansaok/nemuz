package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kansaok/nemuz/internal/plugin"
	"github.com/kansaok/nemuz/internal/sandbox"
	"github.com/kansaok/nemuz/internal/tool"
)

// SandboxMode selects how the built-in tools are confined.
type SandboxMode string

const (
	// SandboxOn requires kernel confinement and refuses to run without it.
	SandboxOn SandboxMode = "on"
	// SandboxAuto confines where the kernel supports it and says so plainly
	// when it cannot. This is the default.
	SandboxAuto SandboxMode = "auto"
	// SandboxOff runs the tools in this process, unconfined. Choosing it is
	// deliberate, and it is recorded in the journal.
	SandboxOff SandboxMode = "off"
)

// toolset is the tools for one turn, plus how they ended up confined.
type toolset struct {
	Registry *tool.Registry
	// Sandbox describes what actually happened, and is written to the
	// journal so a turn's record says whether its tools were confined.
	Sandbox string
	// Workspace is the resolved directory the tools operate on.
	Workspace string

	closers []func()
}

// Close shuts down every process the toolset started.
func (t *toolset) Close() {
	for i := len(t.closers) - 1; i >= 0; i-- {
		t.closers[i]()
	}
}

// toolsetOptions is everything needed to assemble a turn's tools.
type toolsetOptions struct {
	Workspace  string
	Sandbox    SandboxMode
	PluginCmds []string
	AllowNet   []string
	AllowExec  []string
}

// buildToolset assembles the built-in tools and any plugins.
//
// The built-in tools go through the same plugin protocol as everything else,
// served by nemuz re-executing itself as a confined worker. That is what makes
// the sandbox real: Landlock is a thread credential, so the process that runs
// the tools has to be the process that gets restricted — and it cannot be the
// gateway, which needs the journal, the config, and the network the tools must
// not have.
func buildToolset(ctx context.Context, opts toolsetOptions) (*toolset, error) {
	root, err := plugin.CleanWorkspace(opts.Workspace)
	if err != nil {
		return nil, err
	}
	ts := &toolset{Registry: tool.NewRegistry(), Workspace: root}

	if err := ts.addBuiltins(ctx, root, opts.Sandbox); err != nil {
		ts.Close()
		return nil, err
	}

	policy := plugin.WorkspacePolicy{Workspace: root, AllowNet: opts.AllowNet, AllowExec: opts.AllowExec}
	for _, command := range opts.PluginCmds {
		client, _, err := startPlugin(ctx, command, root)
		if err != nil {
			ts.Close()
			return nil, err
		}
		ts.closers = append(ts.closers, func() { client.Close() })

		tools, err := client.Tools(policy)
		if err != nil {
			ts.Close()
			return nil, err
		}
		if err := ts.Registry.Register(tools...); err != nil {
			ts.Close()
			return nil, err
		}
	}
	return ts, nil
}

// addBuiltins registers read_file, write_file and list_dir, confined if it can be.
func (ts *toolset) addBuiltins(ctx context.Context, root string, mode SandboxMode) error {
	if mode == "" {
		mode = SandboxAuto
	}

	switch mode {
	case SandboxOff:
		ts.Sandbox = "off"
		return ts.addBuiltinsInProcess(root)

	case SandboxOn, SandboxAuto:
		abi, err := sandbox.LandlockABI()
		if err != nil {
			if mode == SandboxOn {
				return fmt.Errorf("--sandbox=on was requested but this kernel cannot enforce it: %w", err)
			}
			// Saying so is the point. A sandbox that silently is not there is
			// worse than one that is openly absent.
			ts.Sandbox = "unavailable"
			return ts.addBuiltinsInProcess(root)
		}
		if err := ts.addBuiltinsConfined(ctx, root); err != nil {
			if mode == SandboxOn {
				return err
			}
			ts.Sandbox = "unavailable"
			return ts.addBuiltinsInProcess(root)
		}
		ts.Sandbox = fmt.Sprintf("landlock-v%d", abi)
		return nil

	default:
		return fmt.Errorf("unknown sandbox mode %q; use on, auto, or off", mode)
	}
}

// addBuiltinsInProcess registers the tools directly, with no kernel confinement.
//
// The workspace guard in internal/tool still applies, so paths outside the
// workspace are refused — but by Go, not by the kernel, and a bug in that check
// is the whole blast radius.
func (ts *toolset) addBuiltinsInProcess(root string) error {
	ws, err := tool.NewWorkspace(root)
	if err != nil {
		return err
	}
	return ts.Registry.Register(tool.NewReadFile(ws), tool.NewWriteFile(ws), tool.NewListDir(ws))
}

// executablePath resolves the nemuz binary to re-execute as a tool worker.
//
// It is a variable so tests can point at a real build: inside `go test` the
// running executable is the test binary, which has no tool-worker command.
var executablePath = os.Executable

// addBuiltinsConfined spawns nemuz as its own sandboxed tool worker.
func (ts *toolset) addBuiltinsConfined(ctx context.Context, root string) error {
	self, err := executablePath()
	if err != nil {
		return fmt.Errorf("locate nemuz to spawn the tool worker: %w", err)
	}

	client, err := plugin.Start(ctx, plugin.Options{
		Command:     []string{self, "tool-worker", "--workspace", root},
		Workspace:   root,
		HostVersion: Version,
	})
	if err != nil {
		return fmt.Errorf("start the sandboxed tool worker: %w", err)
	}
	ts.closers = append(ts.closers, func() { client.Close() })

	// An empty namespace keeps the canonical names: these are the built-in
	// tools, and renaming them would change every prompt and every recording.
	tools, err := client.ToolsAs(plugin.WorkspacePolicy{Workspace: root}, "")
	if err != nil {
		return err
	}
	return ts.Registry.Register(tools...)
}
