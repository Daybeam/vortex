package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func (s *DirectedEngine) validateTaskComplexity(inputs []schemas.StepInput) error {
	if len(inputs) == 0 {
		return errors.New("steps is required")
	}

	// Rule: Reject tasks that are just a single "local" step with no specialized skills.
	// These are high-overhead and should be done directly by the caller.
	if len(inputs) == 1 {
		inp := inputs[0]
		s.Mu.RLock()
		role := s.registry.Roles[inp.RoleID]
		s.Mu.RUnlock()

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
	return s.SubmitWithSessionIR(inputs, nil, nil, nil, "", "", "", 0, 0)
}

func (s *DirectedEngine) SubmitWithSessionIR(inputs []schemas.StepInput, roles []*config.Role, skills []*config.Skill, providers []*config.ProviderConfig, mainProviderID string, sessionID string, workspaceRoot string, timeoutSecs int, tokenBudget int64) (string, error) {
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

	// ── Pre-check: Complexity and Relevance ──────────────────────────
	if err := s.validateTaskComplexity(inputs); err != nil {
		return "", err
	}

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

		ctx := ContextWithTrace(context.Background(), traceID, NewSpanID())

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
			ID:               stepID,
			RoleID:           inp.RoleID,
			Task:             inp.Task,
			Status:           schemas.StepPending,
			AdditionalSkills: inp.AdditionalSkills,
			AdditionalMCPs:   inp.AdditionalMCPs,
			ContextRefs:      inp.ContextRefs,
			Deliver:          inp.Deliver,
			ExitCriteria:     inp.ExitCriteria,
			VerifierModel:    inp.VerifierModel,
			MaxAutoRefine:    inp.MaxAutoRefine,
			EnableDebate:     inp.EnableDebate,
			MaxDebateRounds:  inp.MaxDebateRounds,
			SpawnDepth:       inp.SpawnDepth,
			MaxSpawnDepth:    inp.MaxSpawnDepth,
			AutoRefineCount:  0,
			LastWorkedOn:     time.Now(),
			InputDir:         inp.InputDir,
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
			TaskID:           taskID,
			TraceID:          traceID,
			Steps:            map[string]*schemas.Step{stepID: step},
			Status:           schemas.GraphRunning,
			PendingDecisions: []*schemas.Decision{},
			OutputFiles:      []schemas.OutputFile{},
			CreatedAt:        time.Now(),
			ContextTree:      map[string]*schemas.ContextNode{},
			SessionRoles:     map[string]any{},
			SessionSkills:    map[string]any{},
			GlobalWorkspace:  map[string]any{},
			IsSmartRouted:    true,
			SessionID:        sessionID,
			WorkspaceRoot:    workspaceRoot,
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
		// docs/architecture/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §二.1.
		if s.jit != nil {
			graph.LocalMCPs = s.jit.SnapshotJITTools(inp.AdditionalMCPs)
		}

		// Input Seeding: copy declared input directories into task workspace.
		if err := s.seedTaskInputs(graph, s.outputBase); err != nil {
			s.logger.Log("EventInputSeedError", graph.TaskID, "", map[string]any{"error": err.Error()})
		}

		s.Mu.Lock()
		s.graphs[taskID] = graph
		ctx, cancel := context.WithCancel(s.lifecycleCtx) // audit C2: derive from lifecycleCtx so Stop() cancels tasks
		s.cancelFuncs[taskID] = cancel
		s.doneChans[taskID] = make(chan struct{})
		s.Mu.Unlock()

		s.persistGraph(graph) // Re-enabled: needed for task visibility after restart (loadGraphs)
		s.Broadcast()

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
					s.logger.Log("EventFastPathHit", taskID, stepID, map[string]any{
						"path": inp.OutputContract.Path,
					})
				} else {
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

					// Audit gate. Only runs when the step completed OK and declared criteria.
					if err == nil && result != nil && step.ExitCriteria != "" &&
						(result.Output.Status == schemas.StatusOK || result.Output.Status == schemas.StatusPartial) {
					if ok, reason, failType := s.verifyExitCriteria(ctx, graph, step, result); !ok {
						if step.AutoRefineCount < step.MaxAutoRefine {
							step.AutoRefineCount++
							step.LastError = store.SanitizeError(fmt.Sprintf("[%s] Exit criteria not met: %s. Refining...", failType, reason))
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
			if finalErr != nil {
				step.Status = schemas.StepFailed
				step.LastError = finalErr.Error()
				graph.Status = schemas.GraphFailed
			} else if finalResult != nil {
				if finalResult.Output.Status == schemas.StatusDelegationRequired {
					conf := finalResult.Output.Confidence
					step.Confidence = &conf
					step.Status = schemas.StepBlocked
					delegationBlocked = true
				} else {
					conf := finalResult.Output.Confidence
					step.Confidence = &conf
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
				}
			} else {
				step.Status = schemas.StepFailed
				step.LastError = "spawn returned nil result"
				graph.Status = schemas.GraphFailed
			}
			s.Mu.Unlock()

			if delegationBlocked {
				s.addDecision(graph, step, schemas.DecisionDelegationRequired, map[string]any{
					"prompt":  finalResult.Output.Result,
					"role_id": step.RoleID,
				}, []string{"fulfill", "skip", "abort"})
				s.Broadcast()
				return
			}

			s.persistGraph(graph)
			s.broadcastDone(taskID) // safe close under Mu with double-close guard
			s.Broadcast()
		}) // audit C1: goBackground closing

		return taskID, nil
	}

	taskID := schemas.NewTaskID()
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
			VerifierModel:            inp.VerifierModel,
			MaxAutoRefine:            inp.MaxAutoRefine,
			EnableDebate:             inp.EnableDebate,
			MaxDebateRounds:          inp.MaxDebateRounds,
			OutputContract:           inp.OutputContract,
			Deliver:                  inp.Deliver, // ADDED 2026-08-30
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
		TaskID:           taskID,
		TraceID:          traceID,
		Steps:            steps,
		Status:           schemas.GraphRunning,
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
		WorkspaceRoot:    workspaceRoot,
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
	// docs/architecture/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §二.1.
	if s.jit != nil {
		var allMCPs []string
		for _, inp := range inputs {
			allMCPs = append(allMCPs, inp.AdditionalMCPs...)
		}
		graph.LocalMCPs = s.jit.SnapshotJITTools(allMCPs)
	}

	for _, r := range roles {
		if r != nil {
			graph.SessionRoles[r.ID] = r
		}
	}
	for _, sk := range skills {
		if sk != nil {
			graph.SessionSkills[sk.ID] = sk
		}
	}
	for _, p := range providers {
		if p != nil {
			// Use PoolID or provider name as session-provider key
			id := p.PoolID
			if id == "" {
				id = p.Provider
			}
			graph.SessionProviders[id] = p
		}
	}

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
	s.Broadcast()

	s.logger.Log(EventTaskSubmitted, taskID, "", map[string]any{
		"step_count":     len(steps),
		"require_review": requireReview,
	})

	if !s.UseSwarm && !requireReview {
		// Each task graph runs in its own goroutine — no asyncio needed
		s.goBackground(func() { s.run(ctx, taskID) }) // audit C1: use goBackground so Stop() drains
	} else if s.UseSwarm && !requireReview {
		s.goBackground(func() { s.swarmWatchdog(ctx, taskID) }) // audit C1: use goBackground so Stop() drains
	}

	return taskID, nil
}

// isDeterministicContract checks if a step has a deterministic OutputContract
// that can be validated by FastPathEngine without LLM overhead.
// Requires a Path and at least one of RequiredFields/TypeSchema.
func isDeterministicContract(inp schemas.StepInput) bool {
	c := inp.OutputContract
	return c.Path != "" && (len(c.RequiredFields) > 0 || len(c.TypeSchema) > 0)
}

// tryFastPath attempts deterministic contract validation via FastPathEngine.
// On success, returns a synthesized SpawnResult with StatusOK and Confidence 1.0.
// On failure, the caller should fall back to spawner.Spawn.
func (s *DirectedEngine) tryFastPath(ctx context.Context, inp schemas.StepInput, taskID, stepID string) (*SpawnResult, error) {
	engine := NewFastPathEngine(s.outputBase)
	result, err := engine.ExecuteFastPath(ctx, inp.OutputContract, nil)
	if err != nil {
		return nil, err
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		resultMap = map[string]any{"fastpath_result": result}
	}
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 1.0,
			Result:     resultMap,
		},
		Ref: "fastpath:" + inp.OutputContract.Path,
	}, nil
}
