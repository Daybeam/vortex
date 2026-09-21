package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// collectDelegationToolDefs gathers all tool definitions (core + MCP-bound)
// for inclusion in a delegation-mode return payload.
// This fixes the Tool Vacuum defect (DELEGATION_AND_SUBMIT_DEFECTS.md §1.1):
// previously the delegation return only included system_prompt + user_prompt,
// leaving the external gateway with an empty tools list.
func (s *Spawner) collectDelegationToolDefs(hub *ContextHub, bindings []config.MCPBinding) []schemas.ToolDefinition {
	var defs []schemas.ToolDefinition

	defs = append(defs, CoreToolDefinitions()...)

	for _, b := range bindings {
		mcp := hub.GetMCP(b.MCPID)
		if mcp == nil {
			continue
		}
		if b.IsRestricted() {
			allowed := make(map[string]bool)
			for _, t := range b.AllowedTools {
				allowed[t] = true
			}
			for _, td := range mcp.FullToolDefinitions {
				if allowed[td.Name] {
					defs = append(defs, td)
				}
			}
		} else {
			defs = append(defs, mcp.FullToolDefinitions...)
		}
	}

	return defs
}

// redactContextForDelegation strips sensitive internal context (MemoryBank,
// precedents, upstream results) from the merged context before it is passed
// to buildSystemPrompt in delegation mode.
// This fixes the Context Privacy defect (DELEGATION_AND_SUBMIT_DEFECTS.md §1.3):
// previously the full internal context was leaked to the external gateway.
func redactContextForDelegation(mergedContext map[string]any) map[string]any {
	redacted := make(map[string]any)

	for k, v := range mergedContext {
		switch k {
		case "upstream":
			redacted[k] = "[REDACTED: upstream results not shared in delegation mode]"
		case "path":
			redacted[k] = "[REDACTED: context tree paths not shared in delegation mode]"
		case "env":
			if envMap, ok := v.(map[string]any); ok {
				safeEnv := make(map[string]any)
				for ek, ev := range envMap {
					if isSafeEnvKey(ek) {
						safeEnv[ek] = ev
					}
				}
				redacted[k] = safeEnv
			} else {
				redacted[k] = "[REDACTED]"
			}
		default:
			redacted[k] = v
		}
	}

	return redacted
}

func isSafeEnvKey(key string) bool {
	lower := strings.ToLower(key)
	safeKeys := []string{"os", "arch", "cwd", "time", "date", "locale"}
	for _, s := range safeKeys {
		if lower == s || strings.HasPrefix(lower, s+"_") {
			return true
		}
	}
	return false
}

// FulfillDelegation allows an external gateway to submit the result of a
// delegated step, unblocking the DAG and resuming execution.
// This fixes the Async State Machine Dead-End defect
// (DELEGATION_AND_SUBMIT_DEFECTS.md §1.2): previously there was no built-in
// callback mechanism for external gateways to return results.
func (s *DirectedEngine) FulfillDelegation(taskID, stepID string, result map[string]any) error {
	s.Mu.Lock()
	graph, ok := s.graphs[taskID]
	if !ok {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}
	step, ok := graph.Steps[stepID]
	if !ok {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found in task %q", stepID, taskID)
	}

	if step.Status != schemas.StepBlocked {
		s.Mu.Unlock()
		return fmt.Errorf("step %q is not blocked (current status: %s), cannot fulfill delegation", stepID, step.Status)
	}

	var hasDelegationDecision bool
	for _, d := range graph.PendingDecisions {
		if d.StepID == stepID && d.Type == schemas.DecisionDelegationRequired {
			hasDelegationDecision = true
			break
		}
	}
	if !hasDelegationDecision {
		s.Mu.Unlock()
		return fmt.Errorf("step %q has no pending delegation decision", stepID)
	}

	s.Mu.Unlock()

	res := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 1.0,
			Result:     result,
			Capability: step.RoleID,
		},
		Ref: fmt.Sprintf("%s:%s", taskID, stepID),
	}

	s.handleOutput(graph, step, res)

	s.Mu.Lock()
	graph.PendingDecisions = filterDelegationDecisions(graph.PendingDecisions, stepID)

	if len(graph.PendingDecisions) == 0 && graph.Status == schemas.GraphBlocked {
		if graph.IsSmartRouted {
			graph.Status = schemas.GraphCompleted
			if ch, ok := s.doneChans[taskID]; ok {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
		} else {
			graph.Status = schemas.GraphRunning
			s.doneChans[taskID] = make(chan struct{})
		}
	}
	s.Mu.Unlock()

	s.persistGraph(graph)
	s.Broadcast()

	s.logger.Log("EventDelegationFulfilled", taskID, stepID, map[string]any{
		"result_keys": func() []string {
			keys := make([]string, 0, len(result))
			for k := range result {
				keys = append(keys, k)
			}
			return keys
		}(),
	})

	return nil
}

func filterDelegationDecisions(decisions []*schemas.Decision, stepID string) []*schemas.Decision {
	var filtered []*schemas.Decision
	for _, d := range decisions {
		if d.StepID == stepID && d.Type == schemas.DecisionDelegationRequired {
			continue
		}
		filtered = append(filtered, d)
	}
	return filtered
}
