package core

import (
	"strings"

	"github.com/daybeam/vortex/config"
)

// SkillRouter selects a subset of skills from the provided list based on task relevance
// using probabilistic data structures (Cuckoo Filters) for high-performance perception.
type SkillRouter struct {
	Registry *config.Registry
}

func NewSkillRouter(reg *config.Registry) *SkillRouter {
	return &SkillRouter{Registry: reg}
}

// SkillRouteRequest defines parameters for skill routing.
type SkillRouteRequest struct {
	Task            string
	AvailableSkills []string
	Turn            int
	ProgressiveMode bool
}

// Route identifies relevant skills from a set of available skill IDs.
func (r *SkillRouter) Route(req SkillRouteRequest) []string {
	if len(req.AvailableSkills) <= 3 {
		return req.AvailableSkills // Small set doesn't need pruning
	}

	taskLower := strings.ToLower(req.Task)
	taskWords := strings.FieldsFunc(taskLower, func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':'
	})

	var routed []string
	for _, id := range req.AvailableSkills {
		skill := r.Registry.Skills[id]
		if skill == nil {
			continue
		}

		// Priority 4: Domain filtering
		if skill.Domain != "" && skill.Domain != config.DomainGeneral {
			if !strings.Contains(taskLower, strings.ToLower(skill.Domain)) {
				// If domain not mentioned, skip if progressive mode is on
				if req.ProgressiveMode && req.Turn == 0 {
					continue
				}
			}
		}

		// 1. Direct ID match (High signal).
		// FIX (2026-07-17): use whole-word matching, not raw substring containment.
		// core/tool_router.go's isRelevant() suffered three separate regressions
		// (documented F11/F15/06-30 addendum) from exactly this pattern -- e.g. a
		// skill ID like "log" or "run" would falsely match inside unrelated words
		// ("catalog", "running"). Reuses containsWholeWord/isAlphaNum already
		// defined in tool_router.go (same package), rather than duplicating them.
		if containsWholeWord(taskLower, strings.ToLower(skill.ID)) {
			routed = append(routed, id)
			continue
		}

		// 2. High-performance Cuckoo Filter Collision (Perception Layer)
		if skill.Filter != nil {
			matched := false
			for _, word := range taskWords {
				if len(word) < 3 {
					continue
				}
				if skill.Filter.Test(word) {
					matched = true
					break
				}
			}
			if matched {
				routed = append(routed, id)
			}
		}
	}

	// FIX (2026-08-18): remove the arbitrary "first 3" fallback. If nothing
	// matched, return an empty set to avoid polluting the prompt with
	// irrelevant instructions. The agent can still function with its base
	// role instructions. Returning a random subset (which might include
	// substring-match traps like "log") was causing test failures and
	// unnecessary token consumption.
	return routed
}
