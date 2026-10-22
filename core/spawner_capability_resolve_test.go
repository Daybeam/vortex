package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// recExpStore fakes only QuerySkillRecommendations (interface embedding keeps
// the rest nil-panicking, which the code under test must not reach).
type recExpStore struct {
	store.IExperienceStore
	recs map[string][]string
}

func (m *recExpStore) QuerySkillRecommendations(ctx context.Context, capability string, minConfidence float64) []string {
	return m.recs[capability]
}

// Regression (security-audit issue 5, 2026-10-21): a PARTIAL findskills hit
// must not be treated as a satisfied capability. tryFindSkillsForMissing must
// return true only when EVERY missing capability is covered by a discovered
// skill; otherwise the caller falls through to dynamic expansion / RoleGenerator.
func TestTryFindSkillsForMissing_RequiresFullCoverage(t *testing.T) {
	logger := mustNewLogger(t, mustTempDir(t, "capresolve"), nil)

	makeReq := func(reg *config.Registry) *SpawnRequest {
		return &SpawnRequest{
			TaskID: "t1", StepID: "s1", RoleID: "r1",
			Hub: &ContextHub{Registry: reg},
		}
	}

	t.Run("partial coverage returns false", func(t *testing.T) {
		reg := &config.Registry{Skills: map[string]*config.Skill{
			"sk_a": {ID: "sk_a", Capability: "cap_a"},
		}}
		s := &Spawner{logger: logger}
		if s.tryFindSkillsForMissing(context.Background(), makeReq(reg), []string{"cap_a", "cap_b"}) {
			t.Fatal("only cap_a covered — partial hit must NOT be treated as satisfied")
		}
	})

	t.Run("no coverage returns false", func(t *testing.T) {
		reg := &config.Registry{Skills: map[string]*config.Skill{}}
		s := &Spawner{logger: logger}
		if s.tryFindSkillsForMissing(context.Background(), makeReq(reg), []string{"cap_a"}) {
			t.Fatal("no skill found — must NOT be treated as satisfied")
		}
	})

	t.Run("full coverage returns true and appends skills", func(t *testing.T) {
		reg := &config.Registry{Skills: map[string]*config.Skill{
			"sk_a": {ID: "sk_a", Capability: "cap_a"},
			"sk_b": {ID: "sk_b", Capability: "cap_b"},
		}}
		s := &Spawner{logger: logger}
		req := makeReq(reg)
		if !s.tryFindSkillsForMissing(context.Background(), req, []string{"cap_a", "cap_b"}) {
			t.Fatal("full coverage must be satisfied")
		}
		if len(req.AdditionalSkills) != 2 {
			t.Fatalf("expected 2 appended skills, got %v", req.AdditionalSkills)
		}
	})
}

// Regression (findskills TODO 1+2, 2026-10-22): candidates must be ordered by
// learned affinity — experience-library recommendations (QuerySkillRecommendations,
// ranked best-first by ConfidenceDelta) come BEFORE plain registry hits, and
// recommendation IDs that don't resolve in the registry are skipped (they
// cannot be AdditionalSkills).
func TestTryFindSkillsForMissing_AffinityOrderedCandidates(t *testing.T) {
	logger := mustNewLogger(t, mustTempDir(t, "capresolve2"), nil)
	reg := &config.Registry{Skills: map[string]*config.Skill{
		"sk_a": {ID: "sk_a", Capability: "cap_a"},
		"sk_b": {ID: "sk_b", Capability: "cap_a"},
	}}
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "r1", Hub: &ContextHub{Registry: reg}}

	t.Run("recommendation ranks first, registry hit follows", func(t *testing.T) {
		s := &Spawner{logger: logger, expStore: &recExpStore{recs: map[string][]string{
			"cap_a": {"sk_b"}, // learned affinity prefers sk_b
		}}}
		if !s.tryFindSkillsForMissing(context.Background(), req, []string{"cap_a"}) {
			t.Fatal("full coverage expected")
		}
		if len(req.AdditionalSkills) != 2 || req.AdditionalSkills[0] != "sk_b" {
			t.Fatalf("affinity winner must rank first, got %v", req.AdditionalSkills)
		}
	})

	t.Run("foreign recommendation id is skipped, not appended", func(t *testing.T) {
		req2 := &SpawnRequest{TaskID: "t2", StepID: "s2", RoleID: "r1", Hub: &ContextHub{Registry: reg}}
		s := &Spawner{logger: logger, expStore: &recExpStore{recs: map[string][]string{
			"cap_a": {"gen_sk_9"}, // experience-only id, absent from registry
		}}}
		if !s.tryFindSkillsForMissing(context.Background(), req2, []string{"cap_a"}) {
			t.Fatal("registry hits still cover the capability")
		}
		for _, sid := range req2.AdditionalSkills {
			if sid == "gen_sk_9" {
				t.Fatal("unresolvable foreign skill id must not be appended")
			}
		}
	})

	t.Run("nil expStore keeps registry-only behavior", func(t *testing.T) {
		req3 := &SpawnRequest{TaskID: "t3", StepID: "s3", RoleID: "r1", Hub: &ContextHub{Registry: reg}}
		s := &Spawner{logger: logger}
		if !s.tryFindSkillsForMissing(context.Background(), req3, []string{"cap_a"}) {
			t.Fatal("registry-only path must still cover")
		}
	})
}
