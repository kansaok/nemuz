//go:build linux

// Package sandbox applies the kernel-level restrictions that confine tool
// execution.
//
// The security model is capability-first: a tool declares what it needs, and
// the kernel — not a pattern match in Go — refuses everything else. Landlock
// provides the filesystem half of that without requiring root, which is why
// nemuz never needs to run privileged.
//
// # Where restrictions apply
//
// Landlock restricts the calling thread and every thread it later creates. Go
// multiplexes goroutines across OS threads it may have started earlier, so
// calling Restrict from an arbitrary goroutine confines only whichever thread
// happened to run it. Restrict is therefore meant to be called once, early, in
// a process dedicated to running confined work — the tool subprocess or plugin
// host — before that process starts doing anything else. Restrict enforces this
// by pinning itself to its OS thread.
package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Landlock syscall numbers. These are identical across every architecture where
// Landlock exists.
const (
	sysCreateRuleset = 444
	sysAddRule       = 445
	sysRestrictSelf  = 446
)

// createRulesetVersion asks the kernel for its ABI version instead of creating
// a ruleset.
const createRulesetVersion = 1 << 0

// ruleTypePathBeneath selects the "everything under this directory" rule shape,
// the only rule type ABI v1 offers.
const ruleTypePathBeneath = 1

// Filesystem access rights, as defined by the Landlock UAPI.
const (
	accessExecute    uint64 = 1 << 0
	accessWriteFile  uint64 = 1 << 1
	accessReadFile   uint64 = 1 << 2
	accessReadDir    uint64 = 1 << 3
	accessRemoveDir  uint64 = 1 << 4
	accessRemoveFile uint64 = 1 << 5
	accessMakeChar   uint64 = 1 << 6
	accessMakeDir    uint64 = 1 << 7
	accessMakeReg    uint64 = 1 << 8
	accessMakeSock   uint64 = 1 << 9
	accessMakeFifo   uint64 = 1 << 10
	accessMakeBlock  uint64 = 1 << 11
	accessMakeSym    uint64 = 1 << 12
	accessRefer      uint64 = 1 << 13 // ABI v2
	accessTruncate   uint64 = 1 << 14 // ABI v3
)

// ErrUnsupported means the running kernel has no usable Landlock support.
var ErrUnsupported = errors.New("sandbox: landlock unavailable on this kernel")

// LandlockABI returns the Landlock ABI version the kernel implements.
//
// Version 1 gives filesystem access control; later versions add renames across
// directories, truncation, and network rules. A kernel without Landlock returns
// ErrUnsupported, and the caller must then decide whether to fall back to
// container isolation or refuse to run tools at all.
func LandlockABI() (int, error) {
	v, _, errno := syscall.Syscall(sysCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 {
		switch errno {
		case syscall.ENOSYS, syscall.EOPNOTSUPP:
			return 0, ErrUnsupported
		default:
			return 0, fmt.Errorf("sandbox: probe landlock: %w", errno)
		}
	}
	if int(v) < 1 {
		return 0, ErrUnsupported
	}
	return int(v), nil
}

// Available reports whether tool sandboxing can be enforced here.
func Available() bool {
	_, err := LandlockABI()
	return err == nil
}

// Rules is the set of directories a confined process may touch.
//
// Anything not listed is denied. An empty Rules therefore denies all filesystem
// access, which is a valid and useful configuration for a pure-computation tool.
type Rules struct {
	// Read grants reading files and listing directories beneath each path.
	Read []string
	// Write grants creating, modifying, and deleting beneath each path.
	// Writable paths are readable too; a writer that cannot read cannot
	// meaningfully edit.
	Write []string
	// Execute grants running programs beneath each path.
	Execute []string
}

// readAccess is what a read-only grant allows.
func readAccess() uint64 { return accessReadFile | accessReadDir }

// writeAccess is what a write grant allows, limited to the rights the running
// ABI understands. Handling a right the kernel does not know causes EINVAL.
func writeAccess(abi int) uint64 {
	a := accessWriteFile | accessRemoveDir | accessRemoveFile |
		accessMakeChar | accessMakeDir | accessMakeReg |
		accessMakeSock | accessMakeFifo | accessMakeBlock | accessMakeSym
	if abi >= 2 {
		a |= accessRefer
	}
	if abi >= 3 {
		a |= accessTruncate
	}
	return a
}

// handledAccess is every right the ruleset governs. Rights that are handled but
// never granted are denied everywhere — that is how the deny-by-default posture
// is expressed.
func handledAccess(abi int) uint64 {
	return readAccess() | writeAccess(abi) | accessExecute
}

// Restrict applies r to the current process and its future children.
//
// The restriction cannot be lifted: Landlock is one-way, by design. Call it
// once, early, in a process whose only job is to run confined work.
func Restrict(r Rules) error {
	abi, err := LandlockABI()
	if err != nil {
		return err
	}

	// Landlock is a thread credential. Pinning here means the ruleset applies
	// to the thread that goes on to do the work, and to anything it spawns.
	runtime.LockOSThread()

	// Landlock refuses to restrict a process that could still gain privileges
	// through a setuid binary, so this must succeed first.
	if err := setNoNewPrivs(); err != nil {
		return err
	}

	attr := make([]byte, 8)
	binary.LittleEndian.PutUint64(attr, handledAccess(abi))
	fd, _, errno := syscall.Syscall(sysCreateRuleset,
		uintptr(unsafe.Pointer(&attr[0])), uintptr(len(attr)), 0)
	if errno != 0 {
		return fmt.Errorf("sandbox: create ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer syscall.Close(rulesetFD)

	grants := []struct {
		paths  []string
		access uint64
	}{
		{r.Read, readAccess()},
		{r.Write, readAccess() | writeAccess(abi)},
		{r.Execute, readAccess() | accessExecute},
	}
	for _, g := range grants {
		for _, path := range g.paths {
			if err := addPathRule(rulesetFD, path, g.access); err != nil {
				return err
			}
		}
	}

	if _, _, errno := syscall.Syscall(sysRestrictSelf, uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: restrict self: %w", errno)
	}
	return nil
}

// addPathRule grants access beneath one directory.
func addPathRule(rulesetFD int, path string, access uint64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sandbox: open %s for a rule: %w", path, err)
	}
	defer f.Close()

	// struct landlock_path_beneath_attr is packed: a u64 followed immediately
	// by an s32, with no alignment padding. Building the 12 bytes by hand is
	// what keeps this correct; a Go struct would be padded to 16.
	var attr [12]byte
	binary.LittleEndian.PutUint64(attr[0:8], access)
	binary.LittleEndian.PutUint32(attr[8:12], uint32(f.Fd()))

	_, _, errno := syscall.Syscall6(sysAddRule,
		uintptr(rulesetFD), ruleTypePathBeneath,
		uintptr(unsafe.Pointer(&attr[0])), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("sandbox: add rule for %s: %w", path, errno)
	}
	return nil
}

// prSetNoNewPrivs is the prctl option that forbids gaining privileges via exec.
const prSetNoNewPrivs = 38

func setNoNewPrivs() error {
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: set no_new_privs: %w", errno)
	}
	return nil
}
