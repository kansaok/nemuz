package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kansaok/nemuz/internal/config"

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
	// Scratch is the private directory commands may write to, when exec is
	// allowed.
	Scratch string
	// MissingPrograms are approved programs that are not installed. Reported
	// at startup rather than discovered when the model reaches for one.
	MissingPrograms []string

	closers []func()
}

// makeScratch creates the private directory commands write their temporary
// files into.
//
// It lives under the state directory rather than in the project, so a build
// leaves no litter in the repository it was asked to check, and rather than
// /tmp, which is shared with every other process on the machine.
func (ts *toolset) makeScratch() (string, error) {
	paths, err := config.Resolve()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(paths.Root, "scratch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the command scratch directory: %w", err)
	}
	ts.Scratch = dir
	return dir, nil
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

	if err := ts.addBuiltins(ctx, root, opts.Sandbox, opts.AllowExec); err != nil {
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
func (ts *toolset) addBuiltins(ctx context.Context, root string, mode SandboxMode, allowExec []string) error {
	if mode == "" {
		mode = SandboxAuto
	}

	switch mode {
	case SandboxOff:
		ts.Sandbox = "off"
		return ts.addBuiltinsInProcess(root, allowExec)

	case SandboxOn, SandboxAuto:
		abi, err := sandbox.LandlockABI()
		if err != nil {
			if mode == SandboxOn {
				return fmt.Errorf("--sandbox=on was requested but this kernel cannot enforce it: %w", err)
			}
			// Saying so is the point. A sandbox that silently is not there is
			// worse than one that is openly absent.
			ts.Sandbox = "unavailable"
			return ts.addBuiltinsInProcess(root, allowExec)
		}
		if err := ts.addBuiltinsConfined(ctx, root, allowExec); err != nil {
			if mode == SandboxOn {
				return err
			}
			ts.Sandbox = "unavailable"
			return ts.addBuiltinsInProcess(root, allowExec)
		}
		ts.Sandbox = fmt.Sprintf("landlock-v%d", abi)
		if len(allowExec) > 0 {
			// Recorded in the journal, so a turn keeps evidence of how wide
			// its confinement actually was.
			ts.Sandbox += "+exec"
		}
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
func (ts *toolset) addBuiltinsInProcess(root string, allowExec []string) error {
	ws, err := tool.NewWorkspace(root)
	if err != nil {
		return err
	}
	if err := ts.Registry.Register(tool.NewReadFile(ws), tool.NewWriteFile(ws), tool.NewListDir(ws)); err != nil {
		return err
	}
	if len(allowExec) == 0 {
		return nil
	}
	programs, missing := tool.Resolve(allowExec)
	ts.MissingPrograms = missing
	if len(programs) == 0 {
		return nil
	}
	runner := tool.NewRunCommand(ws, programs)
	if scratch, err := ts.makeScratch(); err == nil {
		runner.SetScratch(scratch)
	}
	return ts.Registry.Register(runner)
}

// executablePath resolves the nemuz binary to re-execute as a tool worker.
//
// It is a variable so tests can point at a real build: inside `go test` the
// running executable is the test binary, which has no tool-worker command.
var executablePath = os.Executable

// addBuiltinsConfined spawns nemuz as its own sandboxed tool worker.
func (ts *toolset) addBuiltinsConfined(ctx context.Context, root string, allowExec []string) error {
	self, err := executablePath()
	if err != nil {
		return fmt.Errorf("locate nemuz to spawn the tool worker: %w", err)
	}

	command := []string{self, "tool-worker", "--workspace", root}
	programs, missing := tool.Resolve(allowExec)
	ts.MissingPrograms = missing
	for name, path := range programs {
		command = append(command, "--allow-exec", name+"="+path)
	}
	if len(programs) > 0 {
		scratch, err := ts.makeScratch()
		if err != nil {
			return err
		}
		command = append(command, "--scratch", scratch)
	}

	client, err := plugin.Start(ctx, plugin.Options{
		Command:     command,
		Workspace:   root,
		HostVersion: Version,
	})
	if err != nil {
		return fmt.Errorf("start the sandboxed tool worker: %w", err)
	}
	ts.closers = append(ts.closers, func() { client.Close() })

	// The policy must carry the same exec grants the operator approved, or the
	// worker's own run_command is refused by the host that just launched it
	// with those very programs.
	policy := plugin.WorkspacePolicy{Workspace: root, AllowExec: allowExec}
	// An empty namespace keeps the canonical names: these are the built-in
	// tools, and renaming them would change every prompt and every recording.
	tools, err := client.ToolsAs(policy, "")
	if err != nil {
		return err
	}
	return ts.Registry.Register(tools...)
}
