package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWS(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("isi a"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A secret outside the workspace, for the escape tests to aim at.
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("rahasia"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(work)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func run(t *testing.T, tl Tool, args any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s returned a fatal error: %v", tl.Name(), err)
	}
	return res
}

func TestReadFileReadsInsideWorkspace(t *testing.T) {
	ws := newWS(t)
	res := run(t, NewReadFile(ws), map[string]string{"path": "a.txt"})
	if res.IsError {
		t.Fatalf("reading a workspace file failed: %s", res.Content)
	}
	if res.Content != "isi a" {
		t.Errorf("got %q", res.Content)
	}
}

// TestPathEscapesAreRefused covers the ways an agent might try to leave the
// workspace. The sandbox blocks these at the kernel level too; this check exists
// so the model gets an explanation it can act on.
func TestPathEscapesAreRefused(t *testing.T) {
	ws := newWS(t)
	rd := NewReadFile(ws)

	for _, path := range []string{
		"../secret.txt",
		"sub/../../secret.txt",
		"/etc/passwd",
		"",
	} {
		res := run(t, rd, map[string]string{"path": path})
		if !res.IsError {
			t.Errorf("path %q was accepted; it should have been refused", path)
		}
	}
}

func TestSymlinkEscapeIsRefused(t *testing.T) {
	ws := newWS(t)
	outside := filepath.Join(filepath.Dir(ws.Root()), "secret.txt")
	link := filepath.Join(ws.Root(), "shortcut.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := run(t, NewReadFile(ws), map[string]string{"path": "shortcut.txt"})
	if !res.IsError {
		t.Fatalf("a symlink out of the workspace was followed, exposing %q", res.Content)
	}
	if !strings.Contains(res.Content, "outside the workspace") {
		t.Errorf("the refusal should say the path leaves the workspace, got: %s", res.Content)
	}
}

func TestWriteFileCreatesParentDirectories(t *testing.T) {
	ws := newWS(t)
	res := run(t, NewWriteFile(ws), map[string]string{"path": "baru/nested/file.txt", "content": "halo"})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	body, err := os.ReadFile(filepath.Join(ws.Root(), "baru/nested/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "halo" {
		t.Errorf("file contains %q", body)
	}
}

func TestWriteFileRefusesToEscape(t *testing.T) {
	ws := newWS(t)
	res := run(t, NewWriteFile(ws), map[string]string{"path": "../pwned.txt", "content": "x"})
	if !res.IsError {
		t.Fatal("a write outside the workspace was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws.Root()), "pwned.txt")); err == nil {
		t.Fatal("the file was actually created outside the workspace")
	}
}

func TestListDirSortsAndMarksDirectories(t *testing.T) {
	ws := newWS(t)
	res := run(t, NewListDir(ws), map[string]string{"path": "."})
	if res.IsError {
		t.Fatalf("listing failed: %s", res.Content)
	}
	if res.Content != "a.txt\nsub/" {
		t.Errorf("listing is %q; entries should be sorted with directories marked", res.Content)
	}
}

func TestReadFileRejectsDirectory(t *testing.T) {
	ws := newWS(t)
	res := run(t, NewReadFile(ws), map[string]string{"path": "sub"})
	if !res.IsError {
		t.Fatal("reading a directory succeeded")
	}
	if !strings.Contains(res.Content, "list_dir") {
		t.Errorf("the error should point at list_dir, got: %s", res.Content)
	}
}
