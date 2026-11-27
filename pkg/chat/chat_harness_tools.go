package chat

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// toolCallOutcome distinguishes calls handled by a pre-execution guard from
// calls that produced a normal tool result, and carries post-call escalation.
type toolCallOutcome struct {
	rule    toolAfterRule
	skipped bool
	toolsOK bool
}

// executeToolCall preserves the per-call order: announce, drift guard, cache,
// pre-execution guards, execute, sieve/output processing, last-result steers,
// then emit and close the transcript entry.
func (r *chatRun) executeToolCall(i int, calls []schemas.ToolCall) toolCallOutcome {
	h := r.h
	tc := calls[i]
	r.stats.recordTool(tc.Name)
	r.emit(ChatEvent{Type: "tool_call", Data: tc.Name, Meta: map[string]any{"args": tc.Arguments, "session_task_id": r.taskID}})

	h.checkPlanDrift(i, r.taskID, tc, r.emit)

	if h.Sieve != nil {
		if cached, found := h.Sieve.GetCachedResult(r.taskID, tc.Name, tc.Arguments); found {
			log.Printf("[chat-harness] cache_replay HIT task=%s tool=%s", r.taskID, tc.Name)
			result := cached + "\n\n[CACHED REPLAY — same query as before; no database changes since last call.]"
			r.emit(ChatEvent{Type: "cache_replay", Data: tc.Name})
			r.emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": tc.Name, "cached": true}})
			r.messages = append(r.messages, ChatMessage{Role: "tool", Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, result)})
			return toolCallOutcome{skipped: true}
		}
	}

	if h.beforeToolCall(tc, r.taskID, r.emit, &r.messages) {
		return toolCallOutcome{skipped: true}
	}

	var rule toolAfterRule
	toolsOK := false
	result, derr := h.execTool(r.ctx, tc.Name, tc.Arguments, r.taskID)
	if derr != nil {
		errMsg := derr.Error()
		if runes := []rune(errMsg); len(runes) > 200 {
			errMsg = string(runes[:200]) + "..."
		}
		r.toolFailErrors[tc.Name] = append(r.toolFailErrors[tc.Name], errMsg)
		if len(r.toolFailErrors[tc.Name]) > 5 {
			r.toolFailErrors[tc.Name] = r.toolFailErrors[tc.Name][len(r.toolFailErrors[tc.Name])-5:]
		}
		failCount := len(r.toolFailErrors[tc.Name])
		r.emit(ChatEvent{Type: "tool_error", Data: derr.Error(), Meta: map[string]any{"tool": tc.Name, "fail_count": failCount}})
		if failCount >= 3 {
			result = fmt.Sprintf("[TOOL %s HAS FAILED %d TIMES] Recent errors:\n%s\nDo not repeat the same approach. Try a different strategy or answer directly.", tc.Name, failCount, strings.Join(r.toolFailErrors[tc.Name], "\n"))
		} else {
			result = fmt.Sprintf(core.ToolFailOnceFmt, tc.Name, derr.Error())
		}
	} else {
		r.toolFailErrors[tc.Name] = nil
		// Judge the raw result before any steer or externalization rewrites it.
		if strings.TrimSpace(result) != "" {
			toolsOK = true
		}
		if h.Sieve != nil {
			switch toolResultPolicyFor(tc.Name, tc.Arguments) {
			case alwaysFreshResult:
				// Pollers execute fresh and neither read nor invalidate the cache.
			case actionResult:
				h.Sieve.ClearResultCache(r.taskID)
				result = "[ACTION COMPLETED] " + result
			default:
				h.Sieve.CacheToolResult(r.taskID, tc.Name, tc.Arguments, result)
				if h.Sieve.CheckResultRepetition(r.taskID, result) {
					result = "[RESULT REPETITION] You are receiving the same result from different queries. The data is not changing. Stop re-querying and provide a direct answer based on the information you already have.\n\n" + result
					r.emit(ChatEvent{Type: "result_repetition", Data: tc.Name})
				}
			}
		}

		if h.shouldExternalize(tc.Name, result) {
			result = h.externalizeArtifact(result, tc.Name, r.taskID)
		}
		rule = h.afterToolCall(tc, &result, &r.eg, r.execGuardMode, r.execStreakLimit, r.declaredArtifacts, r.emit)
	}

	if result != "" {
		var metadata map[string]any
		if json.Unmarshal([]byte(result), &metadata) == nil {
			r.lastToolMeta = core.ExtractMeta(metadata)
		}
	}

	if i == len(calls)-1 {
		r.appendLastToolSteers(&result)
	}

	r.emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": tc.Name}})
	r.messages = append(r.messages, ChatMessage{Role: "tool", Content: fmt.Sprintf("[tool %s result]\n%s", tc.Name, result)})
	return toolCallOutcome{rule: rule, toolsOK: toolsOK}
}

// appendLastToolSteers keeps the two correction channels in their established
// slot and order: a concurrent plan steer first, then the completion contract.
func (r *chatRun) appendLastToolSteers(result *string) {
	h := r.h
	h.SteerLock.Lock()
	if h.PendingSteer != "" {
		*result += "\n\n" + h.PendingSteer
		h.PendingSteer = ""
	}
	h.SteerLock.Unlock()

	if !h.contractArmed() || h.SessionRoot == "" {
		return
	}
	_, missing := core.WorkspaceArtifactsComplete(h.SessionRoot, r.declaredArtifacts)
	defects := h.artifactDefects(r.ctx, r.taskID, r.emit)
	if r.contractSteers == 0 && len(r.declaredArtifacts) > 0 && missing == 0 && len(defects) == 0 && (h.DelegateStatus == nil || h.isDelegatedTaskTerminal(r.taskID)) {
		r.contractSteers++
		r.emit(ChatEvent{Type: EvArtifactsComplete, Meta: map[string]any{
			"declared": len(r.declaredArtifacts),
			"mode":     r.completionContract.String(),
		}})
		if marker := core.CompletionContractMarker(r.completionContract); marker != "" {
			*result += "\n\n" + marker
		}
	} else if len(defects) > 0 && r.contractDefectSteers < maxContractDefectSteers {
		r.contractDefectSteers++
		r.emit(ChatEvent{Type: EvArtifactsIncomplete, Meta: map[string]any{
			"declared": len(r.declaredArtifacts),
			"missing":  missing,
			"defects":  len(defects),
			"mode":     r.completionContract.String(),
		}})
		if marker := core.ContractDefectsMarker(defects, h.contract.lastExtra); marker != "" {
			*result += "\n\n" + marker
		}
	}
}
