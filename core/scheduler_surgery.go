package core

import (
	"fmt"
	"github.com/daybeam/vortex/schemas"
	"time"
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
	s.logger.Log(EventStepDeprecated, taskID, data.DeprecatedStepID, map[string]any{
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

	// 1c. Dependency Cleanup (2026-09-07):
	// When a step is deprecated, release all downstream steps that depend on it
	// so they can proceed independently. This is the key mechanism that allows
	// fallback nodes (e.g. s2) to activate after primary path (s1) fails.
	for _, step := range graph.Steps {
		if step.ID == data.DeprecatedStepID {
			continue
		}
		// Remove deprecated step from downstream step's DependsOn
		for i, dep := range step.DependsOn {
			if dep == data.DeprecatedStepID {
				step.DependsOn = append(step.DependsOn[:i], step.DependsOn[i+1:]...)
				break
			}
		}
		// If a pending step loses its last blocker, it may now be ready
		if step.Status == schemas.StepPending {
			if len(step.DependsOn) == 0 {
				// Check if all remaining deps are satisfied
				allReady := true
				for _, dep := range step.DependsOn {
					depStep := graph.Steps[dep]
					if depStep == nil || (depStep.Status != schemas.StepOK && depStep.Status != schemas.StepSkipped && depStep.Status != schemas.StepDeprecated) {
						allReady = false
						break
					}
				}
				if allReady {
					s.logger.Log("dep_cleanup_unblocked", taskID, step.ID, map[string]any{
						"removed_dep": data.DeprecatedStepID,
					})
				}
			}
		}
	}

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

		newStep := &schemas.Step{
			ID:               inp.ID,
			RoleID:           inp.RoleID,
			Task:             inp.Task + inheritance, // Handover heritage
			DependsOn:        inp.DependsOn,
			Status:           schemas.StepPending,
			AdditionalSkills: inp.AdditionalSkills,
			AdditionalMCPs:   inp.AdditionalMCPs,
			ContextRefs:      inp.ContextRefs,
			ExitCriteria:     inp.ExitCriteria,
			VerifierModel:    inp.VerifierModel,
			MaxAutoRefine:    inp.MaxAutoRefine,
			EnableDebate:     inp.EnableDebate,
			MaxDebateRounds:  inp.MaxDebateRounds,
			SpawnDepth:       inp.SpawnDepth,
			MaxSpawnDepth:    inp.MaxSpawnDepth,
			LastWorkedOn:     time.Now(),
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

		graph.Steps[inp.ID] = newStep

		// 3b. Context Tree Alignment (ADDED 2026-09-09)
		// Ensure the new steps belong to the current execution node
		if currNode != nil {
			currNode.StepIDs = append(currNode.StepIDs, inp.ID)
		}

		// Ensure the new step appears in the execution flow
		s.logger.Log(EventStepInjected, taskID, inp.ID, map[string]any{
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
