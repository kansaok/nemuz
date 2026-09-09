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
	"path/filepath"
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

// SystemReadDirs are directories a program reads without running anything from
// them — configuration the loader and ordinary tools consult.
var SystemReadDirs = []string{"/etc"}

// SystemExecDirs get read and execute when the agent is allowed to run
// commands.
//
// The library directories are here, not in SystemReadDirs, and that surprised
// me: granting execute on /usr/bin — or even on /usr/bin/ls exactly — is not
// enough to run it. A dynamically linked program is started by its ELF
// interpreter, and the kernel needs execute on the interpreter too. Read is not
// enough. Since the interpreter lives under the library directories, allowing
// exec at all means allowing execute across the system tree.
//
// That is a real loosening, and enumerating binaries would not avoid it. A
// command also spawns helpers — make runs sh, sh runs cc, go runs the linker —
// so a narrow list ends with the sandbox switched off entirely, which is worse
// than a wide grant.
//
// What this does not widen is where the process may write. Even with exec fully
// allowed, the only writable paths stay the workspace and a handful of device
// files, so a command can build and test and still cannot change anything else
// on the machine.
var SystemExecDirs = []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"}

// ResolverFiles are the files a program reads to look up a hostname.
//
// Granting /etc is not enough for them. On WSL /etc/resolv.conf is a symlink to
// /mnt/wsl/resolv.conf, and under systemd-resolved it points into /run —
// Landlock follows the link to the real inode, finds it outside every grant,
// and the resolver silently falls back to localhost. The failure surfaces as a
// DNS error that has nothing to do with DNS.
//
// ResolvedPaths turns these into the locations they actually point at.
var ResolverFiles = []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf"}

// ResolvedPaths follows symlinks and keeps only the targets that exist.
//
// Landlock refuses a rule for a path it cannot open, so one missing file would
// otherwise make the whole ruleset fail to build.
func ResolvedPaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}

// SystemDevices are the device files ordinary programs expect to exist.
//
// They are named individually rather than granting /dev, which holds disks,
// memory and terminals. /dev/null and /dev/zero need writing as well as
// reading, because a program that opens null for output is doing the normal
// thing.
var SystemDevices = []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random"}

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
			if err := addPathRule(rulesetFD, path, g.access, abi); err != nil {
				return err
			}
		}
	}

	if _, _, errno := syscall.Syscall(sysRestrictSelf, uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: restrict self: %w", errno)
	}
	return nil
}

// fileAccess is the subset of rights that mean anything for a regular file.
//
// The rest — making and removing entries, reading a directory — describe
// operations only a directory can have, and Landlock rejects a rule that claims
// them for a file. Granting /dev/null with the directory set is exactly that
// mistake, and it fails the whole ruleset rather than the one rule.
func fileAccess(abi int) uint64 {
	a := accessExecute | accessReadFile | accessWriteFile
	if abi >= 3 {
		a |= accessTruncate
	}
	return a
}

// addPathRule grants access beneath one path.
//
// A path may be a directory or a single file. Naming a file is how a narrow
// grant is expressed — /dev/null rather than all of /dev — so the access is
// masked to what a file can actually have.
func addPathRule(rulesetFD int, path string, access uint64, abi int) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sandbox: open %s for a rule: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sandbox: inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		access &= fileAccess(abi)
		if access == 0 {
			return fmt.Errorf("sandbox: %s is a file, and none of the requested access applies to files", path)
		}
	}

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
