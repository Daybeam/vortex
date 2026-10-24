package core

import (
	"fmt"
	"os"

	"github.com/daybeam/vortex/store"
)

// handleStagnationTurn applies the output-stagnation decision for one turn.
// Extracted from doSpawn (size gate) — behaviour is identical to the former
// inline switch:
//
//   - stagHint  -> returns the injected hint text (caller sets hinted/hintAt)
//   - stagKill  -> returns a partial SpawnResult the caller must return
//   - stagGrace -> observe-only (added 2026-10-23): no hint, no kill
//
// The caller owns the counter/state updates so the control flow is unchanged.
func (s *Spawner) handleStagnationTurn(
	req *SpawnRequest,
	turn, repeats, grace, hintAt, threshold int,
	hinted bool,
	trace []store.ToolInteraction,
	capability string,
) (hintText string, kill *SpawnResult) {
	switch stagnationStep(repeats, hintAt, grace, threshold, hinted) {
	case stagHint:
		s.logger.Log("EventOutputStagnationHinted", req.TaskID, req.StepID, map[string]any{
			"turn": turn, "consecutive_count": repeats,
		})
		return "System: You have produced identical tool results multiple times. You appear to be stuck re-fetching the same data. Summarize what you have found so far and proceed to complete the task. Do not repeat the same tool calls.", nil

	case stagKill:
		s.logger.Log(EventStepFailed, req.TaskID, req.StepID, map[string]any{
			"error":             "output_stagnation: identical tool results repeated after hint",
			"consecutive_count": repeats,
			"root_cause":        "model_stuck",
		})
		// A8 (eval FINDINGS.md §A8, 2026-10-22): the hint stage logs
		// EventOutputStagnationHinted, but the kill itself was invisible to
		// EVAL-OBS greps. EventStepFailed above is always emitted; this only
		// adds the eval channel.
		if s.registry.System.FeatureFlags.EvalObs {
			fmt.Fprintf(os.Stderr, "[EVAL-OBS] STAGNATION KILL task=%s step=%s turn=%d trace=%d repeats=%d\n",
				req.TaskID, req.StepID, turn+1, len(trace), repeats)
		}
		return "", s.partialStagnationOutput(capability,
			fmt.Sprintf("output_stagnation: identical tool results repeated %d consecutive times after intervention hint", repeats),
			trace)

	case stagGrace:
		// ADDED 2026-10-23: inside the grace window — observe only, no hint
		// re-injection and no kill. Emitted so the A/B can tell "grace was never
		// reached" from "grace was reached and the model still failed to escape".
		if s.registry.System.FeatureFlags.EvalObs {
			fmt.Fprintf(os.Stderr, "[EVAL-OBS] STAGNATION GRACE task=%s step=%s turn=%d repeats=%d grace=%d used=%d\n",
				req.TaskID, req.StepID, turn+1, repeats, grace, repeats-hintAt)
		}
	}
	return "", nil
}
