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
