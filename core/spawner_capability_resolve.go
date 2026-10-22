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
// TODO 1+2 RESOLVED (2026-10-22): candidates are now ordered by learned
// affinity and the experience library is searched, not just reg.Skills —
// store.QuerySkillRecommendations returns SkillAffinities entries ranked
// best-first by ConfidenceDelta. Only recommendation IDs that resolve in the
// registry are taken (AdditionalSkills must be resolvable by
// ResolveFinalSkills); registry hits fill the remainder in registry order.
func (s *Spawner) tryFindSkillsForMissing(ctx context.Context, req *SpawnRequest, missing []string) bool {
	var foundSkills []string
	uncovered := 0
	for _, missingCap := range missing {
		candidates := s.skillCandidatesForCapability(ctx, req, missingCap)
		if len(candidates) == 0 {
			uncovered++
			continue
		}
		foundSkills = append(foundSkills, candidates...)
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

// skillCandidatesForCapability returns registry-resolvable skill IDs for one
// missing capability, affinity-ordered: experience-library recommendations
// first (ranked best-first by the store), then remaining registry hits.
func (s *Spawner) skillCandidatesForCapability(ctx context.Context, req *SpawnRequest, capability string) []string {
	result, _ := FindSkillsImpl(req.Hub.Registry, "", capability, "", 1, 10)
	var registryHits []string
	if result != nil {
		for _, sk := range result.Skills {
			registryHits = append(registryHits, sk.ID)
		}
	}
	if s.expStore == nil {
		return registryHits
	}
	recommended := s.expStore.QuerySkillRecommendations(ctx, capability, 0)
	if len(recommended) == 0 {
		return registryHits
	}
	seen := make(map[string]bool, len(recommended)+len(registryHits))
	ordered := make([]string, 0, len(registryHits)+len(recommended))
	for _, sid := range recommended {
		if sid == "" || seen[sid] {
			continue
		}
		if req.Hub.GetSkill(sid) == nil {
			continue // foreign ID — unresolvable as an AdditionalSkill
		}
		seen[sid] = true
		ordered = append(ordered, sid)
	}
	for _, sid := range registryHits {
		if !seen[sid] {
			seen[sid] = true
			ordered = append(ordered, sid)
		}
	}
	return ordered
}