package core

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/schemas"
	"github.com/google/uuid"
)

func (s *DirectedEngine) handleOutput(
	graph *schemas.TaskGraph,
	step *schemas.Step,
	result *SpawnResult,
) {
	// ── Phase 0.1: Token Budget Deduction (ADDED 2026-08-30) ─────────────
	if result != nil && result.Output.Usage != nil && result.Output.Usage.TotalTokens > 0 && s.budgetGuard != nil {
		// FIX (2026-08-31): DeductBudget mutates graph.TokensUsed/BudgetPaused,
		// graph-level counters shared across every concurrently-running step
		// of this same graph -- see the matching fix + comment on the
		// CheckBudget call site in executeStep above. Take a short write
		// lock around just this mutation.
		s.Mu.Lock()
		s.budgetGuard.DeductBudget(graph, int64(result.Output.Usage.TotalTokens))
		s.Mu.Unlock()
	}

	taskID := graph.TaskID
	output := result.Output
	conf := output.Confidence
	s.Mu.Lock()
	step.Confidence = &conf
	step.MissingCtx = output.MissingContext
	step.StatesVisited = result.StatesVisited // PGPO (ADDED 2026-09-08)
	s.Mu.Unlock()

	switch {
	case output.Status == schemas.StatusOK && conf >= s.registry.System.ConfidenceThreshold:
		s.Mu.Lock()
		step.Status = schemas.StepOK
		// FIX (2026-07-03): a step that failed once and then succeeded on
		// retry previously kept showing the stale error from the earlier
		// failed attempt forever (LastError was only ever set, never
		// cleared on success) -- get_task_status/get_config would report a
		// completed step alongside a misleading last_error string. Clear it
		// here since this step's current, authoritative status is success.
		step.LastError = ""
		step.ResultRef = result.Ref
		s.Mu.Unlock()
		s.logger.LogWithConfidence(EventStepCompleted, taskID, step.ID, conf, nil)
		s.publishEvent(taskID, step.ID, EventStepCompleted, map[string]any{"confidence": conf})

		// ── Experience Evolution (ADDED 2026-09-08) ──
		if step.TriggerError != "" && step.AdditionalPromptContext != "" {
			s.DepositExperienceEvolution(s.lifecycleCtx, graph, step) // audit H7: use lifecycleCtx, not context.Background()
		}

		// Async Embedding of step result (ADDED 2026-07-26)
		s.goBackground(func() { s.updateStepEmbedding(graph.TaskID, step.ID, step.Task) }) // audit H5: use goBackground so Stop() drains

		// ── EvoX Programmatic Merge (ADDED 2026-08-17) ───────────────────────
		if step.MergeStrategy != "" && step.TargetKey != "" {
			s.Mu.Lock()
			if graph.GlobalWorkspace == nil {
				graph.GlobalWorkspace = make(map[string]any)
			}

			val := output.Result
			// If result contains a specific field matching TargetKey, use it, else use full result
			if specific, ok := output.Result[step.TargetKey]; ok {
				val = map[string]any{step.TargetKey: specific}
			}

			switch step.MergeStrategy {
			case "append":
				existing, _ := graph.GlobalWorkspace[step.TargetKey].([]any)
				graph.GlobalWorkspace[step.TargetKey] = append(existing, val)
			case "unique":
				// Simplified unique check for strings/scalars
				existing, _ := graph.GlobalWorkspace[step.TargetKey].([]any)
				found := false
				strVal := fmt.Sprintf("%v", val)
				for _, v := range existing {
					if fmt.Sprintf("%v", v) == strVal {
						found = true
						break
					}
				}
				if !found {
					graph.GlobalWorkspace[step.TargetKey] = append(existing, val)
				}
			case "overlay":
				graph.GlobalWorkspace[step.TargetKey] = val
			}
			s.Mu.Unlock()

			// ── Coordination Edge (arXiv:2608.16801) ──
			payloadSize := int64(len(fmt.Sprintf("%v", val)))
			s.recordCoordinationEdge(graph, step.ID, "global_workspace", "shared_vfs", payloadSize)

			s.logger.Log("EventProgrammaticMerge", graph.TaskID, step.ID, map[string]any{
				"strategy": step.MergeStrategy,
				"key":      step.TargetKey,
			})
		}

		// ── Auto-Deposition (ADDED 2026-08-31) ──────────────────────────────
		// After explicit EvoX merge (if any), automatically deposit the step's
		// result into GlobalWorkspace under stepID and "result" keys. This
		// ensures wait_task returns deliverable data even when the Main Agent
		// didn't configure TargetKey/MergeStrategy — zero cognitive burden.
		s.Mu.Lock()
		AutoDepositResult(graph, step.ID, output.Result)
		s.Mu.Unlock()
		s.logger.Log("EventAutoDeposit", graph.TaskID, step.ID, nil)

		// ── Context Archive Ingestion (ADDED 2026-09-13) ──────────────────
		if s.Archive != nil {
			s.archiveStep(graph, step, output)
		}

		// ACAIS: Deposit signal upon successful step completion
		if s.SignalField != nil {
			intensity := s.SignalField.BaseIntensity * conf
			s.SignalField.Deposit(types.LayerCritical, step.ID, intensity)

			// ACAIS: Dispersion - strengthen signal for dependents to guide swarm
			for _, other := range graph.Steps {
				for _, dep := range other.DependsOn {
					if dep == step.ID && other.Status == schemas.StepPending {
						s.SignalField.Deposit(types.LayerDependency, other.ID, intensity*0.5)
					}
				}
			}
		}

		if ref, ok := output.Result["output_ref"].(string); ok && ref != "" {
			summary, _ := output.Result["summary"].(string)
			format, _ := output.Result["format"].(string)
			sizeBytes, _ := output.Result["size_bytes"].(float64)
			s.Mu.Lock()
			graph.OutputFiles = append(graph.OutputFiles, schemas.OutputFile{
				StepID:    step.ID,
				Path:      ref,
				Format:    format,
				Summary:   summary,
				SizeBytes: int(sizeBytes),
				IsPrimary: true,
				Deliver:   step.Deliver, // ADDED 2026-08-30
				CreatedAt: time.Now(),
			})
			s.Mu.Unlock()
		}

		// --- Side-loading Optimization ---
		// If the result JSON itself is very large, side-load it and keep Task object light.
		resultJSON, _ := json.Marshal(output.Result)
		assetFile, err := s.assets.Handle(graph.TaskID, step.ID, resultJSON, "json", "working")
		if err == nil && assetFile != nil {
			// Asset was side-loaded
			assetFile.Summary = "Side-loaded large JSON result"
			s.Mu.Lock()
			graph.OutputFiles = append(graph.OutputFiles, *assetFile)

			// Replace step result with a lightweight reference
			step.ResultRef = assetFile.Path
			s.Mu.Unlock()
			// Optional: Clear heavy data from memory if needed
		}
		// ---------------------------------

	case output.Status == schemas.StatusDelegationRequired:
		s.Mu.Lock()
		step.Status = schemas.StepBlocked
		s.Mu.Unlock()
		s.addDecision(graph, step, schemas.DecisionDelegationRequired, map[string]any{
			"prompt":  output.Result, // Contains system_prompt and user_prompt from Spawner
			"role_id": step.RoleID,
		}, []string{"fulfill", "skip", "abort"})

	case output.Status == schemas.StatusCapabilityRequired || output.Status == schemas.StatusPartial || conf < s.registry.System.ConfidenceThreshold:
		// ── Auto-Deposition (ADDED 2026-09-07) ──────────────────────────────
		// Ensure partial results are synchronized for REPL/Next-step visibility
		// Mechanism 1: Micro-Step & REPL State Machine
		s.Mu.Lock()
		AutoDepositResult(graph, step.ID, output.Result)
		s.Mu.Unlock()

		// ── Context Archive Ingestion (ADDED 2026-09-13) ──────────────────
		if s.Archive != nil {
			s.archiveStep(graph, step, output)
		}

		// ── ODFTP Triage Engine (ADDED 2026-08-30) ──────────────────────────
		cause, suggestion, details := s.diagnoseFault(&output, step)

		// ── Cycle Breaker (ADDED 2026-09-16) ──────────────────────────────
		// Before ASAE spawning, try upward backtracking: if the downstream
		// step reports MissingContext and has upstream deps, re-run the
		// upstream with feedback instead of spawning new work. Fuses after
		// MaxLoopRounds to prevent weak-model death loops.
		// See docs/architecture/CYCLE_BREAKER_DESIGN.md.
		if s.tryBacktrack(graph, step, &output) {
			return
		}

		// ── ASAE: Autonomous Self-Spawning Engine (ADDED 2026-09-09) ───────
		// Before blocking, check if we can autonomously spawn a sub-step to
		// address missing context or low confidence.
		maxSpawn := s.registry.System.MaxSpawnDepth
		if step.MaxSpawnDepth > 0 {
			maxSpawn = step.MaxSpawnDepth
		}

		if step.SpawnDepth < maxSpawn && (cause == FailureClassContextDeficit || cause == FailureClassGenerativeUncertainty) {
			s.Mu.Lock()
			subStepID := fmt.Sprintf("%s_spawn_%s", step.ID, uuid.New().String()[:4])
			refinedTask := fmt.Sprintf("Autonomous Follow-up Task: Address missing context/uncertainty: %v. Original task: %s",
				output.MissingContext, step.Task)

			surgery := SurgeryData{
				DeprecatedStepID: step.ID,
				ReplacementSteps: []schemas.StepInput{
					{
						ID:            subStepID,
						RoleID:        step.RoleID,
						Task:          refinedTask,
						SpawnDepth:    step.SpawnDepth + 1,
						MaxSpawnDepth: maxSpawn,
					},
				},
			}

			if err := s.mutateGraphTopologyLocked(graph, surgery); err == nil {
				s.Mu.Unlock()
				s.logger.Log("EventAutonomousStepSpawned", taskID, subStepID, map[string]any{
					"parent_step":     step.ID,
					"missing_context": output.MissingContext,
					"spawn_depth":     step.SpawnDepth + 1,
				})
				return // Success: auto-swapped, do not block
			}
			s.Mu.Unlock()
		}

		// ── FallbackFor re-wire (ADDED 2026-09-05) ────────────────────────────
		// Before blocking, check if a fallback step exists for this step.
		// If it does, activate it instead of creating a decision. This restores
		// the proactive degradation semantics that were lost in the 08-30/08-31
		// ODFTP rewrite. The fallback step runs as a normal pending step, with
		// its own role/task/model — the caller doesn't need to make a decision.
		fallback := graph.FallbackFor(step.ID)
		if fallback != nil && fallback.Status == schemas.StepPending {
			// Mark the current step as failed-with-fallback (not blocked)
			s.Mu.Lock()
			step.Status = schemas.StepFailed
			step.LastError = fmt.Sprintf("%s: confidence %.2f, root cause: %s", output.Status, conf, string(cause))
			// Activate the fallback step
			fallback.Status = schemas.StepPending // already pending, but explicit
			s.Mu.Unlock()
			s.logger.Log(EventFallbackActivated, taskID, step.ID, map[string]any{
				"fallback_step": fallback.ID,
				"root_cause":    string(cause),
				"confidence":    conf,
			})
			return // Do not create a decision — fallback handles it
		}

		// No fallback available — proceed with decision blocking as before
		s.Mu.Lock()
		step.Status = schemas.StepBlocked
		s.Mu.Unlock()
		s.publishEvent(taskID, step.ID, EventStepFailed, map[string]any{"root_cause": string(cause), "blocked": true})

		if s.SignalField != nil {
			s.SignalField.Deposit(types.LayerCritical, step.ID, 1.5)
		}

		options := []string{"skip", "abort", "retry", "refine_and_retry"}
		if cause == FailureClassCapabilityRequired {
			// ── SelectSkill UCB1 re-wire (ADDED 2026-09-05) ─────────────────
			// Before the ODFTP rewrite, retry_with_skill options were built by
			// hard-coding each RequiredCapability into a "retry_with_skill:<cap>"
			// option with no ranking. This bypassed the UCB1 bandit entirely.
			// Now: for each required capability, query SkillAffinities for ranked
			// candidates, then let SelectSkill's UCB1 pick the best one. This
			// restores the experience-learning feedback loop.
			ctx := s.lifecycleCtx // audit L6: use lifecycleCtx so queries cancel on shutdown
			if s.expStore != nil {
				for _, cap := range details {
					// Get ranked candidates from learned affinities
					recommended := s.expStore.QuerySkillRecommendations(ctx, cap, 0.0)
					if len(recommended) > 0 {
						// UCB1 picks the best candidate (exploit + explore)
						best := s.expStore.SelectSkill(ctx, recommended, cap, step.RoleID, "", 0.1)
						if best != "" {
							options = append(options, "retry_with_skill:"+best)
						}
					}
					// Always also offer the raw capability name as a last resort
					// (may not have learned affinities yet — cold start)
					options = append(options, "retry_with_skill:"+cap)
				}
			} else {
				// No experience store — fall back to old behavior
				for _, cap := range details {
					options = append(options, "retry_with_skill:"+cap)
				}
			}
		}
		if cause == FailureClassContextDeficit {
			options = append(options, "retry_with_context") // Caller will provide context in SubmitDecision
		}
		options = append(options, "escalate_model")

		s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
			"root_cause":       string(cause),
			"suggested_action": suggestion,
			"missing_details":  details,
			"confidence":       conf,
			"missing_context":  output.MissingContext,
		}, options)

	default:
		step.Status = schemas.StepFailed
		s.maybeBlock(graph, step)
	}
}
