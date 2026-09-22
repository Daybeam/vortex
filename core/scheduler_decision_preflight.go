package core

import (
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_preflight.go — Phase 1: Failure pre-flight detection.
// Extracted from scheduler_decision.go per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md.
// Contains: detectUpstreamInsufficient, diagnoseFault, applyStepFailurePolicy.

// upstreamInsufficientInfo holds a blocked downstream step plus the
// upstream step IDs and their terminal-but-not-success statuses.
type upstreamInsufficientInfo struct {
	Step             *schemas.Step
	UpstreamIDs      []string
	UpstreamStatuses map[string]schemas.StepStatus
}

// detectUpstreamInsufficient scans the graph for a pending step whose
// upstream dependency is terminal but not successful (failed/blocked).
// Returns nil if no such situation exists (meaning some upstream is
// still running and the graph should wait). ADDED (2026-08-27).
func (s *DirectedEngine) detectUpstreamInsufficient(graph *schemas.TaskGraph) *upstreamInsufficientInfo {
	for stepID, step := range graph.Steps {
		if step.Status != schemas.StepPending {
			continue
		}
		upIDs := []string{}
		upStatuses := map[string]schemas.StepStatus{}
		allUpstreamTerminal := true
		anyUpstreamFailed := false
		for _, depID := range step.DependsOn {
			dep, ok := graph.Steps[depID]
			if !ok {
				// Unknown dependency — treat as still-pending
				allUpstreamTerminal = false
				break
			}
			upIDs = append(upIDs, depID)
			upStatuses[depID] = dep.Status
			if dep.IsTerminal() {
				if dep.Status != schemas.StepOK && dep.Status != schemas.StepPartial {
					anyUpstreamFailed = true
				}
			} else {
				allUpstreamTerminal = false
			}
		}
		if !allUpstreamTerminal {
			continue
		}
		if anyUpstreamFailed {
			_ = stepID
			return &upstreamInsufficientInfo{
				Step:             step,
				UpstreamIDs:      upIDs,
				UpstreamStatuses: upStatuses,
			}
		}
	}
	return nil
}

func (s *DirectedEngine) diagnoseFault(output *schemas.SubagentOutput, step *schemas.Step) (rootCause FailureClass, suggestion string, missingDetails []string) {
	if output == nil {
		return FailureClassTransient, "Execution failed with unknown error.", nil
	}

	if len(output.MissingContext) > 0 {
		return FailureClassContextDeficit, "Prompt the Manager to supply required data or upstream variables.", output.MissingContext
	}
	if output.Status == schemas.StatusCapabilityRequired {
		return FailureClassCapabilityRequired, "Instruct Manager to mount the required capability via additional_mcps.", output.RequiredCapabilities
	}

	conf := 0.0
	if step.Confidence != nil {
		conf = *step.Confidence
	} else {
		conf = output.Confidence
	}

	if conf < 0.6 {
		return FailureClassGenerativeUncertainty, "Trigger Auto-Refine with stricter constraints or human review.", nil
	}

	// Default to execution failure
	return "execution_failed", "Escalate model tier or abort/skip.", nil
}

// applyStepFailurePolicy honors a step's declared FailurePolicy.OnFailure
// override for a specific failure class, if one is set, as an alternative
// to the default decision_required behavior. Returns true if a policy was
// applied (in which case the caller should return immediately without also
// calling addDecision), false if there is no policy to apply (nil
// FailurePolicy, or an OnFailure value this function doesn't recognize --
// in either case, the caller should fall through to its existing
// decision_required call, exactly as it did before this field existed).
//
// Semantics deliberately mirror SubmitDecision's own "skip"/"abort" cases
// exactly (see the switch in SubmitDecision above), so a step opting into
// FailurePolicy behaves identically to a caller that would have received
// the decision_required block and immediately submitted that same choice.
func (s *DirectedEngine) applyStepFailurePolicy(graph *schemas.TaskGraph, step *schemas.Step, taskID string, failureClass FailureClass) bool {
	if step.FailurePolicy == nil {
		return false
	}
	switch step.FailurePolicy.OnFailure {
	case "skip":
		step.Status = schemas.StepSkipped
	case "abort":
		s.setGraphStatus(graph, schemas.GraphFailed)
	default:
		return false
	}
	s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
		"error":         "step-level FailurePolicy applied, bypassing decision_required",
		"failure_class": string(failureClass),
		"on_failure":    step.FailurePolicy.OnFailure,
	})
	return true
}
