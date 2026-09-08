package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/tool"
)

// buildExamplePlugin compiles the reference plugin once per test run. Testing
// against the real binary, over real pipes, is the only way to know the protocol
// works — an in-process fake would prove nothing about framing or crashes.
var buildOnce struct {
	sync.Once
	path string
	err  error
}

func examplePlugin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nemuz-plugin-*")
		if err != nil {
			buildOnce.err = err
			return
		}
		bin := filepath.Join(dir, "plugin-go")
		cmd := exec.Command("go", "build", "-o", bin, "../../examples/plugin-go")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildOnce.err = err
			buildOnce.path = string(out)
			return
		}
		buildOnce.path = bin
	})
	if buildOnce.err != nil {
		t.Fatalf("could not build the example plugin: %v\n%s", buildOnce.err, buildOnce.path)
	}
	return buildOnce.path
}

func startExample(t *testing.T, workspace string) *Client {
	t.Helper()
	c, err := Start(context.Background(), Options{
		Command:     []string{examplePlugin(t)},
		Workspace:   workspace,
		HostVersion: "test",
	})
	if err != nil {
		t.Fatalf("plugin did not start: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestHandshakeReturnsAValidManifest(t *testing.T) {
	c := startExample(t, t.TempDir())

	m := c.Manifest()
	if m.Name != "example" {
		t.Errorf("plugin name is %q", m.Name)
	}
	if m.Protocol != ProtocolVersion {
		t.Errorf("protocol is %d, want %d", m.Protocol, ProtocolVersion)
	}
	if len(m.Tools) != 2 {
		t.Fatalf("plugin declared %d tools, want 2", len(m.Tools))
	}
	if err := m.Validate(); err != nil {
		t.Errorf("the manifest the plugin sent is invalid: %v", err)
	}
}

func TestToolCallCrossesTheProcessBoundary(t *testing.T) {
	c := startExample(t, t.TempDir())

	res, err := c.Call(context.Background(), "shout", json.RawMessage(`{"text":"halo dunia"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool reported failure: %s", res.Content)
	}
	if res.Content != "HALO DUNIA" {
		t.Errorf("got %q", res.Content)
	}
}

// TestToolFailureReachesTheModel checks the distinction the protocol draws: a
// tool that fails is a result, not a crash, so the model gets a chance to
// recover instead of the turn ending.
func TestToolFailureReachesTheModel(t *testing.T) {
	c := startExample(t, t.TempDir())

	res, err := c.Call(context.Background(), "count_lines", json.RawMessage(`{"path":"tidak-ada.txt"}`))
	if err != nil {
		t.Fatalf("a failing tool aborted the call instead of returning a result: %v", err)
	}
	if !res.IsError {
		t.Fatal("a missing file was reported as success")
	}
	if !strings.Contains(res.Content, "tidak-ada.txt") {
		t.Errorf("the failure should name the file, got: %s", res.Content)
	}
}

func TestSequentialCallsStayCorrelated(t *testing.T) {
	c := startExample(t, t.TempDir())

	for i, word := range []string{"satu", "dua", "tiga", "empat"} {
		args, _ := json.Marshal(map[string]string{"text": word})
		res, err := c.Call(context.Background(), "shout", args)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Content != strings.ToUpper(word) {
			t.Fatalf("call %d returned %q, want %q — replies are crossing", i, res.Content, strings.ToUpper(word))
		}
	}
}

// ---------- capability policy ----------

// TestPolicyClampsWhatAPluginAsksFor is the security assertion of this package:
// a plugin's declaration is a request, and the host answers it.
func TestPolicyClampsWhatAPluginAsksFor(t *testing.T) {
	workspace := t.TempDir()
	policy := WorkspacePolicy{Workspace: workspace}

	greedy := ToolSpec{
		Name:   "greedy",
		Schema: json.RawMessage(`{"type":"object"}`),
		Capabilities: CapabilitySpec{
			FSRead:  []string{"/", "/etc", "/home"},
			FSWrite: []string{"/"},
		},
	}
	granted, err := policy.Review("suspicious", greedy)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted.FSRead) != 1 || granted.FSRead[0] != workspace {
		t.Errorf("read grant is %v; it should be clamped to the workspace", granted.FSRead)
	}
	if len(granted.FSWrite) != 1 || granted.FSWrite[0] != workspace {
		t.Errorf("write grant is %v; it should be clamped to the workspace", granted.FSWrite)
	}
}

func TestPolicyRefusesUnapprovedNetworkAndExec(t *testing.T) {
	policy := WorkspacePolicy{Workspace: t.TempDir()}

	for name, spec := range map[string]ToolSpec{
		"network": {Name: "fetch", Schema: json.RawMessage(`{}`), Capabilities: CapabilitySpec{Net: []string{"evil.example"}}},
		"exec":    {Name: "shell", Schema: json.RawMessage(`{}`), Capabilities: CapabilitySpec{Exec: []string{"bash"}}},
	} {
		_, err := policy.Review("p", spec)
		if err == nil {
			t.Errorf("%s: an unapproved capability was granted", name)
			continue
		}
		if !strings.Contains(err.Error(), spec.Name) {
			t.Errorf("%s: the refusal should name the tool, got: %v", name, err)
		}
	}
}

func TestPolicyGrantsWhatTheOperatorApproved(t *testing.T) {
	policy := WorkspacePolicy{
		Workspace: t.TempDir(),
		AllowNet:  []string{"api.github.com"},
		AllowExec: []string{"git"},
	}
	spec := ToolSpec{
		Name:   "clone",
		Schema: json.RawMessage(`{}`),
		Capabilities: CapabilitySpec{
			Net:  []string{"api.github.com"},
			Exec: []string{"git"},
		},
	}
	granted, err := policy.Review("gh", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted.Net) != 1 || granted.Net[0] != "api.github.com" {
		t.Errorf("net grant is %v", granted.Net)
	}
	if len(granted.Exec) != 1 || granted.Exec[0] != "git" {
		t.Errorf("exec grant is %v", granted.Exec)
	}
}

func TestReadOnlySessionRefusesWriters(t *testing.T) {
	policy := WorkspacePolicy{Workspace: t.TempDir(), DenyWrite: true}
	spec := ToolSpec{Name: "save", Schema: json.RawMessage(`{}`), Capabilities: CapabilitySpec{FSWrite: []string{"."}}}

	if _, err := policy.Review("p", spec); err == nil {
		t.Fatal("a writing tool was admitted to a read-only session")
	}
}

func TestToolsAreNamespacedAndRegisterable(t *testing.T) {
	workspace := t.TempDir()
	c := startExample(t, workspace)

	tools, err := c.Tools(WorkspacePolicy{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	if err := reg.Register(tools...); err != nil {
		t.Fatal(err)
	}

	names := reg.Names()
	if len(names) != 2 || names[0] != "example__shout" {
		t.Fatalf("tool names are %v; they should carry the plugin's name", names)
	}
	// The pure tool asked for nothing and must be granted nothing.
	shout, _ := reg.Get("example__shout")
	if !shout.Capabilities().IsZero() {
		t.Errorf("a pure tool was granted %+v", shout.Capabilities())
	}

	res, err := shout.Run(context.Background(), json.RawMessage(`{"text":"lewat registry"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "LEWAT REGISTRY" {
		t.Errorf("got %q", res.Content)
	}
}

func TestToolsRequireAPolicy(t *testing.T) {
	c := startExample(t, t.TempDir())
	if _, err := c.Tools(nil); err == nil {
		t.Fatal("tools were exposed without any capability review")
	}
}

// ---------- failure handling ----------

func TestMissingPluginBinaryFailsClearly(t *testing.T) {
	_, err := Start(context.Background(), Options{Command: []string{"/nonexistent/plugin"}})
	if err == nil {
		t.Fatal("starting a nonexistent binary reported success")
	}
	if !strings.Contains(err.Error(), "start") {
		t.Errorf("error is %v", err)
	}
}

func TestPluginThatSaysNothingTimesOut(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Command:      []string{"sleep", "30"},
		StartTimeout: 300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a silent plugin completed its handshake")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("the error should say the handshake failed, got: %v", err)
	}
}

// TestCrashedPluginReportsItsStderr covers the difference between a debuggable
// failure and a mysterious one.
func TestCrashedPluginReportsItsStderr(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Command:      []string{"sh", "-c", "echo 'gagal memuat konfigurasi' >&2; exit 1"},
		StartTimeout: 3 * time.Second,
	})
	if err == nil {
		t.Fatal("a plugin that exited immediately reported success")
	}
	if !strings.Contains(err.Error(), "gagal memuat konfigurasi") {
		t.Errorf("the plugin's own error message was lost; got: %v", err)
	}
}

func TestNonJSONOutputIsRejected(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Command:      []string{"sh", "-c", "echo 'bukan json'; cat > /dev/null"},
		StartTimeout: 20 * time.Second,
	})
	if err == nil {
		t.Fatal("a plugin writing garbage to stdout was accepted")
	}
	if !strings.Contains(err.Error(), "JSON-RPC") {
		t.Errorf("error is %v", err)
	}
}

