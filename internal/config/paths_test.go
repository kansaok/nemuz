package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHonoursEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)

	p, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != dir {
		t.Fatalf("root is %q, want the NEMUZ_HOME override %q", p.Root, dir)
	}
	if p.Journal != filepath.Join(dir, "journal") {
		t.Errorf("journal path %q is not under the override root", p.Journal)
	}
}

func TestEnsureDirsCreatesPrivateDirectories(t *testing.T) {
	p := At(filepath.Join(t.TempDir(), "state"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{p.Root, p.Journal, p.Blobs, p.Skills, p.Plugins} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatalf("%s was not created: %v", d, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 0700 — it holds credentials and transcripts", d, perm)
		}
	}
}
