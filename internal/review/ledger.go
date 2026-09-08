package review

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultLedgerSize is how many turn shapes are remembered. Large enough to
// cover a working session, small enough that a shape returning tomorrow gets
// reviewed again — which is usually right, because the answer may have changed.
const DefaultLedgerSize = 200

// Ledger remembers which turn shapes have been reviewed recently, so a run of
// near-identical turns is reviewed once rather than every time.
//
// This is where nemuz diverges from Hermes Agent, which reviews after every
// turn. Reviewing is a model call: doing it for the twentieth "run the tests"
// of the afternoon costs money and produces nothing, because the nineteenth
// already decided there was nothing to learn.
//
// The file is plain text, one fingerprint per line, newest last. It can be
// deleted at any time; the only consequence is that the next few turns get
// reviewed again.
type Ledger struct {
	path string
	max  int

	mu      sync.Mutex
	loaded  bool
	entries []string
	index   map[string]bool
}

// OpenLedger prepares a ledger at path.
func OpenLedger(path string) (*Ledger, error) {
	if path == "" {
		return nil, errors.New("review: empty ledger path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("review: create ledger directory: %w", err)
	}
	return &Ledger{path: path, max: DefaultLedgerSize, index: map[string]bool{}}, nil
}

// SetSize changes how many shapes are remembered.
func (l *Ledger) SetSize(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > 0 {
		l.max = n
	}
}

// Seen reports whether a shape has been reviewed recently.
func (l *Ledger) Seen(fingerprint string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return false, err
	}
	return l.index[fingerprint], nil
}

// Record notes that a shape has now been reviewed.
func (l *Ledger) Record(fingerprint string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return err
	}
	if l.index[fingerprint] {
		return nil
	}

	l.entries = append(l.entries, fingerprint)
	l.index[fingerprint] = true
	if len(l.entries) > l.max {
		dropped := l.entries[:len(l.entries)-l.max]
		l.entries = l.entries[len(l.entries)-l.max:]
		for _, d := range dropped {
			delete(l.index, d)
		}
	}
	return l.flush()
}

// Len returns how many shapes are currently remembered.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return 0
	}
	return len(l.entries)
}

// load reads the file once per process.
func (l *Ledger) load() error {
	if l.loaded {
		return nil
	}
	l.loaded = true

	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("review: read ledger: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || l.index[line] {
			continue
		}
		l.entries = append(l.entries, line)
		l.index[line] = true
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("review: read ledger: %w", err)
	}
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
		l.index = make(map[string]bool, len(l.entries))
		for _, e := range l.entries {
			l.index[e] = true
		}
	}
	return nil
}

// flush rewrites the file atomically.
func (l *Ledger) flush() error {
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return fmt.Errorf("review: write ledger: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	w := bufio.NewWriter(tmp)
	for _, e := range l.entries {
		if _, err := w.WriteString(e + "\n"); err != nil {
			tmp.Close()
			return fmt.Errorf("review: write ledger: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("review: write ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("review: write ledger: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("review: write ledger: %w", err)
	}
	return os.Rename(tmpName, l.path)
}
