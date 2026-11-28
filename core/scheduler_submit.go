package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func (s *DirectedEngine) validateTaskComplexity(inputs []schemas.StepInput) error {
	if len(inputs) == 0 {
		return errors.New("steps is required")
	}

	// RejectSimpleTasks gate (opt-in, ADDED 2026-10-20): when the flag is off
	// (the default), single-step local tasks are accepted so they can reach
	// the Smart Routing Fast-Path below, which already handles them with
	// near-zero overhead (minimal graph entry + Spawn, no DAG/manifest/run
	// loop). Only opt into the rejection for local (llama.cpp/vLLM)
	// deployments that want to prevent orchestrator misuse as a plain shell.
	if !s.rejectSimpleTasksEnabled() {
		return nil
	}

	// Rule: Reject tasks that are just a single "local" step with no specialized skills.
	// These are high-overhead and should be done directly by the caller.
	if len(inputs) == 1 {
		inp := inputs[0]
		s.registry.Mu.RLock()
		role := s.registry.Roles[inp.RoleID]
		s.registry.Mu.RUnlock()

		isLocal := false
		if role != nil && role.Provider == "local" {
			isLocal = true
		} else if role == nil && s.registry.DefaultProvider == "local" {
			// Fallback if role is not yet created (dynamic generation)
			isLocal = true
		}

		if isLocal && len(inp.AdditionalSkills) == 0 && len(inp.AdditionalMCPs) == 0 {
			return fmt.Errorf("REJECTED: Task %q is too simple for orchestration. "+
				"Single-step local tasks incur unnecessary scheduling overhead. "+
				"Please execute this task directly using your local tools.", inp.Task)
		}
	}

	return nil
}

// validateWorkspaceKeyCollisions checks for parallel steps that both use
// "overlay" merge strategy on the same TargetKey. Such steps produce
// non-deterministic results (last writer wins) because parallel steps
// execute in arbitrary order. Logs a warning; does not reject the task.
//
// Zone 2 boundary fix (C10): two steps are "parallel" if neither directly
// depends on the other. This is a conservative check — it may flag steps
// that are transitively ordered, but over-warning is safer than missing a
// real collision.
func (s *DirectedEngine) validateWorkspaceKeyCollisions(steps map[string]*schemas.Step) {
	// Build TargetKey → overlay step IDs index.
	overlayByKey := make(map[string][]string)
	for _, step := range steps {
		if step.MergeStrategy == "overlay" && step.TargetKey != "" {
			overlayByKey[step.TargetKey] = append(overlayByKey[step.TargetKey], step.ID)
		}
	}

	for key, stepIDs := range overlayByKey {
		if len(stepIDs) < 2 {
			continue
		}
		// Check each pair for direct dependency.
		for i := 0; i < len(stepIDs); i++ {
			for j := i + 1; j < len(stepIDs); j++ {
				a, b := stepIDs[i], stepIDs[j]
				aDependsOnB := sliceContains(steps[a].DependsOn, b)
				bDependsOnA := sliceContains(steps[b].DependsOn, a)
				if !aDependsOnB && !bDependsOnA {
					s.logger.Log("EventWorkspaceKeyCollision", "", "", map[string]any{
						"target_key": key,
						"step_a":     a,
						"step_b":     b,
						"warning":    "parallel overlay on same TargetKey — non-deterministic result",
					})
				}
			}
		}
	}
}

// sliceContains is a simple slice membership check for string slices.
func sliceContains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func (s *DirectedEngine) Submit(inputs []schemas.StepInput) (string, error) {
	return s.SubmitWithSessionIR(inputs, nil, nil, nil, "", "", "", 0, 0, "")
}

func (s *DirectedEngine) SubmitOwned(inputs []schemas.StepInput, ownerID string) (string, error) {
	return s.SubmitWithSessionIR(inputs, nil, nil, nil, "", "", "", 0, 0, ownerID)
}

func firstScheduleDelivery(inputs []schemas.StepInput) *schemas.ScheduleDelivery {
	for i := range inputs {
		if inputs[i].Delivery != nil {
			return inputs[i].Delivery
		}
	}
	return nil
}

// SubmitWithSession is a convenience wrapper that propagates sessionID to the
// task graph so the WireTaskCompletionCallbacks callback can find the chat
// session and notify it when the task completes. Without this, delegated tasks
// have graph.SessionID="" and the completion callback silently returns.
func (s *DirectedEngine) SubmitWithSession(inputs []schemas.StepInput, sessionID string) (string, error) {
	return s.SubmitWithSessionIR(inputs, nil, nil, nil, "", sessionID, "", 0, 0, "")
}

