package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Workspace is the directory tree the filesystem tools operate on.
//
// Every path an agent supplies is resolved against the workspace and rejected
// if it escapes — symlinks included. The sandbox enforces the same boundary at
// the kernel level; this check exists so the agent gets a clear explanation
// instead of a bare EACCES, and so the boundary still holds on platforms where
// Landlock is unavailable.
type Workspace struct{ root string }

// NewWorkspace returns a workspace rooted at dir, which must already exist.
func NewWorkspace(dir string) (*Workspace, error) {
	root, err := cleanDir(dir)
	if err != nil {
		return nil, err
	}
	// Resolving symlinks now means the stored root is the same one the kernel
	// will see, so a symlinked workspace does not silently defeat the check.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("tool: workspace %s: %w", root, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("tool: workspace %s: %w", resolved, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("tool: workspace %s is not a directory", resolved)
	}
	return &Workspace{root: resolved}, nil
}

// Root returns the workspace directory.
func (w *Workspace) Root() string { return w.root }

// ErrOutsideWorkspace means a path resolved outside the workspace.
var ErrOutsideWorkspace = errors.New("path is outside the workspace")

// resolve turns an agent-supplied path into an absolute one inside the
// workspace, or fails.
func (w *Workspace) resolve(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("tool: empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("tool: %w: %s (paths must be relative to the workspace)", ErrOutsideWorkspace, rel)
	}
	abs := filepath.Clean(filepath.Join(w.root, rel))
	if abs != w.root && !strings.HasPrefix(abs, w.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("tool: %w: %s", ErrOutsideWorkspace, rel)
	}
	// A path that exists must also resolve inside the workspace after symlinks
	// are followed; one that does not exist yet is checked via its parent.
	probe, remainder := abs, ""
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if resolved != w.root && !strings.HasPrefix(resolved, w.root+string(os.PathSeparator)) {
				return "", fmt.Errorf("tool: %w: %s resolves to %s", ErrOutsideWorkspace, rel, resolved)
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("tool: resolve %s: %w", rel, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return abs, nil
		}
		remainder = filepath.Join(filepath.Base(probe), remainder)
		probe = parent
	}
}

// maxReadBytes caps a single read so one large file cannot exhaust the model's
// context or the process's memory.
const maxReadBytes = 256 << 10 // 256 KiB

// ReadFile reads a file from the workspace.
type ReadFile struct{ ws *Workspace }

// NewReadFile returns the read tool for ws.
func NewReadFile(ws *Workspace) *ReadFile { return &ReadFile{ws: ws} }

func (t *ReadFile) Name() string { return "read_file" }
func (t *ReadFile) Description() string {
	return "Read a UTF-8 text file from the workspace. Paths are relative to the workspace root."
}
func (t *ReadFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the workspace root"}},"required":["path"],"additionalProperties":false}`)
}
func (t *ReadFile) Capabilities() Capabilities {
	return Capabilities{FSRead: []string{t.ws.Root()}}
}

func (t *ReadFile) Run(_ context.Context, raw json.RawMessage) (Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return Errorf("could not parse arguments: %v", err), nil
	}
	abs, err := t.ws.resolve(args.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Errorf("cannot read %s: %v", args.Path, err), nil
	}
	if fi.IsDir() {
		return Errorf("%s is a directory; use list_dir", args.Path), nil
	}
	if fi.Size() > maxReadBytes {
		return Errorf("%s is %d bytes, over the %d byte read limit", args.Path, fi.Size(), maxReadBytes), nil
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return Errorf("cannot read %s: %v", args.Path, err), nil
	}
	return Result{Content: string(body)}, nil
}

// WriteFile writes a file into the workspace.
type WriteFile struct{ ws *Workspace }

// NewWriteFile returns the write tool for ws.
func NewWriteFile(ws *Workspace) *WriteFile { return &WriteFile{ws: ws} }

func (t *WriteFile) Name() string { return "write_file" }
func (t *WriteFile) Description() string {
	return "Create or overwrite a text file in the workspace. Parent directories are created as needed."
}
func (t *WriteFile) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the workspace root"},"content":{"type":"string","description":"Full file contents"}},"required":["path","content"],"additionalProperties":false}`)
}
func (t *WriteFile) Capabilities() Capabilities {
	return Capabilities{FSRead: []string{t.ws.Root()}, FSWrite: []string{t.ws.Root()}}
}

func (t *WriteFile) Run(_ context.Context, raw json.RawMessage) (Result, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return Errorf("could not parse arguments: %v", err), nil
	}
	abs, err := t.ws.resolve(args.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Errorf("cannot create parent of %s: %v", args.Path, err), nil
	}
	if err := os.WriteFile(abs, []byte(args.Content), 0o600); err != nil {
		return Errorf("cannot write %s: %v", args.Path, err), nil
	}
	return Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path)}, nil
}

// maxListEntries caps directory listings for the same reason reads are capped.
const maxListEntries = 500

// ListDir lists a directory in the workspace.
type ListDir struct{ ws *Workspace }

// NewListDir returns the list tool for ws.
func NewListDir(ws *Workspace) *ListDir { return &ListDir{ws: ws} }

func (t *ListDir) Name() string { return "list_dir" }
func (t *ListDir) Description() string {
	return "List the entries of a workspace directory. Use \".\" for the workspace root."
}
func (t *ListDir) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory relative to the workspace root; \".\" for the root"}},"required":["path"],"additionalProperties":false}`)
}
func (t *ListDir) Capabilities() Capabilities {
	return Capabilities{FSRead: []string{t.ws.Root()}}
}

func (t *ListDir) Run(_ context.Context, raw json.RawMessage) (Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return Errorf("could not parse arguments: %v", err), nil
	}
	abs, err := t.ws.resolve(args.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return Errorf("cannot list %s: %v", args.Path, err), nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name()+"/")
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	truncated := ""
	if len(names) > maxListEntries {
		truncated = fmt.Sprintf("\n… %d more entries not shown", len(names)-maxListEntries)
		names = names[:maxListEntries]
	}
	if len(names) == 0 {
		return Result{Content: "(empty directory)"}, nil
	}
	return Result{Content: strings.Join(names, "\n") + truncated}, nil
}
