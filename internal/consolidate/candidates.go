// Package consolidate finds skills the agent taught itself that overlap, and
// proposes merging them into one.
//
// The curator (internal/curator) re-verifies, retires and expires skills
// without ever calling a model — that is what lets it run automatically and
// for free. Consolidation cannot make that promise: whether two skills are
// "the same idea written twice" is a judgement about meaning, not something a
// mechanical rule can decide. It is therefore its own explicit step, run on
// demand, the same way review is — never folded into the free curator loop.
//
// # Why a cheap local pass comes first
//
// A model call costs money and a wait. Asking it about every pair of skills
// scales quadratically and would burn both on pairs that plainly have nothing
// in common. Candidates are filtered locally first — by how many words their
// descriptions share — so the model is only asked about pairs that already
// look plausibly related.
package consolidate

import (
	"regexp"
	"sort"
	"strings"

	"github.com/kansaok/nemuz/internal/skill"
)

// DefaultMaxPairs bounds how many candidate pairs are sent to the model in one
// run. A skill collection that has accumulated many near-duplicates should be
// worked through a few at a time, not all at once — the same conservatism the
// review novelty ledger applies to reviewing turns.
const DefaultMaxPairs = 5

// MinOverlap is the lowest word-overlap score worth asking the model about.
// Below it, two descriptions share too little to plausibly be the same idea,
// and the pair is dropped for free rather than sent anywhere.
const MinOverlap = 0.2

// Pair is two skills whose descriptions overlap enough to ask the model about.
type Pair struct {
	A, B    *skill.Skill
	Overlap float64
}

// FindCandidates returns pairs of curatable, same-state skills worth asking
// the model to compare, most-similar first, capped at maxPairs.
//
// Scoped to Active skills only: that is where duplication actually causes
// confusion, when the agent has to pick which of two proven skills to reach
// for mid-turn. Quarantined drafts are still speculative, and consolidating
// across states — merging something proven with something not — would muddy
// what the resulting evidence means. Left for later, and said so rather than
// silently assumed away.
func FindCandidates(skills []*skill.Skill, maxPairs int) []Pair {
	if maxPairs <= 0 {
		maxPairs = DefaultMaxPairs
	}

	var active []*skill.Skill
	for _, sk := range skills {
		if sk.State == skill.StateActive && sk.Curatable() {
			active = append(active, sk)
		}
	}

	var pairs []Pair
	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			overlap := descriptionOverlap(active[i], active[j])
			if overlap >= MinOverlap {
				pairs = append(pairs, Pair{A: active[i], B: active[j], Overlap: overlap})
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Overlap != pairs[j].Overlap {
			return pairs[i].Overlap > pairs[j].Overlap
		}
		// A stable tiebreak, so which pairs get chosen when many tie does not
		// depend on map or slice iteration order.
		return pairs[i].A.Name+pairs[i].B.Name < pairs[j].A.Name+pairs[j].B.Name
	})
	if len(pairs) > maxPairs {
		pairs = pairs[:maxPairs]
	}
	return pairs
}

// Fingerprint identifies a pair regardless of argument order, for the novelty
// ledger: "A and B" and "B and A" are the same question asked twice.
func (p Pair) Fingerprint() string {
	names := []string{p.A.Name, p.B.Name}
	sort.Strings(names)
	return "consolidate:" + names[0] + "|" + names[1]
}

// wordPattern and stopWords mirror the tiny lexical scorer in internal/memory,
// duplicated rather than imported: the two packages are answering different
// questions (what should be recalled, versus what two things have in common)
// and coupling them for a handful of lines would cost more in cross-package
// reasoning than it saves in code.
var wordPattern = regexp.MustCompile(`[a-z0-9_]+`)

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "you": true, "are": true, "was": true,
	"this": true, "that": true, "with": true, "from": true, "have": true, "has": true,
	"what": true, "how": true, "why": true, "can": true, "not": true, "but": true,
	"yang": true, "dan": true, "untuk": true, "dari": true, "ini": true, "itu": true,
	"ada": true, "apa": true, "ke": true, "di": true, "saya": true, "kamu": true,
	"bisa": true, "tidak": true, "dengan": true, "pada": true, "atau": true,
}

func significantWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordPattern.FindAllString(strings.ToLower(text), -1) {
		if len(w) >= 3 && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// descriptionOverlap is the Jaccard similarity of two skills' descriptions:
// shared significant words over the union of both. Short one-line descriptions
// are exactly what a skill's Description field holds, which makes this a cheap
// and reasonably honest proxy for "are these about the same thing."
func descriptionOverlap(a, b *skill.Skill) float64 {
	wa, wb := significantWords(a.Description), significantWords(b.Description)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	var shared int
	for w := range wa {
		if wb[w] {
			shared++
		}
	}
	union := len(wa) + len(wb) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
