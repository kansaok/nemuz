package skill

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// fileName is the Markdown file inside each skill's directory.
const fileName = "SKILL.md"

// evalDir holds a skill's eval scenarios, beside the skill itself so the two
// travel together when a skill is copied or shared.
const evalDir = "evals"

// frontmatterFence delimits the YAML header of a skill file.
const frontmatterFence = "---"

// ErrNotFound means no skill by that name exists.
var ErrNotFound = errors.New("skill: not found")

// Store is a directory of skills.
//
// Skills are Markdown on disk on purpose: a person can read one, edit one in
// any editor, diff one in git, and copy one between machines without the agent
// being involved. The bookkeeping lives in YAML frontmatter for the same reason.
type Store struct {
	root string
	now  func() time.Time
}

// Open prepares a store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("skill: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("skill: create store: %w", err)
	}
	return &Store{root: dir, now: time.Now}, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

// SetClock replaces the store's time source, so tests produce stable files.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// dir returns a skill's directory, refusing names that could escape the store.
func (s *Store) dir(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("skill: %q is not a valid name", name)
	}
	return filepath.Join(s.root, name), nil
}

// EvalDir returns where a skill's scenarios live.
func (s *Store) EvalDir(name string) (string, error) {
	dir, err := s.dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, evalDir), nil
}

// Save writes a skill, creating or replacing its file.
func (s *Store) Save(sk *Skill) error {
	if err := sk.Validate(); err != nil {
		return err
	}
	dir, err := s.dir(sk.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("skill: create %s: %w", sk.Name, err)
	}

	sk.UpdatedAt = s.now().UTC()
	body, err := encode(sk)
	if err != nil {
		return err
	}

	// Written via a temporary file so a crash mid-write cannot leave a skill
	// half-replaced — the agent edits these while it is running.
	path := filepath.Join(dir, fileName)
	tmp, err := os.CreateTemp(dir, ".skill-*")
	if err != nil {
		return fmt.Errorf("skill: write %s: %w", sk.Name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("skill: write %s: %w", sk.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("skill: write %s: %w", sk.Name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("skill: write %s: %w", sk.Name, err)
	}
	return os.Rename(tmpName, path)
}

// Load reads one skill.
func (s *Store) Load(name string) (*Skill, error) {
	dir, err := s.dir(name)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("skill: read %s: %w", name, err)
	}
	sk, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", name, err)
	}
	// The directory name is authoritative: a mismatched frontmatter name would
	// otherwise let one skill masquerade as another.
	sk.Name = name
	return sk, nil
}

// List returns every skill, sorted by name.
func (s *Store) List() ([]*Skill, error) {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill: list store: %w", err)
	}

	var out []*Skill
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		sk, err := s.Load(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// InState returns the skills currently in one state.
func (s *Store) InState(state State) ([]*Skill, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []*Skill
	for _, sk := range all {
		if sk.State == state {
			out = append(out, sk)
		}
	}
	return out, nil
}

// Active returns the skills a model may be given.
//
// This is the only accessor the agent should use when building a prompt.
// Quarantined skills exist, are stored, and are visible to an operator — but
// they never reach a model until the gate lets them through.
func (s *Store) Active() ([]*Skill, error) { return s.InState(StateActive) }

// Promote moves a skill out of quarantine.
//
// It requires a passing report: promotion is the one transition that widens
// what a model can do, so it is the one that demands evidence. There is no
// variant of this call that skips the check.
func (s *Store) Promote(name string, report Report) error {
	sk, err := s.Load(name)
	if err != nil {
		return err
	}
	if sk.State == StateActive {
		return nil
	}
	if sk.State != StateQuarantine {
		return fmt.Errorf("skill %s: only quarantined skills can be promoted, this one is %s", name, sk.State)
	}
	if !report.Passed() {
		return fmt.Errorf("skill %s: cannot be promoted, %s", name, report.Summary())
	}
	if report.Skill != name {
		return fmt.Errorf("skill %s: was given an eval report for %q", name, report.Skill)
	}

	sk.State = StateActive
	sk.PromotedBy = report.ID
	sk.ArchivedReason = ""
	return s.Save(sk)
}

// Archive retires a skill. This is the strongest removal the store offers.
//
// There is deliberately no Delete: an agent that can erase its own history can
// erase the evidence of a mistake, and a person who wants a skill truly gone
// can remove the directory themselves.
func (s *Store) Archive(name, reason string) error {
	sk, err := s.Load(name)
	if err != nil {
		return err
	}
	if sk.Pinned {
		return fmt.Errorf("skill %s: is pinned; unpin it before archiving", name)
	}
	if sk.State == StateArchived {
		return nil
	}
	sk.State = StateArchived
	sk.ArchivedReason = strings.TrimSpace(reason)
	return s.Save(sk)
}

// Restore brings an archived skill back — into quarantine, not into service.
//
// A skill that was retired must prove itself again, because whatever made it
// wrong may still be true.
func (s *Store) Restore(name string) error {
	sk, err := s.Load(name)
	if err != nil {
		return err
	}
	if sk.State != StateArchived {
		return fmt.Errorf("skill %s: is %s, not archived", name, sk.State)
	}
	sk.State = StateQuarantine
	sk.PromotedBy = ""
	sk.ArchivedReason = ""
	return s.Save(sk)
}

// Pin exempts a skill from automatic curation.
func (s *Store) Pin(name string, pinned bool) error {
	sk, err := s.Load(name)
	if err != nil {
		return err
	}
	sk.Pinned = pinned
	return s.Save(sk)
}

// RecordUse notes that a skill was used, which is what curation reasons about.
func (s *Store) RecordUse(name string) error {
	sk, err := s.Load(name)
	if err != nil {
		return err
	}
	sk.UseCount++
	sk.LastUsedAt = s.now().UTC()
	return s.Save(sk)
}

// encode renders a skill as frontmatter plus Markdown.
func encode(sk *Skill) ([]byte, error) {
	meta, err := yaml.Marshal(sk)
	if err != nil {
		return nil, fmt.Errorf("skill: encode frontmatter: %w", err)
	}
	var b bytes.Buffer
	b.WriteString(frontmatterFence + "\n")
	b.Write(meta)
	b.WriteString(frontmatterFence + "\n\n")
	b.WriteString(strings.TrimSpace(sk.Body))
	b.WriteString("\n")
	return b.Bytes(), nil
}

// decode parses frontmatter plus Markdown back into a skill.
func decode(body []byte) (*Skill, error) {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.HasPrefix(text, frontmatterFence+"\n") {
		return nil, errors.New("file does not start with YAML frontmatter")
	}
	rest := text[len(frontmatterFence)+1:]
	end := strings.Index(rest, "\n"+frontmatterFence)
	if end < 0 {
		return nil, errors.New("frontmatter is not closed")
	}

	var sk Skill
	if err := yaml.Unmarshal([]byte(rest[:end]), &sk); err != nil {
		return nil, fmt.Errorf("unreadable frontmatter: %w", err)
	}
	remainder := rest[end+len(frontmatterFence)+1:]
	sk.Body = strings.TrimSpace(strings.TrimPrefix(remainder, "\n"))
	return &sk, nil
}
