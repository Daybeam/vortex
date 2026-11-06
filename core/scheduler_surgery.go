package core

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// SurgeryData contains parameters for dynamic topology mutation.
// Mechanism 2.1 of Dynamic DAG Surgery Specification.
type SurgeryData struct {
	DeprecatedStepID string              `json:"deprecated_step_id"`
	ReplacementSteps []schemas.StepInput `json:"replacement_steps"`
}

// MutateGraphTopology performs online topology hot-swap as per Specification 2.2.
// It marks a dead-end node as deprecated and dynamically injects replacement paths.
func (s *DirectedEngine) MutateGraphTopology(taskID string, data SurgeryData) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	graph := s.graphs[taskID]
	if graph == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	return s.mutateGraphTopologyLocked(graph, data)
}

func (s *DirectedEngine) mutateGraphTopologyLocked(graph *schemas.TaskGraph, data SurgeryData) error {
	taskID := graph.TaskID
	depStep := graph.Steps[data.DeprecatedStepID]
	if depStep == nil {
		return fmt.Errorf("deprecated step %q not found", data.DeprecatedStepID)
	}

	// 1. Mark target step as deprecated (Dead-end reference)
	// Specification 2.1: status = deprecated
	depStep.Status = schemas.StepDeprecated
	s.logger.Log(obs.EventStepDeprecated, taskID, data.DeprecatedStepID, map[string]any{
		"last_error": depStep.LastError,
	})

	// 1b. Auto-skip audit steps whose upstream is deprecated (2026-09-07):
	// If an audit step's upstream was just deprecated, skip it
	// immediately to avoid creating unnecessary upstream_insufficient decisions.
	// NOTE: This MUST run before dependency cleanup, as cleanup removes the ID.
	for _, step := range graph.Steps {
		if step.ID == data.DeprecatedStepID {
			continue
		}
		if step.RoleID == "auditor" && step.Status == schemas.StepPending {
			for _, dep := range step.DependsOn {
				if dep == data.DeprecatedStepID {
					step.Status = schemas.StepSkipped
					s.logger.Log("audit_auto_skipped", taskID, step.ID, map[string]any{
						"upstream": data.DeprecatedStepID,
					})
					break
				}
			}
		}
	}

	// 1c. Release every downstream step from the deprecated ID, and revive the
	// ones this surgery un-blocked (see releaseDeprecatedDeps).
	s.releaseDeprecatedDeps(graph, data.DeprecatedStepID)

	// 2. Error Inheritance: Prepare context handover (Specification 2.3)
	// The failure reference is injected into the replacement steps' prompts.
	inheritance := fmt.Sprintf("\n\n[DEPRECATED PATH FAILURE REFERENCE]\nStep %q failed at this dead-end.\nLast Error: %s\nDecision History includes previous attempts. Avoid repeating the same mistakes.",
		data.DeprecatedStepID, depStep.LastError)

	// 3. Inject replacement steps
	currNode := graph.ContextTree[graph.CurrentNodeID]
	for _, inp := range data.ReplacementSteps {
		if _, exists := graph.Steps[inp.ID]; exists {
			return fmt.Errorf("replacement step %q already exists in graph", inp.ID)
		}

		// T5 fix (2026-10-18): scrub references to the deprecated step from
		// the replacement's DependsOn. A deprecated step never reaches
		// StepOK, so a replacement that depends on it would silently
		// deadlock — ReadySteps never returns it. The planner or caller may
		// legitimately include the deprecated ID (e.g. "redo s2"), so we
		// scrub rather than reject, and log when we do.
		scrubbedDeps := make([]string, 0, len(inp.DependsOn))
		var scrubbedRefs []string
		for _, dep := range inp.DependsOn {
			if dep == data.DeprecatedStepID {
				scrubbedRefs = append(scrubbedRefs, dep)
				continue
			}
			scrubbedDeps = append(scrubbedDeps, dep)
		}
		if len(scrubbedRefs) > 0 {
			s.logger.Log("replan_dep_scrubbed", taskID, inp.ID, map[string]any{
				"deprecated_step": data.DeprecatedStepID,
				"removed_refs":    scrubbedRefs,
			})
		}

		newStep := &schemas.Step{
			ID:                       inp.ID,
			RoleID:                   inp.RoleID,
			Task:                     inp.Task + inheritance, // Handover heritage
			DependsOn:                scrubbedDeps,
			Status:                   schemas.StepPending,
			AdditionalSkills:         inp.AdditionalSkills,
			AdditionalMCPs:           inp.AdditionalMCPs,
			AdditionalToolAllowlists: inp.AdditionalToolAllowlists, // T3: preserve tool narrowing across replan
			ContextRefs:              inp.ContextRefs,
			ExitCriteria:             inp.ExitCriteria,
			DeterministicChecks:      inp.DeterministicChecks,
			VerifierModel:            inp.VerifierModel,
			MaxAutoRefine:            inp.MaxAutoRefine,
			EnableDebate:             inp.EnableDebate,
			MaxDebateRounds:          inp.MaxDebateRounds,
			SpawnDepth:               inp.SpawnDepth,
			MaxSpawnDepth:            inp.MaxSpawnDepth,
			LastWorkedOn:             time.Now(),
		}
		if newStep.AdditionalSkills == nil {
			newStep.AdditionalSkills = []string{}
		}
		if newStep.AdditionalMCPs == nil {
			newStep.AdditionalMCPs = []string{}
		}
		if newStep.ContextRefs == nil {
			newStep.ContextRefs = map[string]string{}
		}
		if newStep.AdditionalToolAllowlists == nil {
			newStep.AdditionalToolAllowlists = map[string][]string{}
		}

		graph.Steps[inp.ID] = newStep

		// 3b. Context Tree Alignment (ADDED 2026-09-09)
		// Ensure the new steps belong to the current execution node
		if currNode != nil {
			currNode.StepIDs = append(currNode.StepIDs, inp.ID)
		}

		// Ensure the new step appears in the execution flow
		s.logger.Log(obs.EventStepInjected, taskID, inp.ID, map[string]any{
			"replaces": data.DeprecatedStepID,
			"task":     inp.Task,
		})
	}

	// 4. Update Context Summary
	if currNode != nil {
		currNode.Summary += fmt.Sprintf("\n- Path via step %s abandoned (deprecated). Replacement path injected.", data.DeprecatedStepID)
	}

	return nil
}

