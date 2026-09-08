//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests prove the sandbox actually denies access rather than merely being
// invoked. Landlock is irreversible and thread-scoped, so each case runs in a
// fresh child process: the test binary re-executes itself, the child restricts
// itself and attempts one operation, and the parent asserts on the outcome.

const (
	envRole  = "NEMUZ_SANDBOX_TEST_ROLE"
	envAllow = "NEMUZ_SANDBOX_TEST_ALLOW"
	envWrite = "NEMUZ_SANDBOX_TEST_WRITABLE"
	envPath  = "NEMUZ_SANDBOX_TEST_PATH"
)

func TestMain(m *testing.M) {
	if os.Getenv(envRole) != "" {
		os.Exit(runChild())
	}
	os.Exit(m.Run())
}

// runChild restricts itself, performs one filesystem operation, and reports the
// outcome through its exit code: 0 allowed, 3 denied, 4 unexpected failure.
func runChild() int {
	rules := Rules{}
	if d := os.Getenv(envAllow); d != "" {
		rules.Read = strings.Split(d, ":")
	}
	if d := os.Getenv(envWrite); d != "" {
		rules.Write = strings.Split(d, ":")
	}
	if err := Restrict(rules); err != nil {
		fmt.Fprintln(os.Stderr, "restrict:", err)
		return 4
	}

	target := os.Getenv(envPath)
	var err error
	switch os.Getenv(envRole) {
	case "read":
		_, err = os.ReadFile(target)
	case "write":
		err = os.WriteFile(target, []byte("written under landlock"), 0o600)
	default:
		fmt.Fprintln(os.Stderr, "unknown role")
		return 4
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, err)
	if errors.Is(err, os.ErrPermission) {
		return 3
	}
	return 4
}

// attempt runs one confined operation in a child and returns its exit code.
func attempt(t *testing.T, role, target string, rules Rules) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(),
		envRole+"="+role,
		envPath+"="+target,
		envAllow+"="+strings.Join(rules.Read, ":"),
		envWrite+"="+strings.Join(rules.Write, ":"),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), stderr.String()
	default:
		t.Fatalf("could not run the confined child: %v", err)
		return -1, ""
	}
}

func requireLandlock(t *testing.T) {
	t.Helper()
	abi, err := LandlockABI()
	if err != nil {
		t.Skipf("kernel has no Landlock: %v", err)
	}
	t.Logf("Landlock ABI v%d", abi)
}

func TestGrantedPathIsReadable(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "allowed.txt")
	if err := os.WriteFile(file, []byte("halo"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stderr := attempt(t, "read", file, Rules{Read: []string{dir}})
	if code != 0 {
		t.Fatalf("reading a granted path was denied (exit %d): %s", code, stderr)
	}
}

// TestUngrantedPathIsDeniedByKernel is the assertion the whole security model
// rests on: the refusal comes from the kernel, not from a check in Go.
func TestUngrantedPathIsDeniedByKernel(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()

	code, stderr := attempt(t, "read", "/etc/passwd", Rules{Read: []string{dir}})
	if code == 0 {
		t.Fatal("a confined process read /etc/passwd; the sandbox is not enforcing")
	}
	if code != 3 {
		t.Fatalf("want a permission denial (exit 3), got exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "permission denied") {
		t.Errorf("denial should come from the kernel as EACCES, got: %s", stderr)
	}
}

func TestReadGrantDoesNotAllowWriting(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "new.txt")

	code, stderr := attempt(t, "write", target, Rules{Read: []string{dir}})
	if code == 0 {
		t.Fatal("a read-only grant allowed a write")
	}
	if code != 3 {
		t.Fatalf("want a permission denial (exit 3), got exit %d: %s", code, stderr)
	}
}

func TestWriteGrantAllowsWriting(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "new.txt")

	code, stderr := attempt(t, "write", target, Rules{Write: []string{dir}})
	if code != 0 {
		t.Fatalf("writing to a granted path was denied (exit %d): %s", code, stderr)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the child reported success but wrote nothing: %v", err)
	}
	if string(body) != "written under landlock" {
		t.Errorf("file contains %q", body)
	}
}

// TestEmptyRulesDenyEverything documents the deny-by-default posture: a tool
// that declares no capabilities gets no filesystem access at all.
func TestEmptyRulesDenyEverything(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(file, []byte("halo"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _ := attempt(t, "read", file, Rules{})
	if code == 0 {
		t.Fatal("a process with no granted paths still read a file")
	}
}
