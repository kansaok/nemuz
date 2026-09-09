//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

// archDeniedSyscalls adds x86-specific entries with no arm64 equivalent:
// direct hardware I/O port access, a classic route to bypassing every other
// protection on the machine.
var archDeniedSyscalls = []uintptr{
	uintptr(unix.SYS_IOPL),
	uintptr(unix.SYS_IOPERM),
}
