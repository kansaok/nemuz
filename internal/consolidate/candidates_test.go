package consolidate

import (
	"testing"
	"time"

	"github.com/kansaok/nemuz/internal/skill"
)

func makeSkill(t *testing.T, name, description string, state skill.State) *skill.Skill {
	t.Helper()
	sk, err := skill.New(name, description, "Langkah.", skill.ByAgent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sk.State = state
	if state == skill.StateActive {
		sk.PromotedBy = "eval-lama"
	}
	return sk
}

func TestOverlappingDescriptionsAreCandidates(t *testing.T) {
	a := makeSkill(t, "deploy-staging", "Deploy proyek ini ke staging dengan ship.sh", skill.StateActive)
	b := makeSkill(t, "deploy-ke-staging", "Cara deploy ke staging pakai skrip ship.sh", skill.StateActive)

	pairs := FindCandidates([]*skill.Skill{a, b}, 5)
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1: %+v", len(pairs), pairs)
	}
	if pairs[0].Overlap <= 0 {
		t.Errorf("overlap is %v, want positive", pairs[0].Overlap)
	}
}

func TestUnrelatedDescriptionsAreNotCandidates(t *testing.T) {
	a := makeSkill(t, "deploy-staging", "Deploy proyek ini ke staging", skill.StateActive)
	b := makeSkill(t, "format-tanggal", "Ubah tanggal ke format ISO 8601", skill.StateActive)

	pairs := FindCandidates([]*skill.Skill{a, b}, 5)
	if len(pairs) != 0 {
		t.Fatalf("unrelated skills were paired: %+v", pairs)
	}
}

// TestOnlyActiveCuratableSkillsAreConsidered is the stated scope: quarantined
// drafts are still speculative, and a hand-written skill is a person's
// deliberate act that consolidation must not touch either.
func TestOnlyActiveCuratableSkillsAreConsidered(t *testing.T) {
	active := makeSkill(t, "deploy-a", "Deploy ke staging dengan ship.sh", skill.StateActive)
	quarantined := makeSkill(t, "deploy-b", "Deploy ke staging dengan ship.sh", skill.StateQuarantine)
	human := makeSkill(t, "deploy-c", "Deploy ke staging dengan ship.sh", skill.StateActive)
	human.CreatedBy = skill.ByUser
	pinned := makeSkill(t, "deploy-d", "Deploy ke staging dengan ship.sh", skill.StateActive)
	pinned.Pinned = true

	pairs := FindCandidates([]*skill.Skill{active, quarantined, human, pinned}, 5)
	if len(pairs) != 0 {
		t.Fatalf("a pair was formed touching a non-curatable or non-active skill: %+v", pairs)
	}
}

func TestMaxPairsIsRespected(t *testing.T) {
	var skills []*skill.Skill
	for i := 0; i < 5; i++ {
		skills = append(skills, makeSkill(t, string(rune('a'+i))+"-deploy-staging",
			"Deploy ke staging dengan ship.sh dan verifikasi hasil", skill.StateActive))
	}
	pairs := FindCandidates(skills, 2)
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want the cap of 2", len(pairs))
	}
}

func TestCandidatesAreOrderedByOverlap(t *testing.T) {
	a := makeSkill(t, "a", "deploy staging ship release build", skill.StateActive)
	closeMatch := makeSkill(t, "b", "deploy staging ship release build proses", skill.StateActive)
	farMatch := makeSkill(t, "c", "deploy staging saja", skill.StateActive)

	pairs := FindCandidates([]*skill.Skill{a, closeMatch, farMatch}, 5)
	if len(pairs) < 2 {
		t.Fatalf("got %d pairs, want at least 2", len(pairs))
	}
	if pairs[0].Overlap < pairs[1].Overlap {
		t.Errorf("pairs are not sorted by overlap: %+v", pairs)
	}
}

func TestFingerprintIgnoresOrder(t *testing.T) {
	a := makeSkill(t, "skill-a", "x", skill.StateActive)
	b := makeSkill(t, "skill-b", "x", skill.StateActive)

	p1 := Pair{A: a, B: b}
	p2 := Pair{A: b, B: a}
	if p1.Fingerprint() != p2.Fingerprint() {
		t.Errorf("fingerprints differ by argument order: %q vs %q", p1.Fingerprint(), p2.Fingerprint())
	}
}
