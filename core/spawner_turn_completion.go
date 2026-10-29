package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// turnOutcome reports what an extracted turn block wants the loop to do next.
// An extracted block can express all three control flows the inline code used:
// a terminal `return`, a `continue` (skip the rest of this turn), or falling
// through to the next block.
type turnOutcome struct {
	done   bool         // terminal: the caller returns result
	skip   bool         // 'continue': skip the rest of this turn's body
	result *SpawnResult // set when done
}

// spawnTurnState holds the loop-carried fields the completion path mutates.
// The caller copies them in and back out, mirroring the toolExecState pattern.
type spawnTurnState struct {
	consecutiveParseFailures int
	zeroEffectNudged         bool
	midAbandonNudged         bool
	userBlocks               []schemas.ContentBlock
	trace                    []store.ToolInteraction
}

// completeTurn handles a turn in which the model produced NO tool calls:
// parse the text, run the one-shot gates and nudges, and either nudge
// (skip), terminate (spin-breaker / completion), or fall through.
//
// Extracted verbatim from doSpawn (size gate); behaviour is unchanged —
// `return X, nil` becomes turnOutcome{done: true, result: X} and `continue`
// becomes turnOutcome{skip: true}.
func (s *Spawner) completeTurn(
	ctx context.Context, req *SpawnRequest, role *config.Role,
	resp *providers.ProviderResponse, st *spawnTurnState,
	turn, maxTurns int, turnDecisionID string,
	effectiveProviderID, effectiveModelID string,
	decisionIDs, statesVisited []string,
	maxConsecutiveParseFailures int,
) turnOutcome {
	cleanText := thoughtRe.ReplaceAllString(resp.Text, "")

	output := schemas.ParseOutput(cleanText, role.BaseCapability)
	output.Usage = &schemas.Usage{
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.PromptTokens + resp.CompletionTokens,
	}
	noToolCallsYet := len(st.trace) == 0
	selfReportedLowConfidence := output.Status == schemas.StatusPartial && output.Confidence < 0.4

	// hasTools: whether any tools are available to this role. When false
	// (e.g., a pure text chat role), the engine should not nudge for tool
	// calls or downgrade status=ok to partial for missing tool calls.
	// (live-test stall fix, 2026-10-29)
	hasTools := len(role.BoundSkills) > 0 || len(role.BoundMCPBindings) > 0 ||
		len(req.AdditionalSkills) > 0 || len(req.AdditionalMCPs) > 0

	// P1 fix (2026-10-03, eval §0.5): Detect parse failures and track consecutive count.
	// ParseOutput fallback sets Assumptions to "output not parseable" or
	// "output parseable but semantically hollow". Use this to distinguish
	// "model didn't call tools" from "model called tools but output was garbage".
	parseFailed := false
	for _, a := range output.Assumptions {
		if strings.Contains(a, "not parseable") || strings.Contains(a, "semantically hollow") {
			parseFailed = true
			break
		}
	}
	if parseFailed {
		st.consecutiveParseFailures++
	} else {
		st.consecutiveParseFailures = 0
	}

	s.logTurnObservation(req, turn, maxTurns, resp, st, output, parseFailed, noToolCallsYet, selfReportedLowConfidence, cleanText)

	// P1 fix (2026-10-03, eval §0.5): Spin breaker.
	// After N consecutive parse failures, stop spinning and return the
	// best partial result. Burning the entire turn budget on identical
	// failures wastes 19-43 provider calls for zero benefit.
	if st.consecutiveParseFailures >= maxConsecutiveParseFailures {
		output.Warnings = append(output.Warnings, fmt.Sprintf(
			"orchestrator: broke spin loop after %d consecutive parse failures", st.consecutiveParseFailures))
		s.logger.Log(EventProviderFallback, req.TaskID, req.StepID, map[string]any{
			"turn":                       turn,
			"consecutive_parse_failures": st.consecutiveParseFailures,
			"action":                     "spin_break",
		})
		observability.GetGlobalMetrics().Inc("orchestrator.spin.broken", 1)
		return turnOutcome{done: true, result: &SpawnResult{
			Output:        output,
			ProviderID:    effectiveProviderID,
			ModelID:       effectiveModelID,
			Ref:           req.TaskID + ":" + req.StepID,
			TurnsUsed:     turn + 1,
			StatesVisited: statesVisited,
		}}
	}

	// FIX (2026-09-07): Malformatted Tool Call Guard.
	// When the model outputs pseudo-XML (<anim:call>, <tool_code>) instead of real JSON tool calls,
	// intercept it and inject a stern correction prompt so it doesn't get trapped.
	if hasMalformedToolCallTag(cleanText) {
		for i := range st.userBlocks {
			st.userBlocks[i].CacheControl = ""
		}
		st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: cleanText})
		st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: SpawnerInvalidToolFormatMsg, CacheControl: "ephemeral"})
		return turnOutcome{skip: true}
	}

	// Nudges a model that has not called a tool yet (turn 0, or a
	// self-reported low-confidence partial). Details in noToolNudge.
	// Skip nudge when the model already returned status=ok with high
	// confidence — no point nudging a good response (live-test stall fix).
	if s.noToolNudge(req, turn, maxTurns, st, output, parseFailed, noToolCallsYet, selfReportedLowConfidence, cleanText) {
		return turnOutcome{skip: true}
	}

	// ── Zero-effect completion gate ────────────────────────────────────
	// Details in spawner_completion_gates.go (size gate).
	gateSt := &completionGateState{
		zeroEffectNudged: st.zeroEffectNudged,
		midAbandonNudged: st.midAbandonNudged,
		userBlocks:       st.userBlocks,
	}
	if s.zeroEffectGate(req, gateSt, output, st.trace, turn, maxTurns, parseFailed, noToolCallsYet, cleanText) {
		st.zeroEffectNudged, st.userBlocks = gateSt.zeroEffectNudged, gateSt.userBlocks
		return turnOutcome{skip: true}
	}

	if hasTools && noToolCallsYet && output.Status == schemas.StatusOK {
		output.Status = schemas.StatusPartial
		if output.Confidence > 0.4 {
			output.Confidence = 0.4
		}
		output.Warnings = append(output.Warnings, "orchestrator: model reported status=ok but no tool calls were recorded; downgraded to partial")
	}

	// ── Mid-abandonment gate ───────────────────────────────────────────
	if s.midAbandonGate(req, gateSt, output, st.trace, turn, maxTurns, parseFailed, noToolCallsYet, cleanText) {
		st.midAbandonNudged, st.userBlocks = gateSt.midAbandonNudged, gateSt.userBlocks
		return turnOutcome{skip: true}
	}

	// ── Priority 5: Update Decision Outcome (Final) ────────────────────
	if turnDecisionID != "" && req.Hub != nil && req.Hub.Graph != nil {
		req.Hub.Graph.UpdateDecisionOutcome(turnDecisionID, string(output.Status))
	}

	s.taskStoreSet(ctx, req.TaskID, req.StepID, &store.StepResult{
		Data:           output.Result,
		Confidence:     output.Confidence,
		MissingContext: output.MissingContext,
		Capability:     role.BaseCapability,
		ProviderID:     effectiveProviderID,
		ModelID:        effectiveModelID,
		Attachments:    output.Attachments,
		Trace:          st.trace,
		DecisionIDs:    decisionIDs,
		StatesVisited:  statesVisited,
		CreatedAt:      time.Now(),
	})

	return turnOutcome{done: true, result: &SpawnResult{
		Output:        output,
		ProviderID:    effectiveProviderID,
		ModelID:       effectiveModelID,
		Ref:           req.TaskID + ":" + req.StepID,
		StatesVisited: statesVisited,
		TurnsUsed:     turn + 1,
	}}
}

