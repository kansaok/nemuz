//go:build linux

// Seccomp filtering, layered under Landlock.
//
// Landlock answers "which files may this process touch." Seccomp answers a
// different question: "which syscalls may it make at all," regardless of which
// file or path is involved. A confined tool worker can run arbitrary programs
// once --allow-exec names them — go, make, bash, git — and Landlock's file
// rules say nothing about ptrace, mount, or loading a kernel module, because
// none of those are about a path.
//
// # Scope: a denylist, not an allowlist
//
// An allowlist would need every syscall `go build`, `bash`, `git`, and whatever
// else an operator approves might ever make — impractical to enumerate and
// certain to break real usage the first time it is tried, the same tension
// Landlock resolved by granting whole directory trees rather than individual
// binaries. So this blocks a short, deliberately chosen list of syscalls that
// have essentially no legitimate use inside a coding-agent tool call and a
// clear history of appearing in exploits: process introspection and
// injection, filesystem namespace manipulation, kernel module and code
// loading, and a handful of privileged operations. Everything else is left
// alone, ordinary networking included.
//
// What this does not do, stated rather than implied: it does not distinguish
// a raw or packet socket from an ordinary TCP connection — that needs
// inspecting socket() arguments, which roughly doubles the risk of getting the
// BPF program subtly wrong for a narrower win, so plain socket() stays
// allowed. Defense in depth, not a complete network policy.
package sandbox

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrSeccompUnsupported means this kernel cannot install a seccomp filter.
// Unlike Landlock, seccomp has no clean way to ask "would this work" without
// attempting the (one-way) install, so this is only ever discovered by trying.
var ErrSeccompUnsupported = errors.New("sandbox: seccomp filtering unavailable on this kernel")

// Syscall numbers come from golang.org/x/sys/unix, which resolves them
// correctly per GOARCH at compile time — the alternative is hand-maintaining a
// numeric table per architecture, which is exactly the kind of easy-to-typo,
// hard-to-verify-offline table this project has otherwise avoided.
//
// commonDeniedSyscalls holds every entry that exists on both architectures
// nemuz ships. A few — raw I/O port access — are x86-specific and live in
// archDeniedSyscalls (seccomp_denylist_amd64.go / _arm64.go) instead, because
// unix.SYS_IOPL simply is not a defined constant on arm64: there is no ARM
// equivalent to gate.
var commonDeniedSyscalls = []uintptr{
	uintptr(unix.SYS_PTRACE),            // process injection and introspection
	uintptr(unix.SYS_PROCESS_VM_READV),  // read another process's memory
	uintptr(unix.SYS_PROCESS_VM_WRITEV), // write another process's memory
	uintptr(unix.SYS_MOUNT),             // filesystem namespace manipulation
	uintptr(unix.SYS_UMOUNT2),
	uintptr(unix.SYS_PIVOT_ROOT),
	uintptr(unix.SYS_CHROOT), // could otherwise reframe what a path-based rule sees
	uintptr(unix.SYS_INIT_MODULE),
	uintptr(unix.SYS_FINIT_MODULE),
	uintptr(unix.SYS_DELETE_MODULE),
	uintptr(unix.SYS_KEXEC_LOAD),
	uintptr(unix.SYS_KEXEC_FILE_LOAD),
	uintptr(unix.SYS_REBOOT),
	uintptr(unix.SYS_SWAPON),
	uintptr(unix.SYS_SWAPOFF),
	uintptr(unix.SYS_ACCT),            // process accounting, no use in a tool call
	uintptr(unix.SYS_BPF),             // loading BPF programs, including ones that could tamper with this filter
	uintptr(unix.SYS_PERF_EVENT_OPEN), // a recurring exploit side-channel
	uintptr(unix.SYS_PERSONALITY),     // used to disable ASLR — a standard exploitation primitive
}

// deniedSyscalls is the full list this build enforces.
var deniedSyscalls = append(append([]uintptr{}, commonDeniedSyscalls...), archDeniedSyscalls...)

// bpfInstruction mirrors struct sock_filter: one classic-BPF instruction.
// Verified at unsafe.Sizeof 8 bytes, matching the kernel's packed layout.
type bpfInstruction struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// bpfProgram mirrors struct sock_fprog, what PR_SET_SECCOMP expects a pointer
// to. The explicit padding matters: without it, Filter would still land at
// offset 8 by Go's own alignment rules, but naming the gap makes that fact
// checkable rather than incidental.
type bpfProgram struct {
	Len    uint16
	_      [6]byte
	Filter uintptr
}

