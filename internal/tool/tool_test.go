package tool

import (
	"reflect"
	"testing"
)

func TestUnionDeduplicatesAndSorts(t *testing.T) {
	got := Union(
		Capabilities{FSRead: []string{"/b", "/a"}, Exec: []string{"git"}},
		Capabilities{FSRead: []string{"/a"}, FSWrite: []string{"/out"}},
		Capabilities{Exec: []string{"go", "git"}},
	)

	want := Capabilities{
		FSRead:  []string{"/a", "/b"},
		FSWrite: []string{"/out"},
		Exec:    []string{"git", "go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("union is %+v, want %+v", got, want)
	}
}

// TestUnionIsStable matters because an unstable union would change the journal
// digest between runs that are otherwise identical.
func TestUnionIsStable(t *testing.T) {
	a := Capabilities{FSRead: []string{"/z", "/a", "/m"}}
	b := Capabilities{FSRead: []string{"/m", "/z"}}

	first := Union(a, b)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(Union(a, b), first) {
			t.Fatal("union produced a different result on a later call")
		}
	}
}

func TestZeroCapabilitiesAreRecognised(t *testing.T) {
	if !(Capabilities{}).IsZero() {
		t.Error("empty capabilities did not report as zero")
	}
	if (Capabilities{Net: []string{"example.com"}}).IsZero() {
		t.Error("capabilities with a net grant reported as zero")
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	ws := newWS(t)
	r := NewRegistry()
	if err := r.Register(NewReadFile(ws)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(NewReadFile(ws)); err == nil {
		t.Fatal("registering two tools under one name was accepted")
	}
}

func TestRegistryCapabilitiesUnionEveryTool(t *testing.T) {
	ws := newWS(t)
	r := NewRegistry()
	if err := r.Register(NewReadFile(ws), NewWriteFile(ws), NewListDir(ws)); err != nil {
		t.Fatal(err)
	}

	caps := r.Capabilities()
	if len(caps.FSRead) != 1 || caps.FSRead[0] != ws.Root() {
		t.Errorf("read grants are %v, want just the workspace", caps.FSRead)
	}
	if len(caps.FSWrite) != 1 || caps.FSWrite[0] != ws.Root() {
		t.Errorf("write grants are %v, want just the workspace", caps.FSWrite)
	}
	if len(caps.Net) != 0 || len(caps.Exec) != 0 {
		t.Errorf("filesystem tools asked for network or exec: %+v", caps)
	}
}

func TestRegistryDefsCarrySchemas(t *testing.T) {
	ws := newWS(t)
	r := NewRegistry()
	if err := r.Register(NewReadFile(ws), NewListDir(ws)); err != nil {
		t.Fatal(err)
	}

	defs := r.Defs()
	if len(defs) != 2 {
		t.Fatalf("got %d definitions, want 2", len(defs))
	}
	for _, d := range defs {
		if d.Name == "" || d.Description == "" || len(d.Schema) == 0 {
			t.Errorf("definition %q is incomplete: %+v", d.Name, d)
		}
	}
	if defs[0].Name != "read_file" {
		t.Errorf("definitions are not in registration order: got %q first", defs[0].Name)
	}
}
