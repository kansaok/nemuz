package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kansaok/nemuz/internal/plugin"
	"github.com/kansaok/nemuz/internal/sandbox"
	"github.com/kansaok/nemuz/internal/tool"
	"github.com/spf13/cobra"
)

// toolWorkerName is how the built-in tool process identifies itself. It is
// deliberately not prefixed onto tool names — see registerBuiltinTools.
const toolWorkerName = "builtin"

// toolWorkerCmd is nemuz confining itself to run its own tools.
//
// It is hidden because nobody types it: the gateway spawns it. It exists
// because Landlock is a thread credential, so restricting tool execution needs
// a process whose whole job is to be restricted. Applying Landlock to the
// gateway instead would confine the journal, the config, and the provider
// connection along with the tools, which is the opposite of useful.
func toolWorkerCmd() *cobra.Command {
	var workspace string
	var sandboxed bool
	var selfCheck string
	var allowExec []string
	var scratch string

	c := &cobra.Command{
		Use:    "tool-worker",
		Short:  "Serve the built-in tools inside a sandbox (spawned by nemuz)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runToolWorker(cmd, workspace, sandboxed, selfCheck, allowExec, scratch)
		},
	}
	c.Flags().StringVarP(&workspace, "workspace", "w", "", "the only directory tree this worker may touch")
	c.Flags().BoolVar(&sandboxed, "sandbox", true, "apply kernel restrictions before serving")
	c.Flags().StringVar(&selfCheck, "self-check", "", "after restricting, try to read this path and report the result instead of serving")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program the agent may run, as name=/absolute/path; repeatable")
	c.Flags().StringVar(&scratch, "scratch", "", "private directory commands may use for temporary files")
	_ = c.MarkFlagRequired("workspace")
	return c
}

func runToolWorker(cmd *cobra.Command, workspace string, sandboxed bool, selfCheck string, allowExec []string, scratch string) error {
	// The path is resolved before the sandbox closes, so the grant names the
	// same directory the tools will later open.
	root, err := plugin.CleanWorkspace(workspace)
	if err != nil {
		return err
	}

	programs, err := parsePrograms(allowExec)
	if err != nil {
		return err
	}

	if sandboxed {
		rules := sandbox.Rules{Write: []string{root}}
		if len(programs) > 0 {
			// Running anything means reading the loader and the libraries it
			// pulls in, so exec widens the sandbox to read the system. What it
			// does not widen is Write: the workspace stays the only place this
			// process can change.
			rules.Read = append(rules.Read, sandbox.SystemReadDirs...)
			rules.Execute = append(rules.Execute, sandbox.SystemExecDirs...)
			// Toolchains installed under a home directory are nowhere near
			// the system paths, so the directories the approved programs were
			// actually found in are granted too.
			rules.Execute = append(rules.Execute, existingPaths(tool.ProgramDirs(programs))...)
			// Hostname lookup reads files that are often symlinks pointing
			// out of /etc entirely.
			rules.Read = append(rules.Read, sandbox.ResolvedPaths(sandbox.ResolverFiles)...)
			if scratch != "" {
				// Somewhere to write that is neither the project nor /tmp.
				// It is executable as well, because compiling and then running
				// is the ordinary shape of `go test` and `cargo test`.
				//
				// The workspace deliberately is not: keeping the project
				// writable but not executable stops an agent turning the
				// repository it was asked to check into a launcher.
				rules.Write = append(rules.Write, scratch)
				rules.Execute = append(rules.Execute, scratch)
			}
			// Named individually rather than granting /dev, which also holds
			// disks and memory.
			rules.Write = append(rules.Write, existingPaths(sandbox.SystemDevices)...)
		}
		// From here on the kernel refuses anything outside those grants —
		// including this process's own config and journal. Nothing after this
		// line can widen them; Landlock is one-way.
		if err := sandbox.Restrict(rules); err != nil {
			return fmt.Errorf("tool-worker: %w", err)
		}
	}

	if selfCheck != "" {
		return reportSelfCheck(cmd.OutOrStdout(), selfCheck)
	}

	ws, err := tool.NewWorkspace(root)
	if err != nil {
		return err
	}
	tools := tool.NewRegistry()
	if err := tools.Register(tool.NewReadFile(ws), tool.NewWriteFile(ws), tool.NewListDir(ws)); err != nil {
		return err
	}
	if len(programs) > 0 {
		runner := tool.NewRunCommand(ws, programs)
		runner.SetScratch(scratch)
		if err := tools.Register(runner); err != nil {
			return err
		}
	}

	server := &plugin.Server{Name: toolWorkerName, Version: Version, Tools: tools}
	return server.Serve(cmd.Context(), os.Stdin, os.Stdout)
}

// parsePrograms reads the name=path pairs the host resolved for us.
//
// The host does the resolving because it has the operator's environment; this
// process was started with none, deliberately, and could not find a toolchain
// under a home directory.
func parsePrograms(pairs []string) (map[string]string, error) {
	programs := map[string]string{}
	for _, pair := range pairs {
		name, path, ok := strings.Cut(pair, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("tool-worker: --allow-exec wants name=/absolute/path, got %q", pair)
		}
		programs[name] = path
	}
	return programs, nil
}

// existingPaths filters to the paths that are actually present.
//
// Landlock refuses a rule for a path it cannot open, so a system without
// /dev/random would otherwise make the whole ruleset fail to build.
func existingPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// selfCheckResult is what a confinement probe reports back to the host.
type selfCheckResult struct {
	// Denied is true when the kernel refused the read.
	Denied bool   `json:"denied"`
	Error  string `json:"error,omitempty"`
}

// reportSelfCheck attempts a read that the sandbox should refuse and reports
// what happened.
//
// This is how `nemuz doctor` can say the sandbox works on this machine rather
// than only that the kernel claims to support it. The two are not the same: a
// build could compute the right grants and never apply them, which is exactly
// the bug this probe exists to catch.
func reportSelfCheck(out io.Writer, path string) error {
	var result selfCheckResult
	if _, err := os.ReadFile(path); err != nil {
		result.Denied = errors.Is(err, os.ErrPermission)
		result.Error = err.Error()
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = out.Write(append(body, '\n'))
	return err
}