func TestManifestValidationRejectsBadDeclarations(t *testing.T) {
	valid := ToolSpec{Name: "t", Schema: json.RawMessage(`{"type":"object"}`)}

	cases := map[string]Manifest{
		"wrong protocol": {Protocol: 99, Name: "p", Tools: []ToolSpec{valid}},
		"no name":        {Protocol: ProtocolVersion, Tools: []ToolSpec{valid}},
		"no tools":       {Protocol: ProtocolVersion, Name: "p"},
		"unnamed tool":   {Protocol: ProtocolVersion, Name: "p", Tools: []ToolSpec{{Schema: json.RawMessage(`{}`)}}},
		"no schema":      {Protocol: ProtocolVersion, Name: "p", Tools: []ToolSpec{{Name: "t"}}},
		"bad schema":     {Protocol: ProtocolVersion, Name: "p", Tools: []ToolSpec{{Name: "t", Schema: json.RawMessage(`{oops`)}}},
		"duplicate tool": {Protocol: ProtocolVersion, Name: "p", Tools: []ToolSpec{valid, valid}},
	}
	for name, m := range cases {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	good := Manifest{Protocol: ProtocolVersion, Name: "p", Tools: []ToolSpec{valid}}
	if err := good.Validate(); err != nil {
		t.Errorf("a valid manifest was rejected: %v", err)
	}
}

// TestPluginEnvironmentIsEmptyByDefault guards a real leak: the host process
// holds API keys, and handing its environment to every plugin would share them
// with code the operator may not have written.
func TestPluginEnvironmentIsEmptyByDefault(t *testing.T) {
	t.Setenv("NEMUZ_SECRET_FOR_TEST", "jangan-bocor")
	dump := filepath.Join(t.TempDir(), "env.txt")

	const manifestLine = `{"jsonrpc":"2.0","id":1,"result":{"protocol":1,"name":"env","version":"0",` +
		`"tools":[{"name":"noop","description":"","schema":{"type":"object"},"capabilities":{}}]}}`

	c, err := Start(context.Background(), Options{
		// The probe blocks on stdin rather than sleeping. A sleeping probe
		// races the handshake timeout: under load it exits at the moment the
		// host gives up, and the test fails for reasons unrelated to what it
		// is checking.
		Command:      []string{"/bin/sh", "-c", "env > \"$DUMP\"; echo '" + manifestLine + "'; cat > /dev/null", "sh"},
		Env:          []string{"DUMP=" + dump, "NEMUZ_SECRET_FOR_TEST_SHOULD_NOT_APPEAR=1"},
		StartTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("probe plugin did not start: %v", err)
	}
	defer c.Close()

	body, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("probe wrote no environment dump: %v", err)
	}
	if strings.Contains(string(body), "jangan-bocor") {
		t.Fatalf("the host's secret reached the plugin:\n%s", body)
	}
	if !strings.Contains(string(body), "DUMP=") {
		t.Errorf("the deliberately passed variable is missing:\n%s", body)
	}
}

// TestDefaultEnvIsEmptyNotInherited covers the same rule for the default path,
// where Options.Env is left nil.
func TestDefaultEnvIsEmptyNotInherited(t *testing.T) {
	t.Setenv("NEMUZ_ANOTHER_SECRET", "rahasia")

	cmd := exec.Command("/bin/sh", "-c", "env")
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "rahasia") {
		t.Fatal("an empty Env still inherited the parent environment")
	}
}
