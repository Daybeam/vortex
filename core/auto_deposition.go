package core

import (
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// AutoDepositResult automatically deposits a step's output into the task graph's
// GlobalWorkspace when the step succeeds without an explicit MergeStrategy/TargetKey.
//
// This relieves the Main Agent from manually configuring TargetKey/MergeStrategy for
// simple tasks — the result appears in wait_task's return value under
// global_workspace[stepID] and global_workspace["result"].
//
// Design constraints:
//   - Must NOT overwrite keys set by the explicit EvoX Programmatic Merge path
//     (which runs earlier and uses step.TargetKey + step.MergeStrategy).
//   - Must NOT clobber an existing "result" key (e.g. set by a prior step's
//     explicit MergeStrategy with TargetKey="result").
//   - Thread-safety: callers MUST hold s.Mu.Lock() when invoking this function,
//     consistent with the EvoX merge block above it in scheduler.go.
func AutoDepositResult(graph *schemas.TaskGraph, stepID string, result map[string]any) {
	if graph == nil || stepID == "" || result == nil {
		return
	}
	if graph.GlobalWorkspace == nil {
		graph.GlobalWorkspace = make(map[string]any)
	}

	// Deposit under step ID (primary namespace — one result per step).
	if _, exists := graph.GlobalWorkspace[stepID]; !exists {
		graph.GlobalWorkspace[stepID] = result
	}

	// Deposit under "result" (convenience key for single-step tasks).
	// For multi-step DAGs, the last successful step's result wins the "result" key.
	// This is intentional: the Main Agent typically wants the final deliverable.
	if _, exists := graph.GlobalWorkspace["result"]; !exists {
		graph.GlobalWorkspace["result"] = result
	}
}

// AutoDepositResultSafe is a logging wrapper that never panics.
// Useful for deferred/recover patterns or goroutines.
func AutoDepositResultSafe(graph *schemas.TaskGraph, stepID string, result map[string]any, logger *Logger) {
	defer func() {
		if r := recover(); r != nil {
			if logger != nil {
				logger.Log("EventAutoDepositPanic", "", stepID, map[string]any{
					"error": fmt.Sprintf("%v", r),
				})
			}
		}
	}()
	AutoDepositResult(graph, stepID, result)
}
