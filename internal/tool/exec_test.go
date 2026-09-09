package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func execTool(t *testing.T, allowed ...string) (*RunCommand, *Workspace) {
	t.Helper()
	ws := newWS(t)
	programs, missing := Resolve(allowed)
	if len(missing) > 0 {
		t.Skipf("these programs are not installed here: %v", missing)
	}
	return NewRunCommand(ws, programs), ws
}

func TestRunsAnAllowedProgram(t *testing.T) {
	tool, _ := execTool(t, "echo")
	res := run(t, tool, map[string]any{"command": "echo", "args": []string{"halo", "dunia"}})
	if res.IsError {
		t.Fatalf("running echo failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "halo dunia") {
		t.Errorf("output is %q", res.Content)
	}
}

func TestRefusesAProgramNotOnTheList(t *testing.T) {
	tool, _ := execTool(t, "echo")
	res := run(t, tool, map[string]any{"command": "rm", "args": []string{"-rf", "/"}})
	if !res.IsError {
		t.Fatal("a program outside the allowlist ran")
	}
	// The refusal has to say what *is* allowed, or the model guesses again.
	if !strings.Contains(res.Content, "echo") {
		t.Errorf("the refusal does not list what is allowed: %s", res.Content)
	}
}

// TestEmptyAllowlistRefusesEverything covers the misconfiguration: a caller
// that forgot to allow anything gets a tool that does nothing, not one that
// does anything.
func TestEmptyAllowlistRefusesEverything(t *testing.T) {
	tool, _ := execTool(t)
	res := run(t, tool, map[string]any{"command": "echo", "args": []string{"halo"}})
	if !res.IsError {
		t.Fatal("a tool with no allowlist ran a command")
	}
	if !tool.Capabilities().IsZero() {
		t.Errorf("a tool that can run nothing asked for %+v", tool.Capabilities())
	}
}

// TestPathsAreRefusedAsProgramNames stops the allowlist being sidestepped by
// naming a location instead of a program.
func TestPathsAreRefusedAsProgramNames(t *testing.T) {
	tool, _ := execTool(t, "echo")
	for _, name := range []string{"/bin/echo", "./echo", "../echo", `C:\echo`} {
		res := run(t, tool, map[string]any{"command": name})
		if !res.IsError {
			t.Errorf("%q was accepted as a program name", name)
		}
	}
}

// TestThereIsNoShell is the design decision, checked rather than trusted: the
// arguments go to the operating system directly, so shell syntax is inert.
func TestThereIsNoShell(t *testing.T) {
	tool, ws := execTool(t, "echo")

	res := run(t, tool, map[string]any{
		"command": "echo",
		"args":    []string{"halo > diretas.txt && rm -rf /"},
	})
	if res.IsError {
		t.Fatalf("echo failed: %s", res.Content)
	}
	// The whole thing was one argument, printed literally.
	if !strings.Contains(res.Content, "halo > diretas.txt && rm -rf /") {
		t.Errorf("output is %q", res.Content)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "diretas.txt")); !os.IsNotExist(err) {
		t.Fatal("the redirection was interpreted; there is a shell after all")
	}
}

func TestNonZeroExitIsAResultNotACrash(t *testing.T) {
	tool, _ := execTool(t, "false")
	res, err := tool.Run(context.Background(), json.RawMessage(`{"command":"false"}`))
	if err != nil {
		t.Fatalf("a failing command aborted the turn: %v", err)
	}
	if !res.IsError {
		t.Fatal("a non-zero exit was reported as success")
	}
	if !strings.Contains(res.Content, "exited 1") {
		t.Errorf("the exit code is not reported: %s", res.Content)
	}
}

