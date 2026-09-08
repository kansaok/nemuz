package memory

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultRecallLimit is how many memories a prompt carries by default. Enough
// to be useful, few enough that they do not crowd out the conversation.
const DefaultRecallLimit = 8

// idFormat keeps ids safe as file names and stops a memory id from escaping the
// store directory.
var idFormat = regexp.MustCompile(`^[A-Z0-9]{26}$`)

// ErrNotFound means no memory has that id.
var ErrNotFound = errors.New("memory: not found")

// Store is a directory of memories, one Markdown file each.
//
// The format matches skills for the same reason: a person can read the store,
// edit it, diff it in git, and delete a fact the agent got wrong, without the
// agent being involved.
type Store struct {
	root string
	now  func() time.Time
	// newID is swappable so tests get stable file names.
	newID func() (string, error)
}

// Open prepares a store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("memory: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create store: %w", err)
	}
	return &Store{root: dir, now: time.Now, newID: newULID}, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

// SetClock replaces the time source, so tests produce stable files.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// SetIDSource replaces the id generator, so tests produce stable file names.
func (s *Store) SetIDSource(next func() (string, error)) { s.newID = next }

func (s *Store) path(id string) (string, error) {
	if !idFormat.MatchString(id) {
		return "", fmt.Errorf("memory: %q is not a valid id", id)
	}
	return filepath.Join(s.root, id+".md"), nil
}

// Remember stores a new fact and returns it.
func (s *Store) Remember(text string, kind Kind, source string, tags []string) (*Memory, error) {
	id, err := s.newID()
	if err != nil {
		return nil, err
	}
	m := &Memory{
		ID:        id,
		Text:      strings.TrimSpace(text),
		Kind:      kind,
		Source:    source,
		Tags:      normaliseTags(tags),
		CreatedAt: s.now().UTC(),
	}
	if err := s.Save(m); err != nil {
		return nil, err
	}
	return m, nil
}

// Save writes a memory.
func (s *Store) Save(m *Memory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	path, err := s.path(m.ID)
	if err != nil {
		return err
	}
	m.UpdatedAt = s.now().UTC()

	meta, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("memory: encode %s: %w", m.ID, err)
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(meta)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(m.Text))
	b.WriteString("\n")

	return os.WriteFile(path, b.Bytes(), 0o600)
}

// Load reads one memory.
func (s *Store) Load(id string) (*Memory, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("memory: read %s: %w", id, err)
	}
	m, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("memory %s: %w", id, err)
	}
	m.ID = id
	return m, nil
}

// List returns every memory, oldest first — ULIDs sort chronologically.
func (s *Store) List() ([]*Memory, error) {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: list store: %w", err)
	}

	var out []*Memory
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		if !idFormat.MatchString(id) {
			continue
		}
		m, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Forget removes a memory.
//
// Unlike skills, memories are deletable. A skill is a procedure whose removal
// changes what the agent can do, so archiving keeps the decision reversible. A
// memory is a claim about the world, and a wrong claim should simply stop being
// there — leaving it archived would mean the agent still holds a belief its
// user has explicitly corrected.
func (s *Store) Forget(id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	} else if err != nil {
		return fmt.Errorf("memory: forget %s: %w", id, err)
	}
	return nil
}

// Pin keeps a memory in every prompt, whatever the query.
func (s *Store) Pin(id string, pinned bool) error {
	m, err := s.Load(id)
	if err != nil {
		return err
	}
	m.Pinned = pinned
	return s.Save(m)
}

// Recall returns the memories most relevant to query, pinned ones first.
//
// The result is a deterministic function of the store's contents and the query:
// the same inputs always produce the same memories in the same order. Recalled
// memories go into the system prompt, so anything less would make turns
// unreplayable for reasons unrelated to the agent.
func (s *Store) Recall(query string, limit int) ([]*Memory, error) {
	if limit <= 0 {
		limit = DefaultRecallLimit
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}

	type scored struct {
		m     *Memory
		score float64
	}
	var pinned []*Memory
	var candidates []scored

	queryTerms := terms(query)
	for _, m := range all {
		if m.Pinned {
			pinned = append(pinned, m)
			continue
		}
		if score := m.score(queryTerms); score > 0 {
			candidates = append(candidates, scored{m, score})
		}
	}

	// Ties break by id, which is chronological, so ordering never depends on
	// directory iteration order.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].m.ID > candidates[j].m.ID
	})

	out := pinned
	for _, c := range candidates {
		if len(out) >= limit {
			break
		}
		out = append(out, c.m)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordUse notes that memories were recalled into a prompt.
func (s *Store) RecordUse(ids ...string) error {
	for _, id := range ids {
		m, err := s.Load(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		m.UseCount++
		m.LastUsedAt = s.now().UTC()
		if err := s.Save(m); err != nil {
			return err
		}
	}
	return nil
}

// Prompt renders memories as a system-prompt section. It returns an empty
// string when there is nothing to say, so callers need no special case.
func Prompt(memories []*Memory) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# What you remember\n\n")
	for _, m := range memories {
		b.WriteString(m.Prompt())
		b.WriteString("\n")
	}
	return b.String()
}

func normaliseTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func decode(body []byte) (*Memory, error) {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, errors.New("file does not start with YAML frontmatter")
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, errors.New("frontmatter is not closed")
	}
	var m Memory
	if err := yaml.Unmarshal([]byte(rest[:end]), &m); err != nil {
		return nil, fmt.Errorf("unreadable frontmatter: %w", err)
	}
	m.Text = strings.TrimSpace(strings.TrimPrefix(rest[end+4:], "\n"))
	return &m, nil
}

// crockford is the alphabet ULIDs are rendered in.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a 26-character time-ordered id.
func newULID() (string, error) {
	var raw [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	for i := 0; i < 6; i++ {
		raw[i] = byte(ms >> (40 - 8*i))
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("memory: read randomness: %w", err)
	}

	out := make([]byte, 26)
	var bitPos uint
	for i := 0; i < 26; i++ {
		var v byte
		for b := 0; b < 5; b++ {
			pos := int(bitPos) + b - 2
			var bit byte
			if pos >= 0 {
				bit = (raw[pos/8] >> (7 - uint(pos%8))) & 1
			}
			v = v<<1 | bit
		}
		out[i] = crockford[v]
		bitPos += 5
	}
	return string(out), nil
}
