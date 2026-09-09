package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kansaok/nemuz/internal/plugin"
	"github.com/kansaok/nemuz/internal/sandbox"
	"github.com/kansaok/nemuz/internal/tool"
)

// These tests check the thing that is easy to get wrong and impossible to see:
// that the sandbox is not merely computed but actually applied. A build that
// derives the right capability grants and never hands them to the kernel would
// pass every other test in this repository.

var buildOnce struct {
	sync.Once
	path string
	err  error
	out  string
}

// nemuzBinary builds the real binary, because the worker is spawned by path and
// an in-process fake would prove nothing about spawning.
func nemuzBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nemuz-bin-*")
		if err != nil {
			buildOnce.err = err
			return
		}
		bin := filepath.Join(dir, "nemuz")
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
		buildOnce.err, buildOnce.out, buildOnce.path = err, string(out), bin
	})
	if buildOnce.err != nil {
		t.Fatalf("could not build nemuz: %v\n%s", buildOnce.err, buildOnce.out)
	}
	return buildOnce.path
}

// selfCheck runs the worker's confinement probe against path.
func selfCheck(t *testing.T, workspace, path string, sandboxed bool) selfCheckResult {
	t.Helper()
	args := []string{"tool-worker", "--workspace", workspace, "--self-check", path}
	if !sandboxed {
		args = append(args, "--sandbox=false")
	}
	cmd := exec.Command(nemuzBinary(t), args...)
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the worker did not run: %v", err)
	}
	var result selfCheckResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("the worker returned %q", out)
	}
	return result
}

// useRealBinary points the toolset at a compiled nemuz for the duration of a
// test, since the test binary itself has no tool-worker command.
func useRealBinary(t *testing.T) {
	t.Helper()
	bin := nemuzBinary(t)
	original := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = original })
}

func requireLandlock(t *testing.T) {
	t.Helper()
	if !sandbox.Available() {
		t.Skip("this kernel has no Landlock")
	}
}

// TestWorkerIsConfinedByTheKernel is the assertion the README's security claim
// rests on.
func TestWorkerIsConfinedByTheKernel(t *testing.T) {
	requireLandlock(t)
	got := selfCheck(t, t.TempDir(), "/etc/hostname", true)

	if !got.Denied {
		t.Fatalf("the sandboxed worker read /etc/hostname; it is not confined (error: %q)", got.Error)
	}
	if !strings.Contains(got.Error, "permission denied") {
		t.Errorf("the refusal should be a kernel permission denial, got %q", got.Error)
	}
}

// TestUnsandboxedWorkerIsNotConfined proves the probe can fail. Without this,
// a probe that always reported "denied" would look identical.
func TestUnsandboxedWorkerIsNotConfined(t *testing.T) {
	got := selfCheck(t, t.TempDir(), "/etc/hostname", false)
	if got.Denied {
		t.Fatal("the unsandboxed worker was refused; the probe cannot tell the two apart")
	}
}

