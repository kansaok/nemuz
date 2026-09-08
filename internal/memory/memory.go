// Package memory stores what the agent has learned about its user and their
// work, and decides what to bring back into a prompt.
//
// Memories and skills answer different questions. A skill is a procedure — how
// to do something. A memory is a fact — that the deploy script lives in
// scripts/ship.sh, that this person prefers short answers. Skills are gated
// because acting on a bad procedure does damage; memories are not, because a
// wrong fact is corrected the same way a person corrects one, by saying so.
//
// Recall is deterministic: the same query against the same store always returns
// the same memories in the same order. That matters more here than it looks.
// Memories go into the system prompt, so a recall that varied between runs
// would make every turn unreplayable for a reason that has nothing to do with
// the agent.
package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kind says what sort of fact a memory holds. It exists so recall can prefer
// the right sort for a question, and so a person reviewing the store can see at
// a glance what the agent believes about them.
type Kind string

const (
	// KindUser is about the person: their role, preferences, how they work.
	KindUser Kind = "user"
	// KindProject is about the work: layout, conventions, decisions taken.
	KindProject Kind = "project"
	// KindReference points at something external — a URL, a ticket, a doc.
	KindReference Kind = "reference"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindUser, KindProject, KindReference:
		return true
	}
	return false
}

// Memory is one remembered fact.
type Memory struct {
	// ID is a ULID, so memories sort in the order they were learned.
	ID string `yaml:"id"`
	// Text is the fact itself, in the agent's own words.
	Text string `yaml:"-"`
	// Kind categorises it.
	Kind Kind `yaml:"kind"`
	// Source is the turn this was learned in, so a memory can be traced back
	// to the conversation that produced it.
	Source string `yaml:"source,omitempty"`
	// Tags are free-form labels the agent chose.
	Tags []string `yaml:"tags,omitempty"`

	CreatedAt time.Time `yaml:"created_at"`
	UpdatedAt time.Time `yaml:"updated_at,omitempty"`
	// UseCount counts how often this memory was recalled into a prompt.
	UseCount int `yaml:"use_count"`
	// LastUsedAt is when it was last recalled.
	LastUsedAt time.Time `yaml:"last_used_at,omitempty"`
	// Pinned keeps a memory in every prompt regardless of the query.
	Pinned bool `yaml:"pinned,omitempty"`
}

// MaxTextBytes caps one memory. A memory longer than this is a document, and
// belongs in the workspace where it can be read on demand.
const MaxTextBytes = 2000

// Validate checks a memory is storable.
func (m *Memory) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("memory: has no id")
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return fmt.Errorf("memory %s: is empty", m.ID)
	}
	if len(text) > MaxTextBytes {
		return fmt.Errorf("memory %s: is %d bytes, over the %d byte limit; store long content in the workspace instead",
			m.ID, len(text), MaxTextBytes)
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("memory %s: unknown kind %q", m.ID, m.Kind)
	}
	return nil
}

// Prompt renders a memory for a system prompt.
func (m *Memory) Prompt() string {
	if len(m.Tags) == 0 {
		return fmt.Sprintf("- (%s) %s", m.Kind, strings.TrimSpace(m.Text))
	}
	return fmt.Sprintf("- (%s) %s [%s]", m.Kind, strings.TrimSpace(m.Text), strings.Join(m.Tags, ", "))
}

// wordPattern splits text into lowercase terms for scoring. It keeps digits and
// underscores because file names and identifiers are exactly what gets recalled.
var wordPattern = regexp.MustCompile(`[a-z0-9_]+`)

// terms reduces text to its distinct lowercase terms.
func terms(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordPattern.FindAllString(strings.ToLower(text), -1) {
		if len(w) < 2 || isStopWord(w) {
			continue
		}
		out[w] = true
	}
	return out
}

// stopWords are terms too common to carry meaning in either language this is
// likely to see. Scoring without this makes every memory match every query.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "you": true, "are": true, "was": true,
	"this": true, "that": true, "with": true, "from": true, "have": true, "has": true,
	"what": true, "how": true, "why": true, "can": true, "not": true, "but": true,
	"yang": true, "dan": true, "untuk": true, "dari": true, "ini": true, "itu": true,
	"ada": true, "apa": true, "ke": true, "di": true, "saya": true, "kamu": true,
	"bisa": true, "tidak": true, "dengan": true, "pada": true, "atau": true,
}

func isStopWord(w string) bool { return stopWords[w] }

// score rates a memory against a query's terms.
//
// The formula is deliberately simple and explainable: overlap decides, ties
// break toward memories that have proved useful before, then toward the more
// recent. Anything fancier would be harder to reason about when a recall
// surprises someone, and this runs on a few hundred rows.
func (m *Memory) score(queryTerms map[string]bool) float64 {
	if len(queryTerms) == 0 {
		return 0
	}
	memTerms := terms(m.Text + " " + strings.Join(m.Tags, " "))
	if len(memTerms) == 0 {
		return 0
	}

	var hits int
	for term := range queryTerms {
		if memTerms[term] {
			hits++
		}
	}
	if hits == 0 {
		return 0
	}

	// Overlap relative to the query, so a long memory does not outrank a
	// precise one just by containing more words.
	overlap := float64(hits) / float64(len(queryTerms))
	// A small, bounded nudge for memories that have earned their place.
	useful := float64(m.UseCount)
	if useful > 5 {
		useful = 5
	}
	return overlap*10 + useful*0.1
}
