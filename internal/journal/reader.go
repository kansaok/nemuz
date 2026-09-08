package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxLine caps a single JSONL record. Anything larger should have spilled to
// the blob store, so exceeding this means the journal is corrupt.
const maxLine = 1 << 20 // 1 MiB

// Read parses a journal file into its events, verifying that sequence numbers
// are contiguous and start at 1. A gap means the file was truncated or edited,
// which invalidates any replay built on it.
func Read(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}
	defer f.Close()
	return ReadFrom(f, path)
}

// ReadFrom parses events from r. name is used only in error messages.
func ReadFrom(r io.Reader, name string) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	var events []Event
	for line := 1; sc.Scan(); line++ {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, fmt.Errorf("journal: %s line %d: %w", name, line, err)
		}
		if want := int64(len(events) + 1); ev.Seq != want {
			return nil, fmt.Errorf("journal: %s line %d: sequence gap, got %d want %d", name, line, ev.Seq, want)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("journal: read %s: %w", name, err)
	}
	return events, nil
}

// Turn is a journal file located on disk.
type Turn struct {
	ID   string
	Path string
}

// List returns the turns recorded under dir, ordered by id. Turn ids are
// lexicographically sortable (ULID), so this is also chronological order.
func List(dir string) ([]Turn, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal: list %s: %w", dir, err)
	}
	var turns []Turn
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		turns = append(turns, Turn{
			ID:   strings.TrimSuffix(name, ".jsonl"),
			Path: filepath.Join(dir, name),
		})
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].ID < turns[j].ID })
	return turns, nil
}

// Find locates a turn by id or by any unambiguous prefix of one, so operators
// can type the first few characters instead of a full ULID.
func Find(dir, idOrPrefix string) (Turn, error) {
	turns, err := List(dir)
	if err != nil {
		return Turn{}, err
	}
	var matches []Turn
	for _, t := range turns {
		if t.ID == idOrPrefix {
			return t, nil
		}
		if strings.HasPrefix(t.ID, idOrPrefix) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return Turn{}, fmt.Errorf("journal: no turn matching %q", idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return Turn{}, fmt.Errorf("journal: %q matches %d turns: %s", idOrPrefix, len(matches), strings.Join(ids, ", "))
	}
}
