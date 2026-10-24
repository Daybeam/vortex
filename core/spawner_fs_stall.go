package core

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
)

// fsOnlyStallGuard updates the fs-only turn streak for this turn and reports
// whether the configured limit was reached, in which case it also persists the
// partial trace and returns the sentinel error the caller must propagate.
//
// Extracted from doSpawn (size gate); behaviour is identical to the inline
// block — #15 (eval §8.35) no-progress detection plus #4+#15 (eval
// §8.37.2/§8.40) break-the-loop. A turn with no tool calls leaves the streak
// untouched.
func (s *Spawner) fsOnlyStallGuard(
	ctx context.Context, req *SpawnRequest, role *config.Role,
	resp *providers.ProviderResponse, trace []store.ToolInteraction,
	streak, streakLimit, turn, maxTurns int, toolResults []string,
	effectiveProviderID, effectiveModelID string,
) (int, error) {
	if len(resp.ToolCalls) == 0 {
		return streak, nil
	}
	allFsOnly := true
	for _, tc := range resp.ToolCalls {
		if !CoreToolNames[tc.Name] {
			allFsOnly = false
			break
		}
	}
	if allFsOnly {
		streak++
	} else {
		if streak > 0 {
			observability.GetGlobalMetrics().Inc("orchestrator.progress.fs_only_streak_max", int64(streak))
		}
		streak = 0
	}
	if streak >= 2 {
		observability.GetGlobalMetrics().Inc("orchestrator.progress.stalled_turns", 1)
	}
	// #4+#15 (eval §8.37.2/§8.40): Break the turn loop if the fs-only streak
	// exceeds the configured limit. The model is stuck in a write→read
	// self-proof loop with no domain progress.
	if streakLimit > 0 && streak >= streakLimit {
		observability.GetGlobalMetrics().Inc("orchestrator.progress.fs_only_stall_break", 1)
		s.logger.Log("EventFsOnlyStallBreak", req.TaskID, req.StepID, map[string]any{
			"turn":         turn,
			"streak":       streak,
			"limit":        streakLimit,
			"max_turns":    maxTurns,
			"tool_results": len(toolResults),
		})
		// Persist partial trace for observability (eval §8.48.6).
		s.persistTraceOnError(ctx, req, trace, effectiveProviderID, effectiveModelID, role.BaseCapability)
		return streak, fmt.Errorf("fs_only_stall: %d consecutive turns used only filesystem tools (write_file/read_file/execute_code) without domain MCP calls; breaking to conserve budget (limit=%d)", streak, streakLimit)
	}
	return streak, nil
}
