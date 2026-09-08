package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

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

	c := &cobra.Command{
		Use:    "tool-worker",
		Short:  "Serve the built-in tools inside a sandbox (spawned by nemuz)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runToolWorker(cmd, workspace, sandboxed, selfCheck)
		},
	}
	c.Flags().StringVarP(&workspace, "workspace", "w", "", "the only directory tree this worker may touch")
	c.Flags().BoolVar(&sandboxed, "sandbox", true, "apply kernel restrictions before serving")
	c.Flags().StringVar(&selfCheck, "self-check", "", "after restricting, try to read this path and report the result instead of serving")
	_ = c.MarkFlagRequired("workspace")
	return c
}

func runToolWorker(cmd *cobra.Command, workspace string, sandboxed bool, selfCheck string) error {
	// The path is resolved before the sandbox closes, so the grant names the
	// same directory the tools will later open.
	root, err := plugin.CleanWorkspace(workspace)
	if err != nil {
		return err
	}

	if sandboxed {
		// From here on the kernel refuses anything outside the workspace —
		// including this process's own binary, config, and journal. Nothing
		// after this line can widen the grant; Landlock is one-way.
		if err := sandbox.Restrict(sandbox.Rules{Write: []string{root}}); err != nil {
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

	server := &plugin.Server{Name: toolWorkerName, Version: Version, Tools: tools}
	return server.Serve(cmd.Context(), os.Stdin, os.Stdout)
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
