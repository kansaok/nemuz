// Package config resolves where nemuz keeps its state.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvHome overrides the state directory. Tests and multi-profile setups use it.
const EnvHome = "NEMUZ_HOME"

// Paths names every directory nemuz writes to.
//
// The split is deliberate: the journal and blobs are append-only files that
// back up with plain rsync and survive a database reset, while the database
// holds only indexes and operational state that can be rebuilt from them.
type Paths struct {
	Root     string // ~/.nemuz
	Journal  string // append-only turn records
	Blobs    string // content-addressed payloads
	Skills   string // learned skills and their eval scenarios
	Memories string // facts kept from past turns
	Plugins  string // installed plugins
	DB       string // state.db
}

// Resolve returns the paths for this machine, honouring NEMUZ_HOME.
func Resolve() (Paths, error) {
	root := os.Getenv(EnvHome)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("config: locate home directory: %w", err)
		}
		root = filepath.Join(home, ".nemuz")
	}
	return At(root), nil
}

// At returns the paths rooted at dir.
func At(dir string) Paths {
	return Paths{
		Root:     dir,
		Journal:  filepath.Join(dir, "journal"),
		Blobs:    filepath.Join(dir, "blobs"),
		Skills:   filepath.Join(dir, "skills"),
		Memories: filepath.Join(dir, "memories"),
		Plugins:  filepath.Join(dir, "plugins"),
		DB:       filepath.Join(dir, "state.db"),
	}
}

// EnsureDirs creates the directories nemuz needs, with owner-only permissions
// because credentials and transcripts live under them.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.Root, p.Journal, p.Blobs, p.Skills, p.Memories, p.Plugins} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("config: create %s: %w", d, err)
		}
	}
	return nil
}
