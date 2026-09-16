//go:build !linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Non-Linux builds retain the existing execution behaviour. The caller marks
// this explicitly as unavailable rather than claiming a sandbox exists.
func runPluginWorker(_ string, argv []string, _ bool, sandboxed bool) error {
	if sandboxed {
		return fmt.Errorf("plugin-worker: kernel sandbox is unavailable on this platform")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
