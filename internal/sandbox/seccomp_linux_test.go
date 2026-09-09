//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Every test here re-executes the test binary as a child, restricts that
// child, and checks the outcome — seccomp is one-way and process-scoped, so
// the only honest way to prove a filter denies something is to watch a real
// process hit it.
//
// The child dispatch shares TestMain and envRole with landlock_linux_test.go,
// because a package may define TestMain only once; runChild there routes any
// "seccomp-*" role here.

// runSeccompChild installs the filter (or not, for the control case) and
// attempts one denylisted syscall, reporting the outcome through its exit
// code: 0 allowed, 3 denied, 4 unexpected failure.
func runSeccompChild(role string) int {
	switch role {
	case "seccomp-filtered":
		if err := RestrictSyscalls(); err != nil {
			fmt.Fprintln(os.Stderr, "restrict:", err)
			return 4
		}
	case "seccomp-unfiltered":
		// no filter — the control case
	default:
		fmt.Fprintln(os.Stderr, "unknown seccomp role:", role)
		return 4
	}

	_, _, errno := syscall.Syscall(uintptr(unix.SYS_PTRACE), uintptr(unix.PTRACE_TRACEME), 0, 0)
	if errno == 0 {
		return 0
	}
	if errors.Is(errno, syscall.EPERM) {
		return 3
	}
	fmt.Fprintln(os.Stderr, "unexpected errno:", errno)
	return 4
}

func attemptSeccompChild(t *testing.T, role string) (code int, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(), envRole+"="+role)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, errBuf.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), errBuf.String()
	default:
		t.Fatalf("could not run the confined child: %v", err)
		return -1, ""
	}
}

// TestFilterRefusesPtrace is the assertion the whole feature rests on: with
// the filter installed, a syscall that succeeds by default is refused by the
// kernel, not by a check in Go.
func TestFilterRefusesPtrace(t *testing.T) {
	code, stderr := attemptSeccompChild(t, "seccomp-filtered")
	if code != 3 {
		t.Fatalf("want a permission denial (exit 3), got exit %d: %s", code, stderr)
	}
}

// TestWithoutTheFilterPtraceSucceeds proves the probe can fail. Without this,
// a probe that always reported "denied" would look identical to one that
// actually worked.
func TestWithoutTheFilterPtraceSucceeds(t *testing.T) {
	code, stderr := attemptSeccompChild(t, "seccomp-unfiltered")
	if code != 0 {
		t.Fatalf("an unfiltered process was refused ptrace (exit %d): %s — the probe cannot tell the two apart", code, stderr)
	}
}

// TestOrdinarySyscallsStillWork guards the opposite failure: a filter broad
// enough to deny everything is not a seccomp filter, it is a crash.
func TestOrdinarySyscallsStillWork(t *testing.T) {
	if err := RestrictSyscalls(); err != nil {
		t.Skipf("seccomp unavailable on this kernel: %v", err)
	}
	// getpid is about as ordinary as a syscall gets, and is not on the
	// denylist; if this process is still alive to report it, it worked.
	if pid := os.Getpid(); pid <= 0 {
		t.Fatalf("getpid returned %d after installing the filter", pid)
	}
	if _, err := os.ReadFile("/proc/self/status"); err != nil {
		t.Errorf("ordinary file I/O broke under the filter: %v", err)
	}
}

func TestArchitectureIsKnown(t *testing.T) {
	arch, err := auditArch()
	if err != nil {
		t.Skipf("no audit arch mapping for this GOARCH: %v", err)
	}
	if arch == 0 {
		t.Fatal("audit arch resolved to zero")
	}
}

func TestBuildSeccompProgramShape(t *testing.T) {
	prog, err := buildSeccompProgram([]uintptr{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	// arch check (2) + kill (1) + load nr (1) + 3 checks + allow + deny(errno).
	want := 4 + 3 + 2
	if len(prog) != want {
		t.Fatalf("program has %d instructions, want %d", len(prog), want)
	}
	last := prog[len(prog)-1]
	if last.Code != bpfRetK || last.K != seccompRetErrno|errnoDenied {
		t.Errorf("last instruction is %+v, want the ERRNO(EPERM) return", last)
	}
	secondLast := prog[len(prog)-2]
	if secondLast.Code != bpfRetK || secondLast.K != seccompRetAllow {
		t.Errorf("second-to-last instruction is %+v, want the ALLOW return", secondLast)
	}
}

// TestJumpDistancesStayInByteRange guards the one-byte width of a classic BPF
// jump offset: too long a denylist would silently overflow jt/jf.
func TestJumpDistancesStayInByteRange(t *testing.T) {
	long := make([]uintptr, 250)
	for i := range long {
		long[i] = uintptr(i + 1000)
	}
	if _, err := buildSeccompProgram(long); err != nil {
		t.Fatalf("250 entries should still fit: %v", err)
	}

	tooLong := make([]uintptr, 251)
	if _, err := buildSeccompProgram(tooLong); err == nil {
		t.Fatal("a denylist too long for a one-byte jump was accepted")
	}
}

func TestDeniedSyscallsHaveNoDuplicates(t *testing.T) {
	seen := map[uintptr]bool{}
	for _, sc := range deniedSyscalls {
		if seen[sc] {
			t.Errorf("syscall %d appears twice in the denylist", sc)
		}
		seen[sc] = true
	}
	if len(deniedSyscalls) == 0 {
		t.Fatal("the denylist is empty")
	}
}
