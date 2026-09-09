package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultExecTimeout bounds one command. Builds and test suites are slow, so
// this is generous; a command still running after it is treated as stuck.
const DefaultExecTimeout = 5 * time.Minute

// maxExecOutput caps what a command may return. Output past it is truncated
// with a note, because a model given a hundred thousand lines of build log
// learns nothing it could not learn from the last hundred.
const maxExecOutput = 64 << 10 // 64 KiB

// RunCommand runs an allowed program in the workspace.
//
// # No shell
//
// The command and its arguments are passed to the operating system directly.
// There is no `sh -c`, so there is no quoting to get wrong, no globbing, no
// pipes, no `;` or `&&`, and no way for an argument to become a second command.
// A model that wants two things done asks twice. This costs some convenience
// and removes an entire class of hole.
//
// # The allowlist is names, not paths
//
// Programs are named — "make", "go", "git" — and looked up on PATH. Naming a
// path would invite an agent to reach for one the operator never approved, and
// the capability the sandbox enforces is about which programs may run, not
// where they happen to live.
type RunCommand struct {
	ws      *Workspace
	allowed []string
	// scratch is a private directory the command may write to, used for its
	// temporary files and as its HOME.
	scratch string
	// paths maps a program name to where it was found. The host resolves
	// these, because it has the operator's environment; the confined worker
	// has none and could not find a toolchain installed under a home
	// directory, which is where Go, node and cargo usually live.
	paths   map[string]string
	timeout time.Duration
}

// NewRunCommand returns the exec tool for ws, limited to the named programs.
//
// With an empty allowlist the tool refuses everything. That is deliberate: a
// caller that forgot to configure it gets a tool that does nothing, rather than
// a tool that does anything.
func NewRunCommand(ws *Workspace, programs map[string]string) *RunCommand {
	allowed := make([]string, 0, len(programs))
	paths := make(map[string]string, len(programs))
	for name, path := range programs {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		allowed = append(allowed, name)
		paths[name] = path
	}
	sort.Strings(allowed)
	return &RunCommand{ws: ws, allowed: allowed, paths: paths, timeout: DefaultExecTimeout}
}

// Resolve finds the named programs using this process's PATH.
//
// It runs on the host rather than in the worker, because the worker is started
// with an empty environment on purpose and a toolchain under a home directory
// would be invisible to it. Names that are not installed come back in missing,
// so an operator hears about it at startup instead of mid-turn.
func Resolve(names []string) (found map[string]string, missing []string) {
	found = map[string]string{}
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		path, err := exec.LookPath(name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		found[name] = path
	}
	sort.Strings(missing)
	return found, missing
}

// ProgramDirs returns the directories the sandbox must allow for the resolved
// programs to run.
//
// For each program this is the directory it sits in — and, when that directory
// is called "bin", its parent as well. A toolchain is not one file: `go vet`
// runs go/pkg/tool/linux_amd64/vet, npm reaches into its lib, cargo into its
// own tree. Granting only the bin directory finds the entry point and then
// fails on the first thing it calls, which is a confusing way to be stopped.
//
// The parent of a bin directory is the toolchain's root by near-universal
// convention, so this covers Go, node and cargo without asking an operator to
// know where each keeps its parts.
func ProgramDirs(programs map[string]string) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		if dir == "" || dir == "/" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}

	for _, path := range programs {
		dir := filepath.Dir(path)
		add(dir)
		if filepath.Base(dir) == "bin" {
			add(filepath.Dir(dir))
		}
	}
	sort.Strings(dirs)
	return dirs
}

// SetScratch gives commands a private directory for temporary files.
//
// Toolchains need somewhere to write that is not the project: `go vet` wants a
// work directory, npm wants a cache, cc wants somewhere for object files. The
// sandbox does not allow /tmp, and it should not — that is shared with every
// other process on the machine.
//
// It is also the command's HOME. A command therefore cannot read the operator's
// caches or credentials through it, and cannot leave anything behind in the
// project it was asked to work on.
func (t *RunCommand) SetScratch(dir string) { t.scratch = dir }

// SetTimeout overrides how long a command may run.
func (t *RunCommand) SetTimeout(d time.Duration) {
	if d > 0 {
		t.timeout = d
	}
}

// Allowed returns the programs this tool may run.
func (t *RunCommand) Allowed() []string { return append([]string(nil), t.allowed...) }

func (t *RunCommand) Name() string { return "run_command" }

func (t *RunCommand) Description() string {
	if len(t.allowed) == 0 {
		return "Run a command in the workspace. No programs are currently allowed, so every call will be refused."
	}
	return fmt.Sprintf(
		"Run one of these programs in the workspace: %s. Arguments are passed directly — there is no shell, so pipes, globs and operators like && do not work. Run one command per call.",
		strings.Join(t.allowed, ", "))
}

