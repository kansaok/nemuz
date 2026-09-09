//go:build linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// attemptPtrace calls ptrace(PTRACE_TRACEME), a request with no legitimate use
// in a tool call and one that succeeds by default when nothing restricts it —
// which is what makes it a clean probe: any failure at all means something is
// refusing it, and under this filter that something is seccomp.
func attemptPtrace() error {
	_, _, errno := syscall.Syscall(uintptr(unix.SYS_PTRACE), uintptr(unix.PTRACE_TRACEME), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
