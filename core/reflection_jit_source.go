package core

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/daybeam/vortex/pkg/env"
	"github.com/daybeam/vortex/store"
)

// captureTraceJITSkills folds dynamically generated tools that appear in a
// successful step's trace into the step's skill list and seeds a
// GeneratedSkill record on first sighting. Without the seed, maybePromoteSkill
// can never find an ephemeral JIT tool, so its UsageCount/SuccessRate gate is
// unreachable for tools that were never crystallized.
//
// The generated script body is captured on the record: JIT tools expire after
// their TTL while the record lives on, and without a persisted copy additive
// editing and hybrid merge silently lose their baseline once the tool is gone.
func (re *ReflectionEngine) captureTraceJITSkills(ctx context.Context, capability string, trace []store.ToolInteraction, skills []string) []string {
	if len(trace) == 0 {
		return skills
	}
	seen := make(map[string]bool, len(skills))
	for _, sk := range skills {
		seen[sk] = true
	}
	for _, t := range trace {
		if !strings.HasPrefix(t.ToolName, "jit_") || seen[t.ToolName] {
			continue
		}
		seen[t.ToolName] = true
		skills = append(skills, t.ToolName)
		if _, exists := re.expStore.GetGeneratedSkill(t.ToolName); !exists {
			re.expStore.AddGeneratedSkill(ctx, &store.GeneratedSkill{
				ID:         t.ToolName,
				Capability: capability,
				// SuccessRate starts at 1.0: we only seed on StepOK.
				// UpdateRouteWeight in RecordTaskCompletion will blend in
				// subsequent failures on the affinity side, and
				// maybePromoteSkill's > 0.8 gate will naturally filter out
				// tools that start failing.
				SuccessRate: 1.0,
				LastUsed:    time.Now(),
				OS:          runtime.GOOS,
				Arch:        runtime.GOARCH,
				Shell:       env.GetShellVersion(),
				SourceText:  re.liveToolSource(t.ToolName),
			})
		} else {
			re.expStore.IncrementGeneratedSkillUsage(t.ToolName)
		}
	}
	return skills
}

// liveToolSource returns a JIT script body while its tool is still registered.
func (re *ReflectionEngine) liveToolSource(id string) string {
	if re.jit == nil {
		return ""
	}
	src, err := re.jit.GetSource(id)
	if err != nil {
		return ""
	}
	return src
}

// skillSource returns a generated skill's script body, preferring the live JIT
// registry and falling back to the body persisted on the skill record. JIT
// tools expire by design (24h TTL); the skill record does not, so the persisted
// copy is the only baseline left for additive editing and hybrid merge.
func (re *ReflectionEngine) skillSource(id string) (string, bool) {
	if src := re.liveToolSource(id); src != "" {
		return src, true
	}
	if re.expStore != nil {
		if gs, ok := re.expStore.GetGeneratedSkill(id); ok && gs.SourceText != "" {
			return gs.SourceText, true
		}
	}
	return "", false
}