// releaseDeprecatedDeps strips the deprecated step from every other step's
// DependsOn and, since the O22 fix, revives the steps that this surgery is the
// only thing still holding blocked. MUST be called with s.Mu held.
//
// The strip has always been here (2026-09-07): it is the mechanism that lets a
// fallback node activate after the primary path fails. What was missing is the
// status transition. maybeBlock (core/scheduler_decision_gate.go:73-79) turns
// every *pending* dependent of a failed step into StepBlocked, and nothing in the
// engine ever moves StepBlocked back — schemas/task.go:308 counts it terminal.
// So a step that was blocked because of the very step now being deprecated stayed
// blocked with an empty DependsOn: ReadySteps only returns pending steps, and the
// graph silently ended `failed` with the whole downstream DAG forfeited even
// though surgery had removed the reason for the block (14 such steps observed
// across the delegation SOP run; O22's measurement is the parallel_hunting →
// adversarial_verification pair).
func (s *DirectedEngine) releaseDeprecatedDeps(graph *schemas.TaskGraph, deprecatedStepID string) {
	for _, step := range graph.Steps {
		if step.ID == deprecatedStepID {
			continue
		}
		// audit L-6.1: remove ALL occurrences, not just the first (was `break`).
		// Duplicate deps would block the step forever since the deprecated step
		// has StepDeprecated status which fails ReadySteps' satisfaction check.
		filtered := step.DependsOn[:0]
		removed := 0
		for _, dep := range step.DependsOn {
			if dep != deprecatedStepID {
				filtered = append(filtered, dep)
			} else {
				removed++
			}
		}
		step.DependsOn = filtered

		switch step.Status {
		case schemas.StepPending:
			if len(step.DependsOn) == 0 {
				// audit C-11: removed dead inner loop (iterated empty slice, allReady always true).
				// No remaining deps = step is trivially unblocked.
				s.logger.Log("dep_cleanup_unblocked", graph.TaskID, step.ID, map[string]any{
					"removed_dep": deprecatedStepID,
				})
			}
		case schemas.StepBlocked:
			// `removed > 0` keeps this scoped to the dependency the surgery just
			// deleted: a step blocked by some other failure must stay blocked,
			// otherwise deprecating one branch would revive unrelated branches.
			if removed > 0 && !hasPendingDecisionFor(graph, step.ID) && depsSatisfiedFor(graph, step) {
				step.Status = schemas.StepPending
				step.LastError = ""
				s.logger.Log("dep_cleanup_unblocked", graph.TaskID, step.ID, map[string]any{
					"removed_dep": deprecatedStepID,
					"from_status": string(schemas.StepBlocked),
				})
			}
		}
	}
}

// hasPendingDecisionFor reports whether an unanswered decision is parked on this
// step. StepBlocked doubles as "awaiting the host" — a delegation hand-over
// (handleDelegationRequired), an exhausted token budget, max-turns suspension, a
// missing role or skill — and reviving such a step would let the run loop execute
// it while its envelope is still outstanding, which is the failure class the
// O-series audit keeps hitting. Blocking the revival on a pending decision keeps
// the resurrection to the steps that are blocked purely by topology.
func hasPendingDecisionFor(graph *schemas.TaskGraph, stepID string) bool {
	for _, d := range graph.PendingDecisions {
		if d.StepID == stepID {
			return true
		}
	}
	return false
}

// depsSatisfiedFor mirrors TaskGraph.ReadySteps' dependency rule
// (schemas/task.go:732-742): OK, Partial and Skipped satisfy a dependency, and a
// dependency that no longer resolves in the graph is skipped rather than held.
// Kept local to this file because the revival is the only caller that needs to ask
// the question about a non-pending step; if ReadySteps' rule ever changes, this is
// the place that has to change with it.
func depsSatisfiedFor(graph *schemas.TaskGraph, step *schemas.Step) bool {
	for _, depID := range step.DependsOn {
		dep, ok := graph.Steps[depID]
		if !ok {
			continue
		}
		if dep.Status != schemas.StepOK && dep.Status != schemas.StepPartial && dep.Status != schemas.StepSkipped {
			return false
		}
	}
	return true
}
