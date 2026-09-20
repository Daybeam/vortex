package store

import (
	"strings"

	"github.com/daybeam/vortex/config"
)

// PruneStaleJITRefs scans TaskPatterns for references to expired jit_ tool IDs
// and removes patterns that reference them. Returns the count of pruned patterns.
// Implements JIT_LIFECYCLE_AND_SELF_CORRECTION_ARCH.md §3.2.
func (s *ExperienceStore) PruneStaleJITRefs(reg *config.Registry) int {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	pruned := 0
	for id, pat := range s.TaskPatterns {
		if hasStaleJITRef(pat.StepSequence, reg) {
			delete(s.TaskPatterns, id)
			pruned++
		}
	}
	return pruned
}

func hasStaleJITRef(steps []map[string]any, reg *config.Registry) bool {
	for _, step := range steps {
		for _, v := range step {
			if findStaleJIT(v, reg) {
				return true
			}
		}
	}
	return false
}

func findStaleJIT(v any, reg *config.Registry) bool {
	switch val := v.(type) {
	case string:
		if strings.HasPrefix(val, "jit_") && reg.GetMCP(val) == nil {
			return true
		}
	case []any:
		for _, item := range val {
			if findStaleJIT(item, reg) {
				return true
			}
		}
	case map[string]any:
		for _, item := range val {
			if findStaleJIT(item, reg) {
				return true
			}
		}
	}
	return false
}