// Classic BPF opcodes and the seccomp return-value encoding this program uses.
// These are stable kernel ABI from linux/bpf_common.h, linux/filter.h and
// linux/seccomp.h, hardcoded here the same way Landlock's access bits are: as
// named constants next to the one place that needs them.
const (
	bpfLdW  = 0x00 | 0x00 | 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK = 0x05 | 0x10 | 0x00 // BPF_JMP | BPF_JEQ | BPF_K
	bpfRetK = 0x06 | 0x00        // BPF_RET | BPF_K

	// Offsets into struct seccomp_data: { int nr; __u32 arch; ... }.
	seccompDataNrOffset   = 0
	seccompDataArchOffset = 4

	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	// errnoDenied is what a filtered syscall returns to its caller: a plain
	// EPERM, so a denied call fails cleanly instead of killing the process —
	// the same choice Landlock makes by returning EACCES rather than
	// terminating whatever asked.
	errnoDenied = uint32(unix.EPERM)

	prSetSeccomp      = 22
	seccompModeFilter = 2
)

// auditArch identifies the calling convention a filter must check against, to
// refuse a 32-bit or x32 syscall entry point smuggling in a number this filter
// never examined — the classic seccomp bypass this check exists to close. The
// values are Linux's stable AUDIT_ARCH_* constants (linux/audit.h) for the two
// architectures nemuz ships.
func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xC000003E, nil // AUDIT_ARCH_X86_64
	case "arm64":
		return 0xC00000B7, nil // AUDIT_ARCH_AARCH64
	default:
		return 0, fmt.Errorf("sandbox: seccomp is not implemented for GOARCH=%s", runtime.GOARCH)
	}
}

// buildSeccompProgram assembles a linear filter: check the calling
// architecture, then check the syscall number against each denied value,
// falling through to ALLOW and jumping to ERRNO(EPERM) on a match.
//
// The instruction layout, worked out once and then simply followed:
//
//	0: load arch
//	1: if arch == expected, skip 1 (continue); else fall through to kill
//	2: return KILL_PROCESS
//	3: load syscall number
//	4..4+n-1: for each denied syscall, compare; a match jumps to the ERRNO
//	          instruction placed just after ALLOW; no match falls through
//	4+n:   return ALLOW          (the fallthrough when nothing matched)
//	4+n+1: return ERRNO(EPERM)   (the jump target for a match)
//
// Every forward jump target and distance is computed from this fixed shape,
// which is what keeps the arithmetic checkable by inspection.
func buildSeccompProgram(denied []uintptr) ([]bpfInstruction, error) {
	arch, err := auditArch()
	if err != nil {
		return nil, err
	}
	if len(denied) > 250 {
		// A classic BPF jump offset is one byte; this keeps every computed
		// jump distance far under that limit with room to spare.
		return nil, fmt.Errorf("sandbox: %d denied syscalls exceeds what a single-pass filter can jump over", len(denied))
	}

	n := len(denied)
	allowIdx := 4 + n
	denyIdx := allowIdx + 1

	prog := make([]bpfInstruction, 0, denyIdx+1)
	prog = append(prog,
		bpfInstruction{Code: bpfLdW, K: seccompDataArchOffset},
		bpfInstruction{Code: bpfJeqK, K: arch, Jt: 1, Jf: 0},
		bpfInstruction{Code: bpfRetK, K: seccompRetKillProcess},
		bpfInstruction{Code: bpfLdW, K: seccompDataNrOffset},
	)
	for i, sc := range denied {
		pos := 4 + i
		jumpToDeny := uint8(denyIdx - pos - 1)
		prog = append(prog, bpfInstruction{Code: bpfJeqK, K: uint32(sc), Jt: jumpToDeny, Jf: 0})
	}
	prog = append(prog,
		bpfInstruction{Code: bpfRetK, K: seccompRetAllow},
		bpfInstruction{Code: bpfRetK, K: seccompRetErrno | errnoDenied},
	)
	return prog, nil
}

// RestrictSyscalls installs the seccomp filter on the current thread and its
// future children.
//
// Like Landlock, this is one-way and thread-scoped: call it once, early, in a
// process dedicated to confined work, before that process does anything real.
// It sets no_new_privs itself, so it is safe to call independently of
// sandbox.Restrict — the two layers do not need a particular order relative to
// each other, only before the work they are meant to confine.
func RestrictSyscalls() error {
	prog, err := buildSeccompProgram(deniedSyscalls)
	if err != nil {
		return err
	}

	runtime.LockOSThread()

	if err := setNoNewPrivs(); err != nil {
		return err
	}

	fprog := bpfProgram{
		Len:    uint16(len(prog)),
		Filter: uintptr(unsafe.Pointer(&prog[0])),
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL,
		prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); errno != 0 {
		if errors.Is(errno, syscall.EINVAL) || errors.Is(errno, syscall.ENOSYS) {
			return fmt.Errorf("%w: %v", ErrSeccompUnsupported, errno)
		}
		return fmt.Errorf("sandbox: install seccomp filter: %w", errno)
	}
	return nil
}
