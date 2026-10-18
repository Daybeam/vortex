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

// buildPreviousSummary extracts the upstream step's previous output summary
// for delta-backtrack injection. Returns "(no previous output)" if the step
// has never been executed or no archive is available.
//
// See docs/plan/DELTA_BACKTRACK_DESIGN.md §change 3.
func (s *DirectedEngine) buildPreviousSummary(dep *schemas.Step, taskID string) string {
	// Source 1: ResultRef — direct output reference via archive search
	if dep.ResultRef != "" && s.Archive != nil {
		items := s.Archive.SearchAllStepSummaries(taskID, dep.ResultRef, 1)
		if len(items) > 0 && items[0].Summary != "" {
			return items[0].Summary
		}
	}

	// Source 2: Archive step summaries (by step ID, most recent first)
	if s.Archive != nil {
		summaries := s.Archive.SearchRecentStepSummaries(taskID, 5)
		for _, sm := range summaries {
			for _, sid := range sm.StepIDs {
				if sid == dep.ID && sm.Summary != "" {
					return sm.Summary
				}
			}
		}
		// audit L-17-1: do NOT fall back to summaries[0] from a different step.
		// Returning a foreign step's output as this step's "previous output"
		// misleads the model into skipping necessary work or producing wrong
		// supplements. Fall through to Source 3 / default instead. See
		// AUDIT_REPORT_2026-10-17.md §L-17-1.
	}

	// Source 3: TargetFile — if artifacts exist on disk
	if dep.TargetFile != "" {
		return fmt.Sprintf("(artifact exists at %s — read it for previous output)", dep.TargetFile)
	}

	return "(no previous output available — first execution)"
}

// tryBacktrack implements the Cycle Breaker (docs/architecture/CYCLE_BREAKER_DESIGN.md).
// It is invoked from handleOutput when a downstream step reports MissingContext.
//
// Returns true if the cycle breaker handled the situation (caller should return
// immediately — no further ASAE/fallback/block processing). Returns false when
// backtracking is not applicable (no MissingContext, no DependsOn, or disabled)
// and the caller should continue with the existing failure-handling flow.
//
// Behavior (Delta Backtrack mode, 2026-10-17):
//   - Under the fuse limit: build a previous-output summary from the upstream
//     step's ResultRef/archive, inject it as PreviousOutputSummary + structured
//     feedback, set the DeltaBacktrack metadata flag, reset upstream to Pending.
//     The DAG walker re-executes upstream with memory of what it already produced,
//     so it supplements rather than re-runs. See docs/plan/DELTA_BACKTRACK_DESIGN.md.
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
		hasPrevious := false

		for _, depID := range step.DependsOn {
			dep, ok := graph.Steps[depID]
			if !ok {
				continue
			}

			// Delta Backtrack: build previous output summary for injection.
			previousSummary := s.buildPreviousSummary(dep, graph.TaskID)
			if previousSummary != "" && !strings.HasPrefix(previousSummary, "(no previous") {
				hasPrevious = true
			}

			// Structured feedback with previous context and supplement-only instruction.
			feedback := fmt.Sprintf(
				"%s\n"+
					"Previous-round output summary of upstream step %s:\n%s\n\n"+
					"Downstream step %s reports the following missing information:\n%s\n\n"+
					"Please supplement only the missing parts while preserving all completed content from the previous round.\n"+
					"Do NOT re-execute already-completed steps. Do NOT re-call already-completed external APIs or re-perform already-completed write operations.\n"+
					"This is supplement round %d/%d; if still unsatisfied, the system will force a degradation.",
				MarkerSystemFeedback,
				depID,
				previousSummary,
				step.ID,
				strings.Join(output.MissingContext, "\n"),
				currentRound,
				step.MaxLoopRounds,
			)

			// Set to Pending (required for DAG walker to re-execute)
			// but preserve previous output summary for prompt injection.
			dep.Status = schemas.StepPending
			dep.AdditionalPromptContext = feedback
			dep.PreviousOutputSummary = previousSummary
			dep.LastError = ""

			// Mark as delta-backtrack so prompt_assembler injects PreviousOutputSummary.
			if dep.Metadata == nil {
				dep.Metadata = &schemas.Metadata{}
			}
			dep.Metadata.DeltaBacktrack = true
		}

		step.Status = schemas.StepPending
		step.MissingCtx = nil
		step.LastError = ""

		s.logger.Log("EventDeltaBacktrack", graph.TaskID, step.ID, map[string]any{
			"cycle_id":      cycleID,
			"current_round": currentRound,
			"max_rounds":    step.MaxLoopRounds,
			"upstream":      step.DependsOn,
			"has_previous":  hasPrevious,
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
		"%s has reached the maximum supplement rounds (%d). Please generate the best possible final output from the available information.",
		MarkerSystemDegrade, step.MaxLoopRounds,
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