// logTurnObservation emits the per-turn [EVAL-OBS] diagnostic. Extracted from
// completeTurn for the size gate (writing it needs `os`; guarding it keeps the
// hot path free of the flag check twice).
func (s *Spawner) logTurnObservation(
	req *SpawnRequest, turn, maxTurns int, resp *providers.ProviderResponse,
	st *spawnTurnState, output schemas.SubagentOutput,
	parseFailed, noToolCallsYet, selfReportedLowConfidence bool, cleanText string,
) {
	if !s.registry.System.FeatureFlags.EvalObs {
		return
	}
	fmt.Fprintf(os.Stderr, "[EVAL-OBS] task=%s step=%s turn=%d/%d calls=%d trace=%d status=%s conf=%.2f parse=%v(consec=%d) noTool=%v lowConf=%v\n",
		req.TaskID, req.StepID, turn+1, maxTurns, len(resp.ToolCalls), len(st.trace),
		output.Status, output.Confidence, parseFailed, st.consecutiveParseFailures, noToolCallsYet, selfReportedLowConfidence)
	for _, tc := range resp.ToolCalls {
		fmt.Fprintf(os.Stderr, "[EVAL-OBS]   -> %s %v\n", tc.Name, tc.Arguments)
	}
	if strings.TrimSpace(cleanText) != "" {
		txt := strings.TrimSpace(cleanText)
		if len(txt) > 200 {
			txt = txt[:200]
		}
		fmt.Fprintf(os.Stderr, "[EVAL-OBS]   text: %s\n", strings.ReplaceAll(txt, "\n", " | "))
	}
}

// noToolNudge reports whether a model that has not called a tool yet was nudged
// to act (the caller then skips the rest of the turn). Extracted from
// completeTurn for the size gate.
//
// A7 (eval FINDINGS.md §A7, 2026-10-22): DisableForceTool is NOT consulted
// here — it disables the provider-side forced-tool constraint, a different
// mechanism, so turning that off must not silence this prompt-side reminder.
func (s *Spawner) noToolNudge(
	req *SpawnRequest, turn, maxTurns int, st *spawnTurnState, output schemas.SubagentOutput,
	parseFailed, noToolCallsYet, selfReportedLowConfidence bool, cleanText string,
) bool {
	if !(turn < maxTurns-1 && noToolCallsYet && (selfReportedLowConfidence || (turn == 0 && output.Status != schemas.StatusOK))) {
		return false
	}
	if s.registry.System.FeatureFlags.EvalObs {
		fmt.Fprintf(os.Stderr, "[EVAL-OBS] NUDGE triggered turn=%d noTool=%v lowConf=%v parse=%v\n",
			turn+1, noToolCallsYet, selfReportedLowConfidence, parseFailed)
	}
	msg := SpawnerNoToolsMsg
	if output.Status == schemas.StatusCapabilityRequired && len(output.RequiredCapabilities) > 0 {
		msg = fmt.Sprintf("%s Before finalizing capability_required, double-check your bound tools. If none fulfill %v, explain and finalize.", MarkerSystem, output.RequiredCapabilities)
	}
	// P1 fix (2026-10-03, eval §0.5): a parse-failure spin gets format
	// guidance too, so the model fixes BOTH "call tools" and "valid JSON".
	if parseFailed {
		msg = SpawnerNoToolsBadJSONMsg
	}
	for i := range st.userBlocks {
		st.userBlocks[i].CacheControl = ""
	}
	st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: cleanText})
	st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: msg, CacheControl: "ephemeral"})
	return true
}
