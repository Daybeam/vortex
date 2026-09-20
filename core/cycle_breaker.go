package core

import (
	"fmt"
	"sort"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// makeCycleID produces a deterministic signature for a local loop by sorting
// and joining the triggering step ID with its static DependsOn list.
// Example: step "s3" depends on ["s1","s2"] → "s1:s2:s3".
func makeCycleID(triggerStepID string, dependsOn []string) string {
	all := append([]string{triggerStepID}, dependsOn...)
	sort.Strings(all)
	return strings.Join(all, ":")
}

// tryBacktrack implements the Cycle Breaker (docs/architecture/CYCLE_BREAKER_DESIGN.md).
// It is invoked from handleOutput when a downstream step reports MissingContext.
//
// Returns true if the cycle breaker handled the situation (caller should return
// immediately — no further ASAE/fallback/block processing). Returns false when
// backtracking is not applicable (no MissingContext, no DependsOn, or disabled)
// and the caller should continue with the existing failure-handling flow.
//
// Behavior:
//   - Under the fuse limit: reset upstream steps to Pending with feedback
//     injected via AdditionalPromptContext, reset the triggering step to Pending.
//     The DAG walker re-executes upstream, then downstream.
//   - At the fuse limit: suspend all involved steps (StepSuspended), block the
//     graph, and create a human decision with accept_degraded/abort/provide_context.
func (s *DirectedEngine) tryBacktrack(graph *schemas.TaskGraph, step *schemas.Step, output *schemas.SubagentOutput) bool {
	if len(output.MissingContext) == 0 || len(step.DependsOn) == 0 || step.MaxLoopRounds == 0 {
		return false
	}

	cycleID := makeCycleID(step.ID, step.DependsOn)

	s.Mu.Lock()

	if graph.CycleCounters == nil {
		graph.CycleCounters = make(map[string]int)
	}
	graph.CycleCounters[cycleID]++
	currentRound := graph.CycleCounters[cycleID]

	if currentRound <= step.MaxLoopRounds {
		feedback := fmt.Sprintf(
			"[SYSTEM FEEDBACK]\n"+
				"下游步骤 %s 报告以下信息缺失，请在本轮输出中补充：\n%s\n\n"+
				"上一轮你的输出被下游判定为不足。请尝试不同的数据源或分析角度。\n"+
				"本轮是第 %d/%d 轮，如仍无法满足，系统将强制降级。",
			step.ID, strings.Join(output.MissingContext, "\n"), currentRound, step.MaxLoopRounds,
		)

		for _, depID := range step.DependsOn {
			dep, ok := graph.Steps[depID]
			if !ok {
				continue
			}
			dep.Status = schemas.StepPending
			dep.AdditionalPromptContext = feedback
			dep.LastError = ""
		}

		step.Status = schemas.StepPending
		step.MissingCtx = nil
		step.LastError = ""

		s.logger.Log("EventCycleBacktrack", graph.TaskID, step.ID, map[string]any{
			"cycle_id":      cycleID,
			"current_round": currentRound,
			"max_rounds":    step.MaxLoopRounds,
			"upstream":      step.DependsOn,
		})
		s.Mu.Unlock()
		return true
	}

	// Fuse: max rounds exceeded — suspend all involved steps.
	for _, depID := range step.DependsOn {
		if dep, ok := graph.Steps[depID]; ok {
			dep.Status = schemas.StepSuspended
		}
	}
	step.Status = schemas.StepSuspended
	step.AdditionalPromptContext = fmt.Sprintf(
		"[SYSTEM DEGRADE] 已达到最大补充轮次(%d轮)，请根据现有信息尽力生成最终输出。",
		step.MaxLoopRounds,
	)
	delete(graph.CycleCounters, cycleID)

	s.logger.Log("EventCycleFuse", graph.TaskID, step.ID, map[string]any{
		"cycle_id":   cycleID,
		"max_rounds": step.MaxLoopRounds,
	})
	s.publishEvent(graph.TaskID, step.ID, EventStepFailed, map[string]any{"cycle_fuse": cycleID, "suspended": true})
	s.Mu.Unlock()

	// addDecision calls broadcastDone which takes s.Mu.Lock — must NOT
	// be called while holding the lock (sync.RWMutex is not reentrant).
	s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"root_cause":      "cycle_fuse",
		"cycle_id":        cycleID,
		"max_rounds":      step.MaxLoopRounds,
		"missing_context": output.MissingContext,
	}, []string{"abort", "accept_degraded", "provide_context"})

	return true
}