func TestCommandsRunInTheWorkspace(t *testing.T) {
	tool, ws := execTool(t, "pwd")
	res := run(t, tool, map[string]any{"command": "pwd"})
	if res.IsError {
		t.Fatalf("pwd failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, ws.Root()) {
		t.Errorf("ran in the wrong directory: %s", res.Content)
	}
}

func TestSubdirectoryIsAllowedButEscapeIsNot(t *testing.T) {
	tool, _ := execTool(t, "pwd")

	res := run(t, tool, map[string]any{"command": "pwd", "dir": "sub"})
	if res.IsError {
		t.Errorf("running in a workspace subdirectory failed: %s", res.Content)
	}

	res = run(t, tool, map[string]any{"command": "pwd", "dir": "../.."})
	if !res.IsError {
		t.Fatal("a command ran outside the workspace")
	}
}

func TestTimeoutStopsAStuckCommand(t *testing.T) {
	tool, _ := execTool(t, "sleep")
	tool.SetTimeout(200 * time.Millisecond)

	started := time.Now()
	res := run(t, tool, map[string]any{"command": "sleep", "args": []string{"30"}})
	if !res.IsError {
		t.Fatal("a command that outran its timeout reported success")
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("the failure does not say it timed out: %s", res.Content)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the timeout took %v to take effect", elapsed)
	}
}

// TestLargeOutputIsTruncatedFromTheFront keeps a build log from filling a
// model's context, and keeps the end — which is where a failure explains itself.
func TestLargeOutputIsTruncatedFromTheFront(t *testing.T) {
	long := strings.Repeat("x", maxExecOutput+5000)
	got := truncate(long)
	if len(got) > maxExecOutput+200 {
		t.Fatalf("truncated output is %d bytes", len(got))
	}
	if !strings.Contains(got, "omitted") {
		t.Error("truncation is silent; a reader cannot tell the log was cut")
	}
	if !strings.HasSuffix(got, "x") {
		t.Error("the end was dropped, which is the part that explains a failure")
	}
}

func TestCapabilitiesStayConfinedToTheWorkspace(t *testing.T) {
	tool, ws := execTool(t, "make", "go")
	caps := tool.Capabilities()

	if len(caps.FSWrite) != 1 || caps.FSWrite[0] != ws.Root() {
		t.Errorf("write grant is %v; exec must not widen where a command can write", caps.FSWrite)
	}
	if len(caps.Exec) != 2 || caps.Exec[0] != "go" || caps.Exec[1] != "make" {
		t.Errorf("exec grant is %v, want the allowlist sorted", caps.Exec)
	}
}

// TestResolveReportsWhatIsNotInstalled matters at startup: an operator who
// approved a program they do not have should hear so then, not when the model
// reaches for it mid-turn.
func TestResolveReportsWhatIsNotInstalled(t *testing.T) {
	found, missing := Resolve([]string{"echo", "program-yang-pasti-tidak-ada"})
	if len(missing) != 1 || missing[0] != "program-yang-pasti-tidak-ada" {
		t.Fatalf("missing is %v", missing)
	}
	if path, ok := found["echo"]; !ok || !filepath.IsAbs(path) {
		t.Fatalf("echo resolved to %q", path)
	}
}

// TestResolveFindsToolchainsOutsideSystemPaths covers the case that made this
// necessary: Go, node and cargo are usually installed under a home directory,
// nowhere near /usr/bin.
func TestResolveFindsToolchainsOutsideSystemPaths(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "alat-khusus")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	found, missing := Resolve([]string{"alat-khusus"})
	if len(missing) > 0 {
		t.Fatalf("a program on PATH was reported missing: %v", missing)
	}
	if found["alat-khusus"] != fake {
		t.Errorf("resolved to %q, want %q", found["alat-khusus"], fake)
	}
	if dirs := ProgramDirs(found); len(dirs) != 1 || dirs[0] != dir {
		t.Errorf("program dirs are %v, want the directory it was found in", dirs)
	}
}

// TestProgramDirsIncludesTheToolchainRoot covers what a bin directory alone
// misses: `go vet` runs go/pkg/tool/.../vet, and granting only go/bin finds the
// entry point then fails on the first thing it calls.
func TestProgramDirsIncludesTheToolchainRoot(t *testing.T) {
	dirs := ProgramDirs(map[string]string{"go": "/home/x/.local/go/bin/go"})

	var sawBin, sawRoot bool
	for _, d := range dirs {
		switch d {
		case "/home/x/.local/go/bin":
			sawBin = true
		case "/home/x/.local/go":
			sawRoot = true
		}
	}
	if !sawBin || !sawRoot {
		t.Fatalf("dirs are %v, want both the bin directory and the toolchain root", dirs)
	}

	// A program that is not under a bin directory contributes only its own.
	dirs = ProgramDirs(map[string]string{"alat": "/opt/alat"})
	if len(dirs) != 1 || dirs[0] != "/opt" {
		t.Errorf("dirs are %v", dirs)
	}
}

// TestSearchPathIncludesWhereProgramsWereFound is what lets one command find
// another: make has to be able to run go, not only nemuz.
func TestSearchPathIncludesWhereProgramsWereFound(t *testing.T) {
	ws := newWS(t)
	tool := NewRunCommand(ws, map[string]string{"khusus": "/opt/aneh/bin/khusus"})
	if !strings.Contains(tool.searchPath(), "/opt/aneh/bin") {
		t.Errorf("search path is %q", tool.searchPath())
	}
	if !strings.Contains(tool.searchPath(), "/usr/bin") {
		t.Error("the system directories were dropped from the search path")
	}
}

func TestDescriptionNamesWhatIsAllowed(t *testing.T) {
	tool, _ := execTool(t, "go", "make")
	if !strings.Contains(tool.Description(), "go, make") {
		t.Errorf("the model is not told what it may run: %s", tool.Description())
	}
	if !strings.Contains(tool.Description(), "no shell") {
		t.Error("the model is not told there is no shell, so it will try pipes")
	}

	empty, _ := execTool(t)
	if !strings.Contains(empty.Description(), "No programs") {
		t.Errorf("an unusable tool does not say so: %s", empty.Description())
	}
}