func (t *RunCommand) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"command":{"type":"string","description":"Program name, not a path"},` +
		`"args":{"type":"array","items":{"type":"string"},"description":"Arguments, one per element; not a single string"},` +
		`"dir":{"type":"string","description":"Directory to run in, relative to the workspace root. Defaults to the root."}` +
		`},"required":["command"],"additionalProperties":false}`)
}

// Capabilities reports what running these programs needs.
//
// The write grant stays the workspace and nothing wider. That is the guarantee
// worth keeping once exec exists: a command can read the system and run
// programs, and it still cannot write outside the directory it was pointed at.
func (t *RunCommand) Capabilities() Capabilities {
	if len(t.allowed) == 0 {
		return Capabilities{}
	}
	caps := Capabilities{
		FSRead:  []string{t.ws.Root()},
		FSWrite: []string{t.ws.Root()},
		Exec:    t.allowed,
	}
	if t.scratch != "" {
		caps.FSRead = append(caps.FSRead, t.scratch)
		caps.FSWrite = append(caps.FSWrite, t.scratch)
	}
	return Union(caps)
}

func (t *RunCommand) permits(name string) bool {
	for _, a := range t.allowed {
		if a == name {
			return true
		}
	}
	return false
}

func (t *RunCommand) Run(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Dir     string   `json:"dir"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return Errorf("could not parse arguments: %v", err), nil
	}

	name := strings.TrimSpace(args.Command)
	switch {
	case name == "":
		return Errorf("no command given"), nil
	case strings.ContainsAny(name, `/\`):
		return Errorf("name the program, not a path: %q. Allowed: %s",
			name, listOrNothing(t.allowed)), nil
	case !t.permits(name):
		return Errorf("%q is not allowed. Allowed: %s. An operator can permit more with --allow-exec.",
			name, listOrNothing(t.allowed)), nil
	}

	dir := t.ws.Root()
	if args.Dir != "" && args.Dir != "." {
		resolved, err := t.ws.resolve(args.Dir)
		if err != nil {
			return Errorf("%v", err), nil
		}
		dir = resolved
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	program := t.paths[name]
	if program == "" {
		return Errorf("%q is allowed but was not found when nemuz started", name), nil
	}

	cmd := exec.CommandContext(ctx, program, args.Args...)
	cmd.Dir = dir
	// An empty environment, for the same reason plugins get one: the host
	// process holds API keys, and a command the model chose should not inherit
	// them. PATH is supplied because a program has to be findable.
	// PATH covers the system directories plus wherever the approved programs
	// were actually found, because a command spawns others: make runs go, and
	// go has to be findable by make, not only by nemuz.
	home := t.scratch
	if home == "" {
		home = dir
	}
	cmd.Env = []string{
		"PATH=" + t.searchPath(),
		"HOME=" + home,
		"TMPDIR=" + home,
		// Some toolchains read these instead.
		"TMP=" + home,
		"TEMP=" + home,
	}

	// An explicit empty stdin, so Go does not open /dev/null for it. A tool
	// call is not an interactive session; a command that waits for input
	// should see end-of-file immediately rather than hang until the timeout.
	cmd.Stdin = strings.NewReader("")

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	started := time.Now()
	runErr := cmd.Run()
	took := time.Since(started).Round(time.Millisecond)

	body := truncate(out.String())
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Errorf("%s timed out after %s\n%s", name, t.timeout, body), nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		// A non-zero exit is a result the model should see and work with, not
		// a failure of the tool.
		return Result{
			Content: fmt.Sprintf("%s exited %d after %s\n%s", name, exitErr.ExitCode(), took, body),
			IsError: true,
		}, nil
	}
	if runErr != nil {
		return Errorf("could not run %s: %v", name, runErr), nil
	}

	if strings.TrimSpace(body) == "" {
		return Result{Content: fmt.Sprintf("%s finished in %s with no output", name, took)}, nil
	}
	return Result{Content: fmt.Sprintf("%s finished in %s\n%s", name, took, body)}, nil
}

// systemPath is where ordinary programs live.
const systemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// searchPath is the PATH given to a command: the system directories plus
// wherever the approved programs were found.
func (t *RunCommand) searchPath() string {
	path := systemPath
	for _, dir := range ProgramDirs(t.paths) {
		if !strings.Contains(systemPath, dir) {
			path += string(filepath.ListSeparator) + dir
		}
	}
	return path
}

func truncate(s string) string {
	if len(s) <= maxExecOutput {
		return s
	}
	// The end is kept, because that is where a failure explains itself.
	cut := s[len(s)-maxExecOutput:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return fmt.Sprintf("… %d earlier bytes omitted …\n%s", len(s)-len(cut), cut)
}

func listOrNothing(items []string) string {
	if len(items) == 0 {
		return "nothing"
	}
	return strings.Join(items, ", ")
}