// TestConfinedWorkerStillReachesItsWorkspace guards the opposite failure: a
// sandbox that blocks everything is not a sandbox, it is a broken build.
func TestConfinedWorkerStillReachesItsWorkspace(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	inside := filepath.Join(dir, "ada.txt")
	if err := os.WriteFile(inside, []byte("isi"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := selfCheck(t, dir, inside, true)
	if got.Denied {
		t.Fatalf("the worker was refused a file inside its own workspace: %s", got.Error)
	}
}

// TestConfinedToolsServeOverTheProtocol checks the built-in tools still work
// from behind the sandbox — the whole arrangement is pointless if they do not.
func TestConfinedToolsServeOverTheProtocol(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catatan.txt"), []byte("halo"), 0o600); err != nil {
		t.Fatal(err)
	}

	client, err := plugin.Start(context.Background(), plugin.Options{
		Command:   []string{nemuzBinary(t), "tool-worker", "--workspace", dir},
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("the sandboxed worker did not start: %v", err)
	}
	defer client.Close()

	if name := client.Manifest().Name; name != toolWorkerName {
		t.Errorf("worker identifies as %q", name)
	}

	res, err := client.Call(context.Background(), "read_file", json.RawMessage(`{"path":"catatan.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content != "halo" {
		t.Fatalf("read_file returned %+v", res)
	}

	// The workspace guard still applies inside the sandbox, and it is what
	// gives the model an explanation rather than a bare EACCES.
	res, err = client.Call(context.Background(), "read_file", json.RawMessage(`{"path":"../luar.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a path outside the workspace was read")
	}
}

// TestBuiltinToolsKeepTheirNames guards a subtle regression: routing the
// built-in tools through the plugin protocol must not namespace them, or every
// existing prompt, recording, and eval scenario would stop matching.
func TestBuiltinToolsKeepTheirNames(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	useRealBinary(t)

	ts, err := buildToolset(context.Background(), toolsetOptions{Workspace: dir, Sandbox: SandboxOn})
	if err != nil {
		t.Fatalf("could not build a sandboxed toolset: %v", err)
	}
	defer ts.Close()

	names := ts.Registry.Names()
	want := []string{"read_file", "write_file", "list_dir"}
	if len(names) != len(want) {
		t.Fatalf("registered %v", names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("tool %d is %q, want %q — the built-in tools must keep their canonical names", i, names[i], n)
		}
	}
	if !strings.HasPrefix(ts.Sandbox, "landlock-") {
		t.Errorf("sandbox mode is %q, want a landlock version", ts.Sandbox)
	}
}

func TestSandboxOffIsHonestAboutIt(t *testing.T) {
	dir := t.TempDir()
	ts, err := buildToolset(context.Background(), toolsetOptions{Workspace: dir, Sandbox: SandboxOff})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	if ts.Sandbox != "off" {
		t.Errorf("sandbox mode is %q; turning it off must be visible in the journal", ts.Sandbox)
	}
	if ts.Registry.Len() != 3 {
		t.Errorf("registered %d tools", ts.Registry.Len())
	}
}

func TestUnknownSandboxModeIsRejected(t *testing.T) {
	_, err := buildToolset(context.Background(), toolsetOptions{Workspace: t.TempDir(), Sandbox: "kadang-kadang"})
	if err == nil {
		t.Fatal("an unknown sandbox mode was accepted")
	}
	if !strings.Contains(err.Error(), "on, auto, or off") {
		t.Errorf("the error should list the valid modes, got: %v", err)
	}
}

// ---------- exec inside the sandbox ----------

// TestExecCannotWriteOutsideTheWorkspace is the guarantee that has to survive
// once an agent can run commands: it may read the system and execute programs,
// and it still cannot change anything outside the directory it was pointed at.
func TestExecCannotWriteOutsideTheWorkspace(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "pwned.txt")

	client := startWorkerWithExec(t, dir, "touch")
	defer client.Close()

	// Inside: allowed.
	res, err := client.Call(context.Background(), "run_command",
		json.RawMessage(`{"command":"touch","args":["boleh.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("writing inside the workspace failed: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "boleh.txt")); err != nil {
		t.Fatalf("the file was not created: %v", err)
	}

	// Outside: refused by the kernel.
	res, err = client.Call(context.Background(), "run_command",
		json.RawMessage(`{"command":"touch","args":["`+outside+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a command wrote outside the workspace")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("the file outside the workspace exists; the sandbox leaked")
	}
	if !strings.Contains(res.Content, "Permission denied") {
		t.Errorf("the refusal should be the kernel's, got: %s", res.Content)
	}
}

// TestExecCannotReadOutsideTheWorkspace uses a world-readable file, so a
// refusal cannot be explained by ordinary Unix permissions.
func TestExecCannotReadOutsideTheWorkspace(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()

	elsewhere := filepath.Join(t.TempDir(), "terbuka.txt")
	if err := os.WriteFile(elsewhere, []byte("rahasia"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := startWorkerWithExec(t, dir, "cat")
	defer client.Close()

	res, err := client.Call(context.Background(), "run_command",
		json.RawMessage(`{"command":"cat","args":["`+elsewhere+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(res.Content, "rahasia") {
		t.Fatalf("a world-readable file outside the workspace was read: %s", res.Content)
	}

	// The same file inside the workspace is readable, which isolates the
	// sandbox as the reason rather than the file's own permissions.
	inside := filepath.Join(dir, "terbuka.txt")
	if err := os.WriteFile(inside, []byte("rahasia"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = client.Call(context.Background(), "run_command",
		json.RawMessage(`{"command":"cat","args":["terbuka.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "rahasia") {
		t.Fatalf("the same file inside the workspace was not readable: %s", res.Content)
	}
}

// TestExecToolIsAbsentWithoutPermission keeps the default honest: no
// --allow-exec, no way to run anything.
func TestExecToolIsAbsentWithoutPermission(t *testing.T) {
	requireLandlock(t)
	useRealBinary(t)
	dir := t.TempDir()

	ts, err := buildToolset(context.Background(), toolsetOptions{Workspace: dir, Sandbox: SandboxOn})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	if _, ok := ts.Registry.Get("run_command"); ok {
		t.Fatal("the exec tool was offered without --allow-exec")
	}

	withExec, err := buildToolset(context.Background(), toolsetOptions{
		Workspace: dir, Sandbox: SandboxOn, AllowExec: []string{"echo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer withExec.Close()

	if _, ok := withExec.Registry.Get("run_command"); !ok {
		t.Fatalf("--allow-exec did not add the tool: %v", withExec.Registry.Names())
	}
	// The journal has to record that confinement was widened.
	if !strings.HasSuffix(withExec.Sandbox, "+exec") {
		t.Errorf("sandbox is recorded as %q; widening it must be visible", withExec.Sandbox)
	}
}

// startWorkerWithExec launches the confined tool worker with exec permitted.
//
// Programs are resolved here, the way buildToolset does it: the worker runs
// with an empty environment and cannot look anything up for itself.
func startWorkerWithExec(t *testing.T, workspace string, names ...string) *plugin.Client {
	t.Helper()
	programs, missing := tool.Resolve(names)
	if len(missing) > 0 {
		t.Skipf("these programs are not installed here: %v", missing)
	}
	command := []string{nemuzBinary(t), "tool-worker", "--workspace", workspace}
	for name, path := range programs {
		command = append(command, "--allow-exec", name+"="+path)
	}
	client, err := plugin.Start(context.Background(), plugin.Options{Command: command, Workspace: workspace})
	if err != nil {
		t.Fatalf("the confined worker did not start: %v", err)
	}
	return client
}

// TestWorkspaceIsWritableButNotExecutable is the one separation kept once exec
// exists. An agent can write to the project it was asked to work on, and cannot
// then run what it wrote — so the repository never becomes a launcher.
//
// This is not a claim that the allowlist is a hard boundary. Allowing an
// interpreter or a compiler grants arbitrary execution by construction; what
// the sandbox still bounds is what any of it can reach.
func TestWorkspaceIsWritableButNotExecutable(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()

	script := filepath.Join(dir, "buatan-agen.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho dijalankan\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	client := startWorkerWithExec(t, dir, "sh", "cat")
	defer client.Close()

	// Readable: the agent can inspect what is in the project.
	res, err := client.Call(context.Background(), "run_command",
		json.RawMessage(`{"command":"cat","args":["buatan-agen.sh"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "dijalankan") {
		t.Fatalf("a workspace file was not readable: %s", res.Content)
	}

	// Not executable: running it directly is refused, even though the file
	// carries the execute bit and sh is allowed.
	res, err = client.Call(context.Background(), "run_command",
		json.RawMessage(`{"command":"sh","args":["-c","./buatan-agen.sh"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("a script written into the workspace was executed: %s", res.Content)
	}
}

// TestSandboxStatusReflectsWhatTheWorkerAchieved is what makes the reported
// status honest: it comes from the worker's own manifest, not a guess made by
// the host before the worker ever ran. A worker whose seccomp filter failed to
// install must not report success it never achieved.
func TestSandboxStatusReflectsWhatTheWorkerAchieved(t *testing.T) {
	requireLandlock(t)
	useRealBinary(t)
	dir := t.TempDir()

	ts, err := buildToolset(context.Background(), toolsetOptions{Workspace: dir, Sandbox: SandboxOn})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	if !strings.Contains(ts.Sandbox, "landlock-v") {
		t.Errorf("sandbox status is %q, want it to name the Landlock ABI", ts.Sandbox)
	}
	if !strings.Contains(ts.Sandbox, "seccomp") {
		t.Errorf("sandbox status is %q, want it to report seccomp on a kernel that supports it", ts.Sandbox)
	}
}
