package core

import "context"

// spawner_capability_resolve.go — capability-gap resolution helpers
// extracted from spawner_utils.go to keep that file under its size budget.

// tryFindSkillsForMissing resolves a Generatable role's missing capabilities
// via the fast findskills path (tier-1). It appends every found skill to
// req.AdditionalSkills and returns true only when EVERY missing capability is
// covered.
//
// SECURITY FIX (2026-10-21, issue 5): a partial hit is NOT a satisfied
// capability. Previously ANY found skill made this return nil, so a role with
// missing=[a,b] passed as long as `a` was covered while `b` stayed
// unsatisfied. Require full coverage; otherwise the caller falls through to
// dynamic expansion (warning) or RoleGenerator escalation (error).
//
// NOTE: with the current exact-match semantics this partial case is latent
// (CheckCapabilities already treats a skill-covered capability as satisfied,
// so it never appears in `missing`) — this is defensive hardening against a
// future change to the matching rule.
func (s *Spawner) tryFindSkillsForMissing(ctx context.Context, req *SpawnRequest, missing []string) bool {
	var foundSkills []string
	uncovered := 0
	for _, missingCap := range missing {
		result, _ := FindSkillsImpl(req.Hub.Registry, "", missingCap, "", 1, 10)
		if result == nil || len(result.Skills) == 0 {
			uncovered++
			continue
		}
		for _, sk := range result.Skills {
			foundSkills = append(foundSkills, sk.ID)
		}
	}
	if len(foundSkills) == 0 || uncovered > 0 {
		return false
	}
	req.AdditionalSkills = append(req.AdditionalSkills, foundSkills...)
	s.logger.LogCtx(ctx, "EventFindSkillsResolved", req.TaskID, req.StepID, map[string]any{
		"role":         req.RoleID,
		"missing":      missing,
		"found_skills": foundSkills,
		"source":       "findskills_tier1",
	})
	return true
}
