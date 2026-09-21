package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// checkSieveGuardian runs the Sieve loop-detection interceptor on planned
// tool calls. Returns (true, "") if the call plan is valid.
// Returns (false, reason) if the Sieve detects a loop — the caller should
// abort the spawn with a failed output.
// Extracted from spawner.go per SPAWNER_REFACTORING_EXECUTION_PLAN.md Step 2.
func (s *Spawner) checkSieveGuardian(taskID, stepID string, turn int, toolCalls []schemas.ToolCall, trace []store.ToolInteraction) (bool, string) {
	plannedInteractions := make([]store.ToolInteraction, len(toolCalls))
	for i, tc := range toolCalls {
		plannedInteractions[i] = store.ToolInteraction{
			ToolName:  tc.Name,
			Arguments: tc.Arguments,
		}
	}

	valid, reason := s.sieve.InspectToolCalls(taskID, plannedInteractions)
	if !valid {
		var loopDetail []map[string]any
		for _, pi := range plannedInteractions {
			argsJSON, _ := json.Marshal(pi.Arguments)
			entry := map[string]any{
				"tool":      pi.ToolName,
				"arguments": truncateForLog(string(argsJSON), 500),
			}
			for i := len(trace) - 1; i >= 0; i-- {
				if trace[i].ToolName == pi.ToolName {
					entry["prior_result"] = truncateForLog(formatForLog(trace[i].Result), 500)
					break
				}
			}
			loopDetail = append(loopDetail, entry)
		}
		s.logger.Log(EventTaskFailed, taskID, stepID, map[string]any{
			"error":       "Sieve Guardian intercepted",
			"reason":      reason,
			"turn":        turn,
			"loop_detail": loopDetail,
		})
	}
	return valid, reason
}

// handleCoreToolCall executes a core tool (write_file, read_file, execute_code)
// directly without MCP transport. Returns the result string, status, and the
// tool interaction record for the trace.
// Extracted from spawner.go per SPAWNER_REFACTORING_EXECUTION_PLAN.md Step 2.
func (s *Spawner) handleCoreToolCall(ctx context.Context, call schemas.ToolCall, taskID, stepID string, validator *SafePathValidator) (string, string, store.ToolInteraction) {
	result, herr := HandleCoreTool(ctx, call.Name, call.Arguments, s.outputBase, taskID, validator)
	res := result
	status := "success"
	if herr != nil {
		res = fmt.Sprintf("Error: %v", herr)
		status = "error"
	}
	interaction := store.ToolInteraction{
		ToolName:  call.Name,
		Arguments: call.Arguments,
		Result:    res,
	}
	s.logger.Log("EventCoreToolCall", taskID, stepID, map[string]any{
		"tool":   call.Name,
		"status": status,
	})
	return res, status, interaction
}

// ClearSieveHistory clears the Sieve loop-detection history for a task.
// Moved from spawner.go during god-class split.
func (s *Spawner) ClearSieveHistory(taskID string) {
	if s.sieve != nil {
		s.sieve.ClearHistory(taskID)
	}
}
