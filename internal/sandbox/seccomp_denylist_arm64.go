//go:build linux && arm64

package sandbox

// archDeniedSyscalls is empty on arm64: the x86-specific I/O port syscalls
// (iopl, ioperm) this list adds on amd64 do not exist here — ARM has no
// equivalent instruction-level port I/O to gate.
var archDeniedSyscalls = []uintptr{}
