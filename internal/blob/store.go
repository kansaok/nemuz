// Package blob implements a content-addressed store.
//
// Large payloads — model responses, tool output, media — never live in the
// journal or the database. They are written here once, keyed by the SHA-256 of
// their content, and referenced everywhere else by that hash. Identical content
// written twice costs nothing the second time.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Ref is the lowercase hex SHA-256 of a blob's content.
type Ref string

// ErrNotFound is returned when a ref has no corresponding blob.
var ErrNotFound = errors.New("blob: not found")

const refLen = 64

// Valid reports whether r is a well-formed reference.
func (r Ref) Valid() bool {
	if len(r) != refLen {
		return false
	}
	_, err := hex.DecodeString(string(r))
	return err == nil
}

func (r Ref) String() string { return string(r) }

// Short returns the first 12 characters, for display only.
func (r Ref) Short() string {
	if len(r) < 12 {
		return string(r)
	}
	return string(r[:12])
}

// Store is a content-addressed blob store rooted at a directory.
//
// Blobs are sharded by the first two characters of their hash so that no single
// directory accumulates an unbounded number of entries.
type Store struct{ root string }

// Open prepares a store at root, creating the directory if needed.
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("blob: empty root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("blob: create root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the store's base directory.
func (s *Store) Root() string { return s.root }

// Path returns the on-disk location of ref. The blob need not exist.
func (s *Store) Path(ref Ref) string {
	return filepath.Join(s.root, string(ref[:2]), string(ref))
}

// Has reports whether ref is already stored.
func (s *Store) Has(ref Ref) bool {
	if !ref.Valid() {
		return false
	}
	_, err := os.Stat(s.Path(ref))
	return err == nil
}

// PutBytes stores b and returns its ref. Storing identical content again is a
// no-op that returns the same ref.
func (s *Store) PutBytes(b []byte) (Ref, error) {
	sum := sha256.Sum256(b)
	ref := Ref(hex.EncodeToString(sum[:]))
	if s.Has(ref) {
		return ref, nil
	}
	dst := s.Path(ref)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("blob: create shard: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("blob: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", fmt.Errorf("blob: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("blob: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o400); err != nil {
		return "", fmt.Errorf("blob: chmod: %w", err)
	}
	// Rename is atomic within a directory, so a reader never observes a
	// partially written blob under its final name.
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("blob: commit: %w", err)
	}
	return ref, nil
}

// Put streams r into the store. The content is buffered in memory, so callers
// with very large inputs should chunk them.
func (s *Store) Put(r io.Reader) (Ref, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("blob: read source: %w", err)
	}
	return s.PutBytes(b)
}

// GetBytes returns the content of ref.
func (s *Store) GetBytes(ref Ref) ([]byte, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("blob: malformed ref %q", ref)
	}
	b, err := os.ReadFile(s.Path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref.Short())
	}
	if err != nil {
		return nil, fmt.Errorf("blob: read: %w", err)
	}
	return b, nil
}

// Get opens ref for reading. The caller must close the returned reader.
func (s *Store) Get(ref Ref) (io.ReadCloser, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("blob: malformed ref %q", ref)
	}
	f, err := os.Open(s.Path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref.Short())
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open: %w", err)
	}
	return f, nil
}
