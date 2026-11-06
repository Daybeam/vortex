package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_answer.go — Applying a host's answer to a parked decision.
// Extracted from scheduler_status.go on 2026-11-04 during the O25 fix, which
// pushed that file past the 1000-line file limit: the whole decision-answer
// cluster moved here instead of adding lines to a frozen-violating file.
// Contains: SubmitDecision, SubmitDecisionWithPayload, applyDecisionChoiceLocked,
// applyCreateRoleChoiceLocked.

func (s *DirectedEngine) SubmitDecision(taskID, decisionID, choice string) error {
	return s.SubmitDecisionWithPayload(taskID, decisionID, choice, "")
}

func (s *DirectedEngine) SubmitDecisionWithPayload(taskID, decisionID, choice, payload string) error {
	s.Mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			s.Mu.Unlock()
		}
	}()

	graph := s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		return fmt.Errorf("task %q not found", taskID)
	}

	if choice == "rewrite_dag" {
		var data SurgeryData
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			return fmt.Errorf("invalid surgery data for rewrite_dag: %v", err)
		}
		if err := s.mutateGraphTopologyLocked(graph, data); err != nil {
			return err
		}
	}

	if choice == "goal_change_requested" {
		if err := s.applyGoalChangeLocked(graph, taskID, payload); err != nil {
			return err
		}
	}

	var dec *schemas.Decision
	for _, d := range graph.PendingDecisions {
		if d.ID == decisionID {
			dec = d
			break
		}
	}
	if dec == nil {
		return fmt.Errorf("decision %q not found", decisionID)
	}

	step := graph.Steps[dec.StepID]
	if step == nil {
		return fmt.Errorf("step %q not found", dec.StepID)
	}

	// ── create_role (RESTORED 2026-10-18) ──────────────────────────────
	//
	// Handled BEFORE the generic choice switch because it needs the LLM,
	// the cookbook loader and the two-phase draft/apply split -- none of
	// which fit the "mutate step flags" shape of the cases below.
	//
	// What it does NOT do (and why the old handler was removed):
	//   - never sets registry.EnableDynamicRoleGen
	//   - never calls GenerateRole (which CommitOps-persists globally)
	//   - never writes into workspace/skills/
	// So the original objection ("flips a global switch and can persist
	// LLM-authored roles with AllowDynamicMCPs:true") no longer applies:
	// this path is task-scoped by construction.
	//
	// Two-phase contract (docs 52 §11.5, "auto-drafting is fine, auto-
	// activation is not"): the generated role lands in graph.SessionRoles
	// only, i.e. inside outputs/<taskID>/manifest.json. Promotion to the
	// global registry is a separate, human-initiated act via
	// orchestrator_invoke(subsystem="config", action="register_role").
	if choice == "create_role" {
		if err := s.applyCreateRoleChoiceLocked(taskID, decisionID, graph, step, dec, payload); err != nil {
			return err
		}
	}

	// #15: Audit hash chain — record decision submission (tamper-evident).
	if s.auditLogger != nil {
		s.auditLogger.Log("submitDecision", taskID, dec.StepID, decisionID, string(dec.Type), choice, dec.Options)
	}

	// The choice switch was moved to applyDecisionChoiceLocked on 2026-11-04 so
	// that the O25 file split did not push this function past its frozen size
	// baseline. Early returns keep their original meaning: an unapplied choice
	// never reaches the pending-decision removal below.
	if err := s.applyDecisionChoiceLocked(taskID, graph, step, dec, choice, payload); err != nil {
		return err
	}

	// Track consecutive skips for cascade guard (filterSkipOptions).
	if choice == "skip" {
		graph.ConsecutiveSkips++
	} else {
		graph.ConsecutiveSkips = 0
	}

	// Remove decision from pending
	filtered := graph.PendingDecisions[:0]
	for _, d := range graph.PendingDecisions {
		if d.ID != decisionID {
			filtered = append(filtered, d)
		}
	}
	graph.PendingDecisions = filtered

	// Resume if no more pending decisions
	if len(graph.PendingDecisions) == 0 {
		if s.finalizeSmartRoutedAfterDecision(graph, taskID, dec.StepID, &unlocked) {
			return nil
		}
		s.doneChans[taskID] = make(chan struct{})
	}

	// O25 (2026-11-04): checkpoint the answer itself. Until now the choices above
	// only mutated the in-memory graph, so a process that rehydrates a task,
	// answers a decision and exits acknowledged the answer and lost it — an
	// `abort` that returned ok left the durable task row and manifest saying
	// `running` (14 such rows at the time of measurement). Inside a live engine
	// the run loop's next checkpoint hides this; CancelTask already persists,
	// which is why cancel settled and abort did not.
	//
	// Release the lock first: persistGraph takes RLock and sync.RWMutex is not
	// re-entrant. The finalizeSmartRoutedAfterDecision branch above already
	// persisted, so reaching here means the lock is still held.
	s.Mu.Unlock()
	unlocked = true
	s.persistGraph(graph)

	s.logger.Log(obs.EventDecisionSubmitted, taskID, dec.StepID, map[string]any{
		"decision_id": decisionID,
		"choice":      choice,
	})
	// Wake the run() loop to check for newly-ready steps after decision resolution.
	s.Broadcast()
	return nil
}

