package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

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
