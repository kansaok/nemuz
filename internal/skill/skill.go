// Package skill stores what the agent has learned, and governs when that
// learning is allowed to take effect.
//
// # The gate
//
// An agent that writes its own skills is only as trustworthy as its worst
// unreviewed idea. Hermes Agent showed the mechanism works — background review,
// an idle curator, a rollback ledger — but a skill it writes becomes active
// immediately, on the strength of the model's own say-so.
//
// nemuz keeps the mechanism and closes that gap. A new skill lands in
// quarantine and stays there until it passes recorded scenarios. Because turns
// are journaled, those scenarios replay for free: proving a skill costs no API
// calls, so there is no reason to skip it.
//
// # Invariants
//
// These hold no matter who or what is acting:
//
//   - Nothing is ever deleted. Archiving is the strongest removal, and it is
//     reversible.
//   - A skill reaches Active only with evidence that it passed its evals.
//   - A pinned skill is never transitioned automatically.
package skill

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// State is where a skill sits in its lifecycle.
type State string

const (
	// StateQuarantine is where every agent-written skill starts. Quarantined
	// skills are stored and visible but never given to a model.
	StateQuarantine State = "quarantine"
	// StateActive means the skill passed its evals and is offered to models.
	StateActive State = "active"
	// StateArchived means the skill is retired but recoverable.
	StateArchived State = "archived"
)

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StateQuarantine, StateActive, StateArchived:
		return true
	}
	return false
}

// Author says who wrote a skill.
type Author string

const (
	// ByAgent marks a skill the agent wrote for itself. Only these are
	// subject to automatic curation.
	ByAgent Author = "agent"
	// ByUser marks a skill a person wrote. The curator never touches these.
	ByUser Author = "user"
)

// Skill is one learned capability: instructions for the model, plus the
// bookkeeping that decides whether it is trusted yet.
type Skill struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	State       State     `yaml:"state"`
	CreatedBy   Author    `yaml:"created_by"`
	CreatedAt   time.Time `yaml:"created_at"`
	UpdatedAt   time.Time `yaml:"updated_at,omitempty"`
	// UseCount drives curation: a skill nobody uses is a candidate for
	// archiving, and one used often is worth keeping sharp.
	UseCount   int       `yaml:"use_count"`
	LastUsedAt time.Time `yaml:"last_used_at,omitempty"`
	// Pinned exempts a skill from every automatic transition.
	Pinned bool `yaml:"pinned,omitempty"`
	// Related names sibling skills, for the learning graph.
	Related []string `yaml:"related,omitempty"`
	// PromotedBy records the eval run that let this skill out of quarantine.
	// A skill in StateActive without it is a bug, and Validate says so.
	PromotedBy string `yaml:"promoted_by,omitempty"`
	// Reason explains the most recent transition out of active — archived or
	// demoted — so it can be reversed knowingly.
	Reason string `yaml:"reason,omitempty"`

	// Body is the Markdown the model actually reads.
	Body string `yaml:"-"`
}

// nameFormat keeps skill names safe as directory names and readable in prompts.
// The rejection of dots and separators is what stops a skill called
// "../../etc/passwd" from being written outside the store.
var nameFormat = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether name may be used as a skill name.
func ValidName(name string) bool {
	return len(name) <= 64 && nameFormat.MatchString(name)
}

// Validate checks a skill's internal consistency.
func (s *Skill) Validate() error {
	if !ValidName(s.Name) {
		return fmt.Errorf("skill: %q is not a valid name; use lowercase words joined by hyphens", s.Name)
	}
	if strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("skill %s: needs a description; it is what the model reads to decide whether to use it", s.Name)
	}
	if !s.State.Valid() {
		return fmt.Errorf("skill %s: unknown state %q", s.Name, s.State)
	}
	if s.CreatedBy != ByAgent && s.CreatedBy != ByUser {
		return fmt.Errorf("skill %s: unknown author %q", s.Name, s.CreatedBy)
	}
	if strings.TrimSpace(s.Body) == "" {
		return fmt.Errorf("skill %s: has no instructions", s.Name)
	}
	// The gate's whole purpose is that this cannot happen quietly.
	if s.State == StateActive && s.CreatedBy == ByAgent && s.PromotedBy == "" {
		return fmt.Errorf("skill %s: is active but carries no evidence of passing evals", s.Name)
	}
	return nil
}

// IsAgentCreated reports whether automatic curation may touch this skill.
func (s *Skill) IsAgentCreated() bool { return s.CreatedBy == ByAgent }

// Curatable reports whether the curator may transition this skill.
//
// Pinned skills and anything a person wrote are off limits — automatic
// maintenance should never undo a deliberate human decision.
func (s *Skill) Curatable() bool { return s.IsAgentCreated() && !s.Pinned }

// Prompt renders the skill for a system prompt.
func (s *Skill) Prompt() string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(s.Name)
	b.WriteString("\n\n")
	b.WriteString(s.Description)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(s.Body))
	b.WriteString("\n")
	return b.String()
}

// New builds a skill in quarantine, which is the only way a skill may start.
func New(name, description, body string, author Author, now time.Time) (*Skill, error) {
	s := &Skill{
		Name:        name,
		Description: strings.TrimSpace(description),
		State:       StateQuarantine,
		CreatedBy:   author,
		CreatedAt:   now.UTC(),
		Body:        body,
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}