// applyDecisionChoiceLocked mutates the step and graph for one answer choice.
// rewrite_dag is applied by the caller before it gets here, so it stays a
// no-op case rather than a second mutation. Single call site, hence exempt
// from the 150-line function limit.
func (s *DirectedEngine) applyDecisionChoiceLocked(
	taskID string, graph *schemas.TaskGraph, step *schemas.Step, dec *schemas.Decision, choice, payload string,
) error {
	if choice != "rewrite_dag" {
		switch choice {
		case "skip":
			step.Status = schemas.StepSkipped
		case "abort":
			graph.Status = schemas.GraphFailed
		case "retry":
			step.Status = schemas.StepPending
			step.RetryCount = 0
			step.LastError = ""
			// Delta Backtrack cleanup (ADDED 2026-10-17): clear stale
			// delta-backtrack context so the user-initiated retry starts
			// fresh without previous-output injection or cycle-breaker
			// feedback bleeding in. Also clear deps that may have been
			// left in delta-backtrack mode by the cycle breaker.
			// See docs/plan/DELTA_BACKTRACK_DESIGN.md §change 3.
			step.PreviousOutputSummary = ""
			step.AdditionalPromptContext = ""
			if step.Metadata != nil {
				step.Metadata.DeltaBacktrack = false
			}
			for _, depID := range step.DependsOn {
				if dep, ok := graph.Steps[depID]; ok {
					dep.PreviousOutputSummary = ""
					dep.AdditionalPromptContext = ""
					if dep.Metadata != nil {
						dep.Metadata.DeltaBacktrack = false
					}
				}
			}
		// ADDED (2026-09-07): resume a step suspended at maxTurns by granting it
		// another chunk of tool-turn budget, then re-running it. Deliberately does
		// NOT clear TurnsBudgetBonus, so successive resumes accumulate room instead
		// of restarting from the same capped loop and hitting the wall again.
		case "resume_more_turns":
			chunk := 50
			if s.registry != nil && s.registry.System.MaxToolTurns > 0 {
				chunk = s.registry.System.MaxToolTurns
			}
			step.TurnsBudgetBonus += chunk
			// audit H3: cap the accumulated bonus so a stuck step that is
			// repeatedly resumed cannot grow its turn budget without bound
			// (which would bypass BudgetGuard and burn unbounded tokens).
			// Allow up to 4× the base budget in accumulated bonus, so total
			// maxTurns is bounded at ~5× base. Beyond that the operator must
			// pick a non-resume decision (rewrite_dag/abort/refine).
			if cap := chunk * 4; step.TurnsBudgetBonus > cap {
				step.TurnsBudgetBonus = cap
			}
			step.Status = schemas.StepPending
			step.RetryCount = 0
			step.LastError = ""
		case "refine_and_retry":
			step.Status = schemas.StepPending
			step.RetryCount = 0
			// Delta Backtrack cleanup: clear stale previous-output injection.
			// User feedback (set below) replaces the cycle-breaker's feedback.
			step.PreviousOutputSummary = ""
			if step.Metadata != nil {
				step.Metadata.DeltaBacktrack = false
			}
			if dec.Context != nil {
				if feedback, ok := dec.Context["feedback"].(string); ok {
					step.AdditionalPromptContext = feedback
				}
			}
		case "approve":
			s.applyApproveChoice(graph, step, dec)
		case "reject":
			step.Status = schemas.StepFailed
			step.LastError = "human_rejected"
		case "confirm_abort":
			// Cost governance (ADDED 2026-09-14): user confirmed Main Agent's
			// autonomous abort request. Cancel the task graph.
			graph.Status = schemas.GraphFailed
			graph.BudgetPaused = true
			if s.cancelFuncs != nil {
				if cancel, ok := s.cancelFuncs[taskID]; ok {
					cancel()
				}
			}
		case "continue":
			// Cost governance: user rejected the abort request, resume execution.
			step.Status = schemas.StepPending
		case "modify_and_resume":
			step.Status = schemas.StepPending
			if payload != "" {
				var mod map[string]any
				if json.Unmarshal([]byte(payload), &mod) == nil {
					if task, ok := mod["task"].(string); ok {
						step.Task = task
					}
				}
			}
		case "create_skill":
			// User is expected to have registered the skill, just retry
			step.Status = schemas.StepPending
			step.RetryCount = 0
			step.LastError = ""
		case "create_role":
			// Unreachable: the create_role work happens above, before this
			// switch, because it needs the LLM and the cookbook loader. Kept
			// as an explicit no-op so the step state already written there is
			// not clobbered and the generic default branch does not reject
			// the choice with "unknown choice".
		case "goal_change_requested":
			// Unreachable: the charter update happens above, before this
			// switch. Kept as an explicit no-op so the generic default
			// branch does not reject the choice with "unknown choice".
		case "replan":
			// Task-level replan (ADDED 2026-10-18).
			//
			// Previously "replan" was offered as a decision option by
			// RequestAutonomousAbort but had NO case here, so choosing it
			// consumed the decision and changed nothing. The planner's
			// replacement steps (produced by tryPlannerReplan and stored in
			// dec.Context["planner_replan"]) are now injected through the
			// existing DAG surgery primitive.
			//
			// Payload precedence: an explicit `output` argument (surgery JSON
			// supplied by the caller) wins over the planner's stored plan.
			surgeryJSON := payload
			if strings.TrimSpace(surgeryJSON) == "" {
				if v, ok := dec.Context["planner_replan"].(string); ok {
					surgeryJSON = v
				}
			}
			if strings.TrimSpace(surgeryJSON) == "" {
				return fmt.Errorf("replan requires replacement steps: no payload and no planner output on this decision")
			}

			var data SurgeryData
			if err := json.Unmarshal([]byte(surgeryJSON), &data); err != nil {
				return fmt.Errorf("invalid replan payload: %v", err)
			}
			if data.DeprecatedStepID == "" {
				data.DeprecatedStepID = dec.StepID
			}
			if len(data.ReplacementSteps) == 0 {
				return fmt.Errorf("replan payload contains no replacement_steps")
			}

			if err := s.mutateGraphTopologyLocked(graph, data); err != nil {
				return fmt.Errorf("replan injection failed: %v", err)
			}
			s.logger.Log("planner_replan_applied", taskID, dec.StepID, map[string]any{
				"replacement_steps": len(data.ReplacementSteps),
				"deprecated":        data.DeprecatedStepID,
			})
		default:
			switch {
			case strings.HasPrefix(choice, "retry_with_skill:"):
				skill := strings.TrimPrefix(choice, "retry_with_skill:")
				step.AdditionalSkills = append(step.AdditionalSkills, skill)
				step.Status = schemas.StepPending
				step.RetryCount = 0
				step.LastError = ""
			case strings.HasPrefix(choice, "retry_with_context:"):
				ctxData := strings.TrimPrefix(choice, "retry_with_context:")
				step.AdditionalPromptContext = ctxData
				step.Status = schemas.StepPending
				step.RetryCount = 0
			case strings.HasPrefix(choice, "escalate_model:"):
				model := strings.TrimPrefix(choice, "escalate_model:")
				// Availability check: verify the requested model is configured
				// before setting the override. Without this, an unavailable
				// model silently falls back to the role's default provider
				// (e.g. gemini), defeating the escalation and causing
				// confusion + repetition loops. Bug P0-2 (2026-10-28).
				if s.registry != nil {
					if s.registry.GetProvider(model) == nil && s.registry.FindProviderByModel(model) == nil {
						return fmt.Errorf("escalate_model: model %q is not available — not a provider ID and no configured provider serves this model", model)
					}
				}
				step.ProviderOverride = model
				step.Status = schemas.StepPending
				step.RetryCount = 0
			default:
				return fmt.Errorf("unknown choice %q", choice)
			}
		}
	}
	return nil
}