// SubmitWithWorkspace is SubmitWithSession plus the workspace root.
//
// SubmitWithSession hardcodes workspaceRoot="" — right for MCP callers that
// never had one, wrong for the chat gateway, which *does* know the workspace
// (ChatHarness.SessionRoot, sourced from HARNESSBENCH_WORKSPACE) but used to
// drop it at this exact call. The consequence was not an error anywhere: the
// graph got WorkspaceRoot="", scheduler_dag_exec passed that to SpawnRequest,
// and every step agent's core tools resolved against outputs/<taskID> — a
// directory containing only manifest.json. The delegated agent could still
// write files ("it worked"), it just wrote them where nobody would look and
// could not read the documents it was asked to analyse.
//
// harness-bench D3, 2026-10-30. See T2-STALL-RESUME-2026-10-30.md §12.7.
func (s *DirectedEngine) SubmitWithWorkspace(inputs []schemas.StepInput, sessionID, workspaceRoot string) (string, error) {
	return s.SubmitWithSessionIR(inputs, nil, nil, nil, "", sessionID, workspaceRoot, 0, 0, "")
}

func (s *DirectedEngine) SubmitWithSessionIR(inputs []schemas.StepInput, roles []*config.Role, skills []*config.Skill, providers []*config.ProviderConfig, mainProviderID string, sessionID string, workspaceRoot string, timeoutSecs int, tokenBudget int64, ownerID string) (taskID string, err error) {
	// C4: capture original inputs before any modification (expandSkills,
	// planner-first). The defer must store the caller's original input,
	// not planner-produced steps.
	originalInputs := make([]schemas.StepInput, len(inputs))
	copy(originalInputs, inputs)

	// C4: only store drafts for errors after validation passes. Security
	// rejections (workspace scope, guard violations) must not persist
	// potentially malicious content in memory.
	passedValidation := false

	defer func() {
		if err == nil || s.drafts == nil || len(originalInputs) == 0 || !passedValidation {
			return
		}
		draftID := "draft_" + uuid.New().String()[:8]
		steps := make([]any, len(originalInputs))
		for i, inp := range originalInputs {
			steps[i] = inp
		}
		s.drafts.Save(&TaskDraft{
			DraftID:    draftID,
			Steps:      steps,
			CreatedAt:  time.Now(),
			LastError:  err.Error(),
			FailedStep: -1,
		})
		// C4: do NOT set taskID = draftID on error. Callers must check err
		// first; returning a draft ID as taskID risks it being used as a
		// task identifier.
	}()
	// ── Tier-Gated Workspace Scope: Complete Mediation (I3) ───────────
	// Defense-in-depth: even if the tools layer's ResolveWorkspaceRoot
	// was bypassed (e.g. direct API handler), the scheduler re-checks
	// that workspaceRoot is within the allowed scope for the owner.
	// See docs/completed/2026-10-21/TIER_GATED_WORKSPACE_SCOPE_DESIGN.md §3.5.
	if err := s.validateWorkspaceScope(workspaceRoot, ownerID); err != nil {
		return "", fmt.Errorf("workspace scope violation: %w", err)
	}

	// ── Session-to-Workspace Binding (D1) ───────────────────────────────
	// Validate workspaceRoot against the AllowedWorkspaces whitelist before
	// accepting any work. Nil-safe: when s.Sessions is nil (tests, backward
	// compat), the check is skipped. When sessionID/workspaceRoot are empty
	// (legacy Submit() path), the check is also skipped.
	if s.Sessions != nil && sessionID != "" && workspaceRoot != "" {
		if _, err := s.Sessions.CreateOrBind(sessionID, workspaceRoot); err != nil {
			return "", fmt.Errorf("session-workspace binding rejected: %w", err)
		}
	}
	// ────────────────────────────────────────────────────────────────────

	// ── Smart Runtime Guardrail (Input Guard) ────────────────────────
	if err := EvaluateAndGuardInputs(inputs, s.registry); err != nil {
		s.logger.Log("GuardViolationIntercepted", "", "", map[string]any{"error": err.Error()})
		return "", err
	}
	// ────────────────────────────────────────────────────────────────

	// ── Pre-check: submission validity (complexity + role callability) ─
	if err := s.validateSubmission(inputs); err != nil {
		return "", err
	}

	// C4: all validation/security checks passed. Drafts may now be saved
	// for downstream errors (but not for the security rejections above).
	passedValidation = true

	// ── Directory-Skill Expansion (PRD §2.2, Skill Interpreter) ──────
	// If the first input targets a role bound to a DirectorySkill, expand
	// it into a multi-step DAG. No-op when no DirectorySkill is bound.
	expandInputs, expandErr := s.expandSkillsIfNecessary(inputs, roles, skills)
	if expandErr != nil {
		return "", expandErr
	}
	inputs = expandInputs
	// ────────────────────────────────────────────────────────────────

	// Planner-first is opt-in, budgeted and best-effort for one dependency-free
	// step. Multi-step submissions already carry the caller's DAG design.
	// See docs/architecture/ARCHITECTURE_FLOWS.md §4.
	if len(inputs) == 1 && len(inputs[0].DependsOn) == 0 && s.plannerFirstEnabled() {
		probeTask := inputs[0].Task
		planTaskID := schemas.NewTaskID()
		planned, reason := s.RequestPlanning(planTaskID, inputs[0].RoleID, probeTask, 0, inputs[0].SubmitterIsAdmin, ownerID)
		if reason != "" {
			s.logger.Log("planner_first_skipped", planTaskID, inputs[0].ID, map[string]any{
				"reason": reason,
			})
		}
		if len(planned) > 0 {
			// The planner owns the DAG now. Preserve the caller's
			// per-run settings that are not part of a plan (skills,
			// context refs, deliver contract, verifier config) onto
			// the terminal steps, otherwise the plan would silently
			// drop them.
			inheritCallerSettings(planned, inputs[0])
			// 🔴 A one-step plan buys nothing structural. It is the same
			// single step with no dependency, so there is no parallelism and
			// no context isolation — the two things a plan exists to buy.
			// Its ONLY delta is replacing the caller's task text, and that
			// replacement is measurably lossy: rewriting "payment requests
			// which have not been approved" as "fetch all payment requests,
			// then filter" moves a query argument into a post-processing
			// stage, so the executor drops the server-side filter and starts
			// scanning the unfiltered collection.
			//
			// Measured 4fab96f_3 (K=3, temperature 0, docs/54 §5.24): all
			// three trials were single_step, all three step texts said
			// "fetch all"; the executor paged 13 unfiltered pages with three
			// different page_limit values and never read payment_request
			// 1624 — 2-3/8, while the plain-ReAct arm read 7 filtered pages
			// and scored 8/8.
			//
			// So when the planner returns exactly the shape we already had,
			// keep the caller's task verbatim. The fast-path below is then
			// the plain ReAct loop it is supposed to be.
			if len(planned) == 1 && strings.TrimSpace(inputs[0].Task) != "" {
				planned[0].Task = inputs[0].Task
			}
			s.logger.Log("planner_first_applied", planTaskID, inputs[0].ID, map[string]any{
				"steps":     len(planned),
				"role":      inputs[0].RoleID,
				"task_kept": len(planned) == 1,
			})
			inputs = planned
		}
	}
	// ────────────────────────────────────────────────────────────────

	// Smart Routing Fast-Path (ADDED 2026-08-27):
	// After validateTaskComplexity passes (task is complex enough for
	// orchestration), check if it's a single-step task with no
	// dependencies. Such tasks are intermediate-complexity: too heavy for
	// "too simple" rejection, but too light to warrant full DAG
	// construction. Route them directly through spawner.Spawn for zero
	// overhead, skipping graph persistence + run() loop.
	s.Mu.RLock()
	requireReview := s.registry.RequirePlanReview
	s.Mu.RUnlock()

	inp := inputs[0]

	if len(inputs) == 1 && !requireReview && len(inp.DependsOn) == 0 {
		taskID := schemas.NewTaskID()
		traceID := NewTraceID()
		stepID := inp.ID
		if stepID == "" {
			stepID = fmt.Sprintf("step_%s", uuid.New().String()[:6])
		}

		ctx := ContextWithTrace(s.lifecycleCtx, traceID, NewSpanID()) // audit P-MED-4: derive from lifecycleCtx so Stop() cancels

		s.logger.LogCtx(ctx, "smart_route", taskID, stepID, map[string]any{
			"task":        inp.Task,
			"role":        inp.RoleID,
			"mcp_count":   len(inp.AdditionalMCPs),
			"skill_count": len(inp.AdditionalSkills),
		})

		// Register a minimal TaskGraph so GetStatus/WaitTask can observe
		// this task. Without this, the fast-path returned a task_id that
		// was completely invisible to GetStatus (always "not found"),
		// causing silent-failure from the caller's perspective.
		step := &schemas.Step{
			ID:                  stepID,
			RoleID:              inp.RoleID,
			Task:                inp.Task,
			Status:              schemas.StepPending,
			AdditionalSkills:    inp.AdditionalSkills,
			AdditionalMCPs:      inp.AdditionalMCPs,
			ContextRefs:         inp.ContextRefs,
			Deliver:             inp.Deliver,
			Delivery:            inp.Delivery,
			MaxTurnsCap:         inp.MaxTurnsCap,
			ExitCriteria:        inp.ExitCriteria,
			DeterministicChecks: inp.DeterministicChecks,
			VerifierModel:       inp.VerifierModel,
			RequireAudit:        inp.RequireAudit,
			MaxAutoRefine:       inp.MaxAutoRefine,
			EnableDebate:        inp.EnableDebate,
			MaxDebateRounds:     inp.MaxDebateRounds,
			EnableStepPlan:      inp.EnableStepPlan,
			SpawnDepth:          inp.SpawnDepth,
			MaxSpawnDepth:       inp.MaxSpawnDepth,
			AutoRefineCount:     0,
			LastWorkedOn:        time.Now(),
			InputDir:            inp.InputDir,
			// D3 §10.2: copy these so overlay merge fires for single-step tasks.
			MergeStrategy: inp.MergeStrategy,
			TargetKey:     inp.TargetKey,
		}
		if step.AdditionalSkills == nil {
			step.AdditionalSkills = []string{}
		}
		if step.AdditionalMCPs == nil {
			step.AdditionalMCPs = []string{}
		}
		if step.ContextRefs == nil {
			step.ContextRefs = map[string]string{}
		}

		graph := &schemas.TaskGraph{
			TaskID:    taskID,
			TraceID:   traceID,
			ProjectID: defaultProjectID(inp.ProjectID),
			Steps:     map[string]*schemas.Step{stepID: step},
			Status:    schemas.GraphRunning, SubmitterIsAdmin: inp.SubmitterIsAdmin,
			PendingDecisions: []*schemas.Decision{},
			OutputFiles:      []schemas.OutputFile{},
			CreatedAt:        time.Now(),
			ContextTree:      map[string]*schemas.ContextNode{},
			SessionRoles:     map[string]any{},
			SessionSkills:    map[string]any{},
			GlobalWorkspace:  map[string]any{},
			IsSmartRouted:    true,
			FastPath:         true,
			SessionID:        sessionID,
			OwnerID:          ownerID,
			WorkspaceRoot:    workspaceRoot,
			Delivery:         firstScheduleDelivery(inputs),
			TimeoutSecs:      timeoutSecs,
			TokenBudget:      tokenBudget,
		}
		graph.ContextTree["root"] = &schemas.ContextNode{
			ID:     "root",
			Intent: inp.Task,
			Status: schemas.NodeActive,
		}
		graph.CurrentNodeID = "root"

		// Task-Local Closure: snapshot JIT tools so TTL expiry mid-task
		// doesn't cause "tool not found". See
		// docs/completed/2026-09-15/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §2.1.
		if s.jit != nil {
			graph.LocalMCPs = s.jit.SnapshotJITTools(inp.AdditionalMCPs)
		}

		// Input Seeding: copy declared input directories into task workspace.
		if err := s.seedTaskInputs(graph, s.outputBase); err != nil {
			s.logger.Log("EventInputSeedError", graph.TaskID, "", map[string]any{"error": err.Error()})
		}

		s.resolveGraphVerifiers(graph)

		s.Mu.Lock()
		s.graphs[taskID] = graph
		// audit C2: derive from lifecycleCtx so Stop() cancels tasks.
		// audit P-MED-4: apply timeoutSecs if configured to prevent indefinite hang.
		var taskCancel context.CancelFunc
		if timeoutSecs > 0 {
			ctx, taskCancel = context.WithTimeout(s.lifecycleCtx, time.Duration(timeoutSecs)*time.Second)
		} else {
			ctx, taskCancel = context.WithCancel(s.lifecycleCtx)
		}
		s.cancelFuncs[taskID] = taskCancel
		s.doneChans[taskID] = make(chan struct{})

		// D5 P1b (DELEGATION_HANDOVER_REDESIGN.md §4.5): in delegation mode
		// with planner-first enabled, delegate the PLANNER's prompt to the
		// host instead of skipping the planner (P1a) or running it locally.
		// The host produces a plan; FulfillStep injects it via surgery.
		planDelegated := false
		if s.registry != nil && s.registry.System.DelegationMode && s.registry.System.EnablePlannerFirst {
			step.Status = schemas.StepBlocked
			planPrompt := buildPlanDelegationPrompt(inp.Task, inp.RoleID)
			graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
				ID:     schemas.NewDecisionID(),
				StepID: stepID,
				Type:   schemas.DecisionDelegationRequired,
				Context: map[string]any{
					"kind":    DelegationKindPlan,
					"prompt":  planPrompt,
					"role_id": inp.RoleID,
				},
				Options: []string{"fulfill", "skip", "abort"},
			})
			planDelegated = true
		}

		s.Mu.Unlock()

		s.persistGraph(graph) // Re-enabled: needed for task visibility after restart (loadGraphs)
		s.pinBaseline(taskID, inputs)
		s.Broadcast()

		if planDelegated {
			// The step is blocked waiting for the host's plan. Don't spawn.
			s.logger.Log("plan_delegation_created", taskID, stepID, map[string]any{
				"kind": DelegationKindPlan,
			})
			return taskID, nil
		}

		hub := NewContextHub(s.registry, graph, s.expStore)

		spawnReq := &SpawnRequest{
			TaskID:                   taskID,
			StepID:                   stepID,
			TraceID:                  traceID,
			RoleID:                   inp.RoleID,
			Task:                     inp.Task,
			AdditionalSkills:         inp.AdditionalSkills,
			AdditionalMCPs:           inp.AdditionalMCPs,
			AdditionalToolAllowlists: inp.AdditionalToolAllowlists,
			ContextRefs:              inp.ContextRefs,
			ProviderOverride:         inp.ProviderOverride,
			RoutingMode:              inp.RoutingMode,
			CompressionHint:          inp.CompressionHint,
			SessionRoot:              workspaceRoot,
			Hub:                      hub,
			Isolation:                inp.Isolation,
			EnableStepPlan:           inp.EnableStepPlan,
			MaxTurnsCap:              inp.MaxTurnsCap,
		}

		// Execute asynchronously — same fire-and-forget semantics as
		// standard task submission so caller's API contract (returns
		// taskID, executes in background) is preserved.
		// On completion, update graph status + close doneChans so
		// WaitTask/GetStatus return the real result.
		s.goBackground(func() { // audit C1: use goBackground so Stop()'s bgWg.Wait() drains this
			var finalResult *SpawnResult
			var finalErr error

			// ── FastPath Short-Circuit (ADDED 2026-09-13) ──────────────────
			// If the step has a deterministic OutputContract (Path + schema),
			// try FastPathEngine first — zero LLM overhead. On success, skip
			// the Spawn + refine loop entirely. On failure, fall back to Spawn.
			fastPathHit := false
			if isDeterministicContract(inp) {
				if fpResult, fpErr := s.tryFastPath(ctx, inp, taskID, stepID); fpErr == nil {
					finalResult = fpResult
					finalErr = nil
					fastPathHit = true
					observability.GetGlobalMetrics().Inc("orchestrator.fast_path.hits", 1)
					s.logger.Log("EventFastPathHit", taskID, stepID, map[string]any{
						"path": inp.OutputContract.Path,
					})
				} else {
					observability.GetGlobalMetrics().Inc("orchestrator.fast_path.misses", 1)
					s.logger.Log("EventFastPathFallback", taskID, stepID, map[string]any{"err": fpErr.Error()})
				}
			}

			if !fastPathHit {
				// ADDED (2026-09-07): Auto-Refine loop. When a step declares
				// exit_criteria and fails the audit, we re-spawn it with the audit
				// feedback injected as AdditionalPromptContext, up to MaxAutoRefine
				// times. This makes exit_criteria genuinely enforceable in the
				// single-step / smart_route path.
				for {
					result, err := s.spawner.Spawn(ctx, spawnReq)
					finalResult = result
					finalErr = err

				// Audit gate. Runs when the step completed OK and either declared
				// exit criteria or requested audit (O47 P1 FIX: require_audit=true
				// must trigger the gate even when ExitCriteria is empty).
				if err == nil && result != nil &&
					(step.ExitCriteria != "" || step.RequireAudit || step.VerifierModel != "") &&
					(result.Output.Status == schemas.StatusOK || result.Output.Status == schemas.StatusPartial) {
						if ok, reason, failType := s.verifyExitCriteria(ctx, graph, step, result); !ok {
							if step.AutoRefineCount < step.MaxAutoRefine {
								s.Mu.Lock() // audit C-3: protect field writes — persistGraph reads under RLock.
								step.AutoRefineCount++
								step.LastError = store.SanitizeError(fmt.Sprintf("[%s] Exit criteria not met: %s. Refining...", failType, reason))
								s.Mu.Unlock()
								s.logger.Log("EventStepRetrying", graph.TaskID, step.ID, map[string]any{
									"reason": "exit_criteria_fail", "details": reason,
									"refine_attempt": step.AutoRefineCount, "fail_type": failType,
								})
								// ── AntiPattern Injection (Module 2, Two-Tier Design) ──────────
								// Query the AntiPatternStore for historical precedents matching
								// this step's domain + the specific rejection reason, and inject
								// them as a [PREVIOUS FAILURE ANTI-PATTERN] block. This gives the
								// weak model curated "error-book" guidance on retry, reducing its
								// secondary error rate. See core/antipattern_injection.go.
								antiPatternBlock := s.buildAntiPatternGuidance(step.Task, reason)
								if antiPatternBlock != "" {
									s.logger.Log("EventAntiPatternInjected", graph.TaskID, step.ID, map[string]any{
										"refine_attempt": step.AutoRefineCount,
										"source":         "antipattern_store",
									})
								}
								// Compose the full recovery prompt: prior context + rejection +
								// anti-pattern guidance. Single composition point keeps the
								// retry-prompt shape consistent and testable.
								spawnReq.AdditionalPromptContext = composeRecoveryPrompt(
									spawnReq.AdditionalPromptContext, reason, antiPatternBlock,
								)
								continue // re-spawn with recovery context
							}
							// Refine budget exhausted → mark failed (audit rejected).
							// Race fix: protect step/graph status with Mu.
							s.Mu.Lock()
							step.Status = schemas.StepFailed
							step.LastError = store.SanitizeError(fmt.Sprintf("exit_criteria failed after %d refine attempts: %s", step.AutoRefineCount, reason))
							graph.Status = schemas.GraphFailed
							s.Mu.Unlock()
							break
						}
						// Audit passed → fall through to success path below.
					}
					break // success or non-audit failure → stop retrying
				}
			}

			s.Mu.Lock()
			delegationBlocked := false
			approvalBlocked := false
			if finalErr != nil {
				step.Status = schemas.StepFailed
				step.LastError = finalErr.Error()
				graph.Status = schemas.GraphFailed
			} else if finalResult != nil {
				conf := finalResult.Output.Confidence
				step.Confidence = &conf
				// Subagent standard (2026-10-23): the single-step fast path used
				// to collapse every non-delegation status to StepOK, so a
				// subagent reporting partial/failed was reported to the caller
				// as a SUCCESS ("I couldn't finish" → completed). That diverged
				// from the walker (scheduler_decision.go routes non-ok to
				// handleStepFailure) and from how real orchestrator-worker systems
				// treat a worker result: partial/failed is NOT done — Anthropic's
				// Research verifies subagent findings in a separate pass before
				// merging. Route by the subagent's actual status.
				status := finalResult.Output.Status
				// Exception: a ParseOutput fallback is a FORMAT failure, not the
				// model declaring incompleteness. A plain-text answer to a simple
				// task is legitimate, and routing it to a decision the caller
				// cannot act on would regress the pinned single-step behavior
				// (TestSubmit_SingleStepPublishesTerminalEvent, §3). Keep it on the
				// completion path.
				if status == schemas.StatusPartial && isParseFallback(finalResult.Output) {
					status = schemas.StatusOK
				}
				switch status {
				case schemas.StatusApprovalRequired:
					step.Status = schemas.StepBlocked
					approvalBlocked = true
				case schemas.StatusDelegationRequired:
					step.Status = schemas.StepBlocked
					delegationBlocked = true
				case schemas.StatusOK:
					step.ResultRef = finalResult.Ref
					step.StatesVisited = finalResult.StatesVisited // PGPO (ADDED 2026-09-08)
					if step.Status != schemas.StepFailed {
						if step.Status != schemas.StepOK {
							step.Status = schemas.StepOK
						}
						graph.Status = schemas.GraphCompleted
						// ── Auto-Deposition (ADDED 2026-08-31) ──────────────────────────
						AutoDepositResult(graph, step.ID, finalResult.Output.Result)
						s.logger.Log("EventAutoDeposit", graph.TaskID, step.ID, nil)

						// ── Experience Evolution (ADDED 2026-09-08) ──
						if step.TriggerError != "" && step.AdditionalPromptContext != "" {
							s.DepositExperienceEvolution(ctx, graph, step)
						}
					}
				case schemas.StatusPartial:
					// Partial is not success — but the fast path is loop-less: it
					// never starts run() (startGraphExecutor is multi-step only),
					// so raising a decision here deadlocks. The decision's
					// Broadcast() has no consumer and the task stays "running"
					// until the harness budget timer expires (PREREG-C5DD §7.2).
					// Honest terminal instead: preserve StepPartial + the artifact
					// (ResultRef) so the caller can inspect what was produced, mark
					// the graph failed (partial ≠ success), and fall through to the
					// terminal-publish block. The caller decides at the call
					// boundary — the react/loop model, not a suspended decision.
					step.ResultRef = finalResult.Ref
					step.StatesVisited = finalResult.StatesVisited
					step.Status = schemas.StepPartial
					graph.Status = schemas.GraphFailed
				default:
					// failed / capability_required / approval_required / unknown:
					// the subagent did not succeed. Terminal failure so the task
					// never reports "completed" on a non-ok result.
					step.ResultRef = finalResult.Ref
					step.StatesVisited = finalResult.StatesVisited
					step.Status = schemas.StepFailed
					step.LastError = store.SanitizeError("step returned status " + string(finalResult.Output.Status))
					graph.Status = schemas.GraphFailed
				}
			} else {
				step.Status = schemas.StepFailed
				step.LastError = "spawn returned nil result"
				graph.Status = schemas.GraphFailed
			}
			s.Mu.Unlock()

			if delegationBlocked {
				s.addDecision(graph, step, schemas.DecisionDelegationRequired, map[string]any{
					"kind":    DelegationKindExternalSubagent,
					"prompt":  finalResult.Output.Result,
					"role_id": step.RoleID,
				}, []string{"fulfill", "skip", "abort"})
				s.Broadcast()
				// C5DD (2026-10-29): this fast path never called startGraphExecutor,
				// so there is no run() loop to receive the Broadcast above. SubmitDecision
				// flips the step back to StepPending and only broadcasts: with nothing
				// waiting, that retry is never dispatched and the task sits in `running`
				// forever (the aw20 stall). Starting the executor is safe here because
				// ReadySteps() only returns StepPending steps -- s1 is StepPartial and a
				// decision is pending, so run() parks on the notify channel and wakes when
				// the answer arrives.
				s.startGraphExecutor(ctx, taskID)
				return
			}
			if approvalBlocked {
				s.emitMountApprovalDecision(graph, step, finalResult.Output.ApprovalRequiredMounts, finalResult.Output.ApprovalRequiredSkills)
				s.Broadcast()
				s.startGraphExecutor(ctx, taskID)
				return
			}
			s.persistGraph(graph)
			s.broadcastDone(taskID) // safe close under Mu with double-close guard
			// §3 fix: publish terminal event for single-step fast-path tasks.
			// Without this, webhook/callback subscribers never hear about these
			// tasks (the common case). See docs/completed/2026-10-06/TASK_WEBHOOK_CALLBACK_DESIGN.md §3.
			s.Mu.Lock()
			conf := graph.ComputeConfidence()
			isFailed := graph.Status == schemas.GraphFailed
			s.Mu.Unlock()
			if isFailed {
				s.publishEvent(taskID, "", obs.EventTaskFailed, map[string]any{"confidence": conf})
			} else {
				s.publishEvent(taskID, "", obs.EventTaskCompleted, map[string]any{"confidence": conf})
			}
			s.Broadcast()
		}) // audit C1: goBackground closing

		return taskID, nil
	}

	taskID = schemas.NewTaskID()
	traceID := NewTraceID()
	steps := make(map[string]*schemas.Step, len(inputs))

	for _, inp := range inputs {
		maxRetries := inp.MaxRetries
		if maxRetries == 0 {
			maxRetries = 3
		}

		stepID := inp.ID
		if stepID == "" {
			stepID = fmt.Sprintf("step_%s", uuid.New().String()[:6])
		}

		step := &schemas.Step{
			ID:                       stepID,
			RoleID:                   inp.RoleID,
			Task:                     inp.Task,
			DependsOn:                inp.DependsOn,
			AdditionalSkills:         inp.AdditionalSkills,
			AdditionalMCPs:           inp.AdditionalMCPs,
			AdditionalToolAllowlists: inp.AdditionalToolAllowlists,
			ContextRefs:              inp.ContextRefs,
			FallbackFor:              inp.FallbackFor,
			Status:                   schemas.StepPending,
			MaxRetries:               maxRetries,
			ExitCriteria:             inp.ExitCriteria,
			DeterministicChecks:      inp.DeterministicChecks,
			VerifierModel:            inp.VerifierModel,
			RequireAudit:             inp.RequireAudit,
			MaxAutoRefine:            inp.MaxAutoRefine,
			EnableDebate:             inp.EnableDebate,
			MaxDebateRounds:          inp.MaxDebateRounds,
			EnableStepPlan:           inp.EnableStepPlan,
			OutputContract:           inp.OutputContract,
			Deliver:                  inp.Deliver, // ADDED 2026-08-30
			Delivery:                 inp.Delivery,
			MaxTurnsCap:              inp.MaxTurnsCap,
			ProviderOverride:         inp.ProviderOverride,
			LastWorkedOn:             time.Now(),
			GroupID:                  inp.GroupID,
			FailurePolicy:            inp.FailurePolicy,
			CompressionHint:          inp.CompressionHint,
			SpawnDepth:               inp.SpawnDepth,
			MaxSpawnDepth:            inp.MaxSpawnDepth,
			MaxLoopRounds:            inp.MaxLoopRounds,
			DynamicRubrics:           inp.DynamicRubrics,
			InputDir:                 inp.InputDir,
			MergeStrategy:            inp.MergeStrategy,
			TargetKey:                inp.TargetKey,
			InputMapping:             inp.InputMapping,
			Isolation:                inp.Isolation,
		}
		if step.AdditionalSkills == nil {
			step.AdditionalSkills = []string{}
		}
		if step.AdditionalMCPs == nil {
			step.AdditionalMCPs = []string{}
		}
		if step.ContextRefs == nil {
			step.ContextRefs = map[string]string{}
		}
		if step.AdditionalToolAllowlists == nil {
			step.AdditionalToolAllowlists = map[string][]string{}
		}
		steps[step.ID] = step
	}

	// Zone 2 boundary fix (C10): detect parallel steps with overlay + same TargetKey.
	s.validateWorkspaceKeyCollisions(steps)

	graph := &schemas.TaskGraph{
		TaskID:    taskID,
		TraceID:   traceID,
		ProjectID: resolveProjectID(inputs),
		Steps:     steps,
		Status:    schemas.GraphRunning, SubmitterIsAdmin: anySubmitterIsAdmin(inputs),
		PendingDecisions: []*schemas.Decision{},
		OutputFiles:      []schemas.OutputFile{},
		CreatedAt:        time.Now(),
		ContextTree:      make(map[string]*schemas.ContextNode),
		SessionRoles:     make(map[string]any),
		SessionSkills:    make(map[string]any),
		SessionProviders: make(map[string]any),
		MainProviderID:   mainProviderID,
		GlobalWorkspace:  make(map[string]any),
		SessionID:        sessionID,
		OwnerID:          ownerID,
		WorkspaceRoot:    workspaceRoot,
		Delivery:         firstScheduleDelivery(inputs),
		TimeoutSecs:      timeoutSecs,
		TokenBudget:      tokenBudget,
	}

	// Input Seeding: copy declared input directories into task workspace.
	// No-op when WorkspaceRoot is empty or no steps declare InputDir.
	if err := s.seedTaskInputs(graph, s.outputBase); err != nil {
		s.logger.Log("EventInputSeedError", graph.TaskID, "", map[string]any{"error": err.Error()})
	}

	// Initialize Token Budget (ADDED 2026-08-30)
	s.Mu.RLock()
	if s.registry != nil {
		graph.TokenBudget = s.registry.System.DefaultTaskTokenBudget
	}
	s.Mu.RUnlock()

	// Task-Local Closure: snapshot JIT tools referenced by any step so
	// TTL expiry mid-task doesn't cause "tool not found". See
	// docs/completed/2026-09-15/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §2.1.
	if s.jit != nil {
		var allMCPs []string
		for _, inp := range inputs {
			allMCPs = append(allMCPs, inp.AdditionalMCPs...)
		}
		graph.LocalMCPs = s.jit.SnapshotJITTools(allMCPs)
	}

	applySessionAssets(graph, roles, skills, providers)
	s.resolveGraphVerifiers(graph)

	s.Mu.RLock()
	rr := s.registry.RequirePlanReview
	s.Mu.RUnlock()
	if rr {
		graph.Status = schemas.GraphPendingReview
	}

	// Initialize Root Node
	rootNode := &schemas.ContextNode{
		ID:     "root",
		Intent: "Initialization",
		Status: schemas.NodeActive,
	}
	if len(inputs) > 0 {
		rootNode.Intent = inputs[0].Task
		if len(rootNode.Intent) > 50 {
			rootNode.Intent = rootNode.Intent[:47] + "..."
		}
	}
	graph.ContextTree[rootNode.ID] = rootNode
	graph.CurrentNodeID = rootNode.ID

	// Async Embed root intent
	s.goBackground(func() { s.updateNodeEmbedding(taskID, rootNode.ID, rootNode.Intent) })

	s.Mu.Lock()
	s.graphs[taskID] = graph
	ctx, cancel := context.WithCancel(s.lifecycleCtx) // audit C2: derive from lifecycleCtx so Stop() cancels tasks
	s.cancelFuncs[taskID] = cancel
	s.doneChans[taskID] = make(chan struct{})
	s.Mu.Unlock()

	s.persistGraph(graph)
	s.pinBaseline(taskID, inputs)
	s.Broadcast()

	s.logger.Log(obs.EventTaskSubmitted, taskID, "", taskSubmittedDetail(len(steps), requireReview, graph))

	if !requireReview {
		// audit C1: startGraphExecutor handles UseSwarm branching.
		s.startGraphExecutor(ctx, taskID)
	}

	return taskID, nil
}
