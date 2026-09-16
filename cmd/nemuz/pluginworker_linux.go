//go:build linux

package main

import (
	"fmt"
	"path/filepath"
	"syscall"

	"github.com/kansaok/nemuz/internal/plugin"
	"github.com/kansaok/nemuz/internal/sandbox"
	"github.com/kansaok/nemuz/internal/tool"
)

func runPluginWorker(workspace string, argv []string, allowNet, sandboxed bool) error {
	root, err := plugin.CleanWorkspace(workspace)
	if err != nil {
		return err
	}
	program, err := filepath.Abs(argv[0])
	if err != nil {
		return fmt.Errorf("plugin-worker: resolve program: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(program); err == nil {
		program = resolved
	}
	if sandboxed {
		if err := sandbox.RestrictSyscallsWithNetwork(allowNet); err != nil {
			return fmt.Errorf("plugin-worker: seccomp: %w", err)
		}
		rules := sandbox.Rules{
			// Plugins commonly need to inspect and update the project. The
			// workspace is deliberately the sole mutable tree; manifest claims
			// cannot be trusted before the plugin has been started.
			Write:   []string{root},
			Read:    append([]string{}, sandbox.SystemReadDirs...),
			Execute: append([]string{}, sandbox.SystemExecDirs...),
		}
		rules.Read = append(rules.Read, sandbox.ResolvedPaths(sandbox.ResolverFiles)...)
		for _, dir := range tool.ProgramDirs(map[string]string{"plugin": program}) {
			rules.Read = append(rules.Read, dir)
			rules.Execute = append(rules.Execute, dir)
		}
		if err := sandbox.Restrict(rules); err != nil {
			return fmt.Errorf("plugin-worker: landlock: %w", err)
		}
	}
	argv[0] = program
	return syscall.Exec(program, argv, []string{})
}