// applyCreateRoleChoiceLocked synthesises a task-scoped role draft and parks
// it in this task's session IR. Moved out of SubmitDecisionWithPayload on
// 2026-11-04 during the O25 file split; single call site. Its contract, the
// two-phase draft/apply split and why the old global-flipping handler was
// removed are documented at the original call site's history.
func (s *DirectedEngine) applyCreateRoleChoiceLocked(
	taskID, decisionID string, graph *schemas.TaskGraph, step *schemas.Step, dec *schemas.Decision, payload string,
) error {
	roleID := step.RoleID
	if v, ok := dec.Context["role_id"].(string); ok && v != "" {
		roleID = v
	}
	if roleID == "" {
		return fmt.Errorf("create_role: step %q declares no role_id to synthesise", dec.StepID)
	}
	// A caller-supplied draft wins: it lets a human pre-write the
	// instruction while still going through the same activation path.
	var draftRole config.Role
	draftFromPayload := false
	if strings.TrimSpace(payload) != "" {
		if err := json.Unmarshal([]byte(payload), &draftRole); err != nil {
			return fmt.Errorf("create_role: invalid role draft payload: %v", err)
		}
		draftFromPayload = true
	}

	if !draftFromPayload {
		loader := NewResourceLoader()
		if s.spawner != nil && s.spawner.ResourceLoader != nil {
			loader = s.spawner.ResourceLoader
		}
		gen := NewRoleGenerator(s.registry, loader, s.logger)
		generated, skill, err := gen.GenerateRoleObjects(
			s.lifecycleCtx, taskID, dec.StepID, roleID, step.Task,
		)
		if err != nil {
			// The decision stays on the graph, so the operator can fall
			// back to abort/skip or register the role out-of-band.
			return fmt.Errorf("create_role: role generation failed: %w", err)
		}
		draftRole = *generated
		if skill != nil {
			graph.SetSessionSkill(skill.ID, skill)
		}
	}
	if draftRole.ID == "" {
		draftRole.ID = roleID
	}

	// Namespace the id so the session role cannot shadow a real global
	// role of the same name, then store it in this task's session IR.
	scopedID, err := AppendSessionRole(graph, taskID, &draftRole)
	if err != nil {
		return fmt.Errorf("create_role: %v", err)
	}
	// The role THIS step dispatches as must be the namespaced one,
	// otherwise LogInterceptor still resolves the missing original id.
	if step.RoleID == roleID {
		step.RoleID = scopedID
	}

	step.Status = schemas.StepPending
	step.RetryCount = 0
	step.LastError = ""
	// Observability only: Metadata.Data is the free-form slot
	// (schemas.Metadata has no typed field for this).
	if step.Metadata == nil {
		step.Metadata = &schemas.Metadata{Namespace: "role_gen", Version: "1"}
	}
	data, _ := step.Metadata.Data.(map[string]any)
	if data == nil {
		data = make(map[string]any)
	}
	data["synthesized_role_id"] = scopedID
	data["requested_role_id"] = roleID
	step.Metadata.Data = data

	s.logger.Log("EventRoleGenerationCompleted", taskID, dec.StepID, map[string]any{
		"role_id":         scopedID,
		"requested_as":    roleID,
		"from_payload":    draftFromPayload,
		"scope":           "session (manifest.json only)",
		"decision_choice": "create_role",
	})
	if s.auditLogger != nil {
		s.auditLogger.Log("createRoleSessionDraft", taskID, dec.StepID, decisionID, scopedID, roleID, nil)
	}
	return nil
}
