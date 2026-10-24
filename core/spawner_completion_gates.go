package core

import (
	"fmt"
	"os"

	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// completionGateState carries the two one-shot gate flags and the user-facing
// blocks the gates append to. Extracted from doSpawn (size gate) so the gates
// can move out; the caller copies userBlocks back after the calls, since the
// slice is shared with the turn loop.
type completionGateState struct {
	zeroEffectNudged bool
	midAbandonNudged bool
	userBlocks       []schemas.ContentBlock
}

// zeroEffectGate reports whether the turn was nudged (the caller then skips the
// rest of the turn). Behaviour is identical to the inline block it replaces:
// the gate fires at most once, only when the trace has no side-effecting call.
//
// The gate also fires on parseFailed (A2): ParseOutput degrading the output to
// partial is the ENGINE's artifact, not the model declaring completion, and the
// parse-fail variant keeps the injected text honest.
func (s *Spawner) zeroEffectGate(
	req *SpawnRequest, st *completionGateState,
	output schemas.SubagentOutput, trace []store.ToolInteraction,
	turn, maxTurns int, parseFailed, noToolCallsYet bool, cleanText string,
) bool {
	if !s.registry.System.FeatureFlags.ZeroEffectNudge || st.zeroEffectNudged || noToolCallsYet ||
		(output.Status != schemas.StatusOK && !parseFailed) || turn >= maxTurns-1 {
		return false
	}
	hasEffect := false
	seenTools := make([]string, 0, len(trace))
	for _, t := range trace {
		seenTools = append(seenTools, t.ToolName)
		if !IsReadOnlyToolName(t.ToolName) {
			hasEffect = true
		}
	}
	if hasEffect {
		return false
	}
	st.zeroEffectNudged = true
	s.logger.Log("EventZeroEffectNudged", req.TaskID, req.StepID, map[string]any{
		"turn":         turn + 1,
		"trace_len":    len(trace),
		"trace_tools":  seenTools,
		"confidence":   output.Confidence,
		"parse_failed": parseFailed,
	})
	observability.GetGlobalMetrics().Inc("orchestrator.zero_effect.nudged", 1)
	if s.registry.System.FeatureFlags.EvalObs {
		fmt.Fprintf(os.Stderr, "[EVAL-OBS] ZERO-EFFECT NUDGE turn=%d trace=%d conf=%.2f parse=%v tools=%v\n",
			turn+1, len(trace), output.Confidence, parseFailed, seenTools)
	}
	for i := range st.userBlocks {
		st.userBlocks[i].CacheControl = ""
	}
	st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: cleanText})
	msg := SpawnerZeroEffectMsg
	if parseFailed {
		msg = SpawnerZeroEffectParseFailMsg
	}
	st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: msg, CacheControl: "ephemeral"})
	return true
}

// midAbandonGate reports whether the turn was nudged. The highest-frequency
// small-model failure mode: work started (read-only trace), friction hit, then a
// PARSEABLE partial while turns remain. One-shot and opt-in (mid_abandon_nudge).
// `!parseFailed` keeps the message honest — a parse-collapsed turn gets the
// zero-effect parse-fail variant instead (A2).
func (s *Spawner) midAbandonGate(
	req *SpawnRequest, st *completionGateState,
	output schemas.SubagentOutput, trace []store.ToolInteraction,
	turn, maxTurns int, parseFailed, noToolCallsYet bool, cleanText string,
) bool {
	if !s.registry.System.FeatureFlags.MidAbandonNudge || st.midAbandonNudged || noToolCallsYet ||
		output.Status != schemas.StatusPartial || parseFailed || turn >= maxTurns-1 ||
		traceHasEffect(trace) {
		return false
	}
	st.midAbandonNudged = true
	seenTools := make([]string, 0, len(trace))
	for _, t := range trace {
		seenTools = append(seenTools, t.ToolName)
	}
	s.logger.Log("EventMidAbandonNudged", req.TaskID, req.StepID, map[string]any{
		"turn":        turn + 1,
		"trace_len":   len(trace),
		"trace_tools": seenTools,
		"confidence":  output.Confidence,
	})
	observability.GetGlobalMetrics().Inc("orchestrator.mid_abandon.nudged", 1)
	if s.registry.System.FeatureFlags.EvalObs {
		fmt.Fprintf(os.Stderr, "[EVAL-OBS] MID-ABANDON NUDGE turn=%d trace=%d conf=%.2f tools=%v\n",
			turn+1, len(trace), output.Confidence, seenTools)
	}
	for i := range st.userBlocks {
		st.userBlocks[i].CacheControl = ""
	}
	st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: cleanText})
	st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: SpawnerMidAbandonMsg, CacheControl: "ephemeral"})
	return true
}
