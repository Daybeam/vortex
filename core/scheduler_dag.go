package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func (s *DirectedEngine) runInterceptors(step *schemas.Step, output *SpawnResult) {
	if step.Metadata == nil || step.Metadata.Namespace == "" {
		return
	}
	s.logger.Log("EventInterceptorTriggered", "", step.ID, map[string]any{
		"namespace": step.Metadata.Namespace,
		"message":   "Processing metadata interceptor",
	})
}

func (s *DirectedEngine) run(ctx context.Context, taskID string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		s.Mu.RLock()
		graph := s.graphs[taskID]
		if graph == nil {
			s.Mu.RUnlock()
			return
		}
		graphBlocked := graph.Status == schemas.GraphBlocked
		ready := graph.ReadySteps()
		s.Mu.RUnlock()

		if graphBlocked {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		if len(ready) == 0 {
			s.Mu.RLock()
			isTerminal := graph.IsTerminal()
			s.Mu.RUnlock()
			if isTerminal {
				s.finalize(graph)
				return
			}
			// Smart Routing Decision Node (ADDED 2026-08-27):
			// No ready steps but graph not terminal means downstream steps
			// are blocked on failed/skipped upstream artifacts. Instead of
			// sleeping forever (the legacy behavior), detect the specific
			// upstream that's insufficient and emit a decision for the
			// caller to resolve (skip, rerun upstream, abort).
			insufficient := s.detectUpstreamInsufficient(graph)
			if insufficient != nil {
				s.addDecision(graph, insufficient.Step,
					schemas.DecisionUpstreamInsufficient,
					map[string]any{
						"step_id":     insufficient.Step.ID,
						"upstream":    insufficient.UpstreamIDs,
						"upstream_st": insufficient.UpstreamStatuses,
					},
				[]string{"rerun_upstream", "skip", "abort"})
			s.setGraphStatus(graph, schemas.GraphBlocked)
			s.persistGraph(graph)
				s.Broadcast()
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		s.persistGraph(graph) // Checkpoint before starting ready steps

		// Phase 1: Sort by Effective Signal (descending)
		if s.SignalField != nil && len(ready) > 1 {
			signals := make([]float64, len(ready))
			for i, st := range ready {
				signals[i] = s.SignalField.GetEffectiveSignal(st.ID, 0.0)
			}
			sort.Slice(ready, func(i, j int) bool {
				return signals[i] > signals[j]
			})
		}

		// Phase 2: Concurrent Execution (FIXED 2026-08-16: Added Context-aware Wait)
		var wg sync.WaitGroup
		maxConcurrent := 10
		if s.registry != nil && s.registry.System.MaxConcurrentSteps > 0 {
			maxConcurrent = s.registry.System.MaxConcurrentSteps
		}
		sem := make(chan struct{}, maxConcurrent) // Configurable max concurrent steps per graph

		for _, step := range ready {
			s.Mu.RLock()
			graphRunning := graph.Status == schemas.GraphRunning
			s.Mu.RUnlock()
			if !graphRunning {
				break
			}
			wg.Add(1)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Done()
				return
			}
			go func(st *schemas.Step) {
				defer wg.Done()
				defer func() { <-sem }()
				s.executeStep(ctx, graph, st)
			}(step)
		}

		// Use a channel to wait for WaitGroup in a non-blocking way
		waitDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitDone)
		}()

		select {
		case <-waitDone:
			// Normal completion
		case <-ctx.Done():
			// Context canceled (e.g. session timeout or shutdown).
			// Wait for in-flight step goroutines with a grace period so they
			// can bail out cleanly instead of orphaning (audit H3).
			select {
			case <-waitDone:
			case <-time.After(5 * time.Second):
				// Grace period expired — steps hold a cancelled ctx and will
				// exit on their own; return to unblock the run loop.
			}
			return
		}
	}
}

// ─── Agent Pool Methods ───────────────────────────────────────────────────

func (s *DirectedEngine) GetAllReadySteps() []ReadyStepPair {
	return s.walker.GetAllReadySteps()
}

// ClaimStep performs a distributed lock check and marks a step as running.
// As of 2026-09-06, all step claiming is handled internally by
// DirectedEngine.run and executeStep; this method is exposed on the public
// interface but has no external callers. It remains for potential
// distributed task scheduling where multiple Orchestrator instances
// coordinate step ownership.
func (s *DirectedEngine) ClaimStep(taskID, stepID string) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	graph := s.graphs[taskID]
	if graph == nil || graph.Status != schemas.GraphRunning {
		return false
	}
	step := graph.Steps[stepID]
	if step == nil || step.Status != schemas.StepPending {
		return false
	}

	// Distributed Lock Check
	ok, _ := s.taskStore.Claim(s.lifecycleCtx, taskID, stepID)
	if !ok {
		return false
	}

	// Mark as claimed (running)
	step.Status = schemas.StepRunning
	return true
}

func (s *DirectedEngine) checkFinalize(graph *schemas.TaskGraph) {
	s.Mu.RLock()
	isTerm := graph.IsTerminal()
	status := graph.Status
	s.Mu.RUnlock()

	if isTerm && status == schemas.GraphRunning {
		s.finalize(graph)
	} else {
		s.Broadcast()
	}
}

// HandleSwarmOutput processes a result submitted by a remote swarm worker.
func (s *DirectedEngine) HandleSwarmOutput(taskID, stepID string, output *schemas.SubagentOutput) {
	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()

	if graph == nil {
		return
	}

	s.Mu.Lock()
	step, ok := graph.Steps[stepID]
	s.Mu.Unlock()

	if !ok {
		return
	}

	// Convert schemas.SubagentOutput to core.SpawnResult (internal type)
	res := &SpawnResult{
		Output: *output,
		Ref:    fmt.Sprintf("%s:%s", taskID, stepID),
	}

	s.handleOutput(graph, step, res)
}

func (s *DirectedEngine) ClaimTask(stepID string) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	// In this implementation, we assume stepID is unique across active graphs
	// or we find the first graph that has this pending step.
	var targetGraph *schemas.TaskGraph
	for _, g := range s.graphs {
		if g.Status != schemas.GraphRunning {
			continue
		}
		if _, ok := g.Steps[stepID]; ok {
			targetGraph = g
			break
		}
	}

	if targetGraph == nil {
		return false
	}

	step := targetGraph.Steps[stepID]
	if step.Status != schemas.StepPending {
		return false
	}

	// Distributed Lock Check
	ok, _ := s.taskStore.Claim(s.lifecycleCtx, targetGraph.TaskID, stepID)
	if !ok {
		return false
	}

	step.Status = schemas.StepRunning
	return true
}

func (s *DirectedEngine) ExecuteTask(ctx context.Context, stepID string) (*schemas.SubagentOutput, error) {
	s.Mu.RLock()
	var targetGraph *schemas.TaskGraph
	for _, g := range s.graphs {
		if _, ok := g.Steps[stepID]; ok {
			targetGraph = g
			break
		}
	}
	s.Mu.RUnlock()

	if targetGraph == nil {
		return nil, fmt.Errorf("task step %s not found", stepID)
	}

	step := targetGraph.Steps[stepID]

	// Delegate to the internal execution logic
	s.executeStep(ctx, targetGraph, step)

	// Retrieve result from store
	res, err := s.taskStore.Get(ctx, targetGraph.TaskID, stepID)
	if err != nil {
		return nil, err
	}

	// Convert StepResult to SubagentOutput for the provider
	data := make(map[string]any)
	if res.Data != nil {
		if m, ok := res.Data.(map[string]any); ok {
			data = m
		} else {
			data = map[string]any{"raw": res.Data}
		}
	}
	output := schemas.SubagentOutput{
		Status:         schemas.StatusOK,
		Confidence:     res.Confidence,
		Result:         data,
		MissingContext: res.MissingContext,
		Capability:     res.Capability,
	}

	return &output, nil
}

// ReadyStepPair binds a graph and its ready step.

// ReadyStepPair binds a graph and its ready step.
type ReadyStepPair struct {
	Graph *schemas.TaskGraph
	Step  *schemas.Step
}

func (s *DirectedEngine) executeStep(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step) {
	// ── Phase 0: Task-Local Closure Restore (ADDED 2026-09-15) ──────────
	// If JIT tools referenced by this task have expired globally (TTL GC),
	// re-register them from the task-local snapshot taken at submit time.
	// See docs/architecture/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §二.1.
	if s.jit != nil && len(graph.LocalMCPs) > 0 {
		if restored := s.jit.RestoreLocalMCPs(graph.LocalMCPs); len(restored) > 0 {
			s.logger.Log("JITClosureRestored", graph.TaskID, step.ID, map[string]any{
				"restored_ids": restored,
			})
		}
	}

	// ── Phase 0: Path-Level Mutex (ADDED 2026-09-13) ──────────────────────
	// If the step declares a TargetFile, acquire a write lock so concurrent
	// steps writing to the same path serialize. Different paths stay parallel.
	// See docs/completed/2026-09-13/PATH_LEVEL_MUTEX_CONCURRENCY_DESIGN.md
	if step.TargetFile != "" && s.pathLocks != nil {
		unlock := s.pathLocks.Acquire(step.TargetFile, true)
		defer unlock()
	}

	// ── Phase 0.1: Token Budget Pre-check (ADDED 2026-08-30) ─────────────
	// FIX (2026-08-31): removed the s.Mu.Lock()/Unlock() wrap around this
	// whole block. addDecision (invoked below via the CheckBudget error
	// path) internally calls broadcastDone, which itself takes s.Mu.Lock()
	// -- since Go's sync.RWMutex is not reentrant, holding s.Mu.Lock() here
	// and then calling addDecision would deadlock the engine's mutex the
	// first time a task's token budget was actually exceeded. Every other
	// addDecision call site in this file calls it without holding s.Mu,
	// which this now matches. The CheckBudget read is taken under a short
	// RLock instead, since graph.TokensUsed/TokenBudget are graph-level
	// counters written concurrently by every step of the same graph via
	// DeductBudget below (s.run's Phase 2 runs up to 10 steps of one graph
	// in parallel goroutines), unlike most other per-step fields in this
	// function which are only ever touched by their own owning goroutine.
	if s.budgetGuard != nil {
		s.Mu.RLock()
		budgetErr := s.budgetGuard.CheckBudget(graph)
		s.Mu.RUnlock()
		if budgetErr != nil {
			s.Mu.Lock()
			graph.BudgetPaused = true
			step.Status = schemas.StepBlocked
			s.Mu.Unlock()
			s.addDecision(graph, step, schemas.DecisionBudgetExhausted, map[string]any{
				"error":        budgetErr.Error(),
				"tokens_used":  graph.TokensUsed,
				"token_budget": graph.TokenBudget,
			}, []string{"resume", "abort"})
			return
		}
	}

	// ── Cost Governance: Budget progress sentinel (P1) ──────────────────
	// Design ref: docs/architecture/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §2.2 告警器2.
	// Soft alert at 60/80/90% + linear prediction from step completion ratio.
	// Never auto-extends budget. Prediction ≠ decision.
	//
	// RLock guard: graph.TokensUsed/TokenBudget/Steps are graph-level fields
	// written concurrently by parallel step goroutines (s.run Phase 2 runs up
	// to 10 steps of one graph in parallel). Snapshot under RLock, then release
	// before calling Observe/Log to avoid holding the lock during I/O (audit M4).
	if s.budgetSentinel != nil {
		s.Mu.RLock()
		tokensUsed := graph.TokensUsed
		tokenBudget := graph.TokenBudget
		taskID := graph.TaskID
		totalSteps, doneSteps := 0, 0
		for _, st := range graph.Steps {
			totalSteps++
			if st.Status == schemas.StepOK || st.Status == schemas.StepFailed {
				doneSteps++
			}
		}
		s.Mu.RUnlock()

		if tokenBudget > 0 && tokensUsed > 0 {
			pct := int(100 * tokensUsed / tokenBudget)
			predictedPct := 0
			if doneSteps > 0 && totalSteps > 0 {
				predictedPct = pct * totalSteps / doneSteps
			}
			if tier := s.budgetSentinel.Observe(taskID, pct, predictedPct); tier > 0 {
				s.logger.Log(EventBudgetPredictExceed, taskID, step.ID, map[string]any{
					"tier":          tier,
					"pct_used":      pct,
					"predicted_pct": predictedPct,
					"tokens_used":   tokensUsed,
					"token_budget":  tokenBudget,
					"steps_done":    doneSteps,
					"steps_total":   totalSteps,
				})
			}
		}
	}

	s.Mu.Lock()
	step.Status = schemas.StepRunning
	step.LastWorkedOn = time.Now()
	s.Mu.Unlock()
	s.persistGraph(graph) // Checkpoint state change
	taskID := graph.TaskID

	// ── StagedWorkspace pre-step snapshot (WIRED 2026-08-21) ────────────
	// Opt-in via system.staging_enabled (default false, zero behavior change
	// for existing configs). See docs/STAGED_WORKSPACE_DESIGN.md and
	// core/staged_workspace.go's own doc comment for scope/limitations.
	var stagedWS *StagedWorkspace
	var preSnapshot *SnapshotIndex
	if s.registry.System.StagingEnabled {
		stagedWS = NewStagedWorkspace(filepath.Join(s.outputBase, taskID))
		if snap, serr := stagedWS.TakePreStepSnapshot(step.ID); serr == nil {
			preSnapshot = snap
		} else {
			s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "pre_snapshot", "error": serr.Error()})
		}
	}

	s.registry.Mu.RLock()
	role := s.registry.Roles[step.RoleID]
	s.registry.Mu.RUnlock()

	for step.RetryCount <= step.MaxRetries {
		var dynTemp *float32
		if step.RetryCount > 0 {
			t := float32(0.2 + float64(step.RetryCount)*0.2)
			if t > 1.0 {
				t = 1.0
			}
			dynTemp = &t
		}

		// ── Phase 0.7: Tree-based Context Assembly ─────────────────────────────
		var treeHistory []map[string]any
		s.Mu.RLock()
		if g, ok := s.graphs[taskID]; ok {
			currID := g.CurrentNodeID
			for currID != "" {
				node := g.ContextTree[currID]
				if node == nil {
					break
				}
				nodeData := map[string]any{
					"id":      node.ID,
					"intent":  node.Intent,
					"summary": node.Summary,
					"status":  node.Status,
				}
				treeHistory = append(treeHistory, nodeData)
				currID = node.ParentID
			}
			for i, j := 0, len(treeHistory)-1; i < j; i, j = i+1, j-1 {
				treeHistory[i], treeHistory[j] = treeHistory[j], treeHistory[i]
			}
		}
		s.Mu.RUnlock()

		// Resolve InputMapping (ADDED 2026-08-17)
		resolvedInputs := make(map[string]any)
		if len(step.InputMapping) > 0 {
			// Phase 1: Under RLock, collect needed step IDs for DB prefetch
			s.Mu.RLock()
			neededSteps := make(map[string]bool)
			for _, path := range step.InputMapping {
				parts := strings.SplitN(path, ".", 2)
				if len(parts) == 2 {
					neededSteps[parts[0]] = true
				}
			}
			s.Mu.RUnlock()

			// Phase 2: Outside lock, pre-fetch step results from DB
			// (audit: was DB I/O under RLock, blocking all graph mutations)
			stepResults := make(map[string]*store.StepResult)
			for sid := range neededSteps {
				if res, err := s.taskStore.Get(ctx, taskID, sid); err == nil {
					stepResults[sid] = res
				}
			}

			// Phase 3: Under RLock, build resolvedInputs + record coordination edges
			s.Mu.RLock()
			for arg, path := range step.InputMapping {
				// Path format: step_id.field or just key (from GlobalWorkspace)
				parts := strings.SplitN(path, ".", 2)
				if len(parts) == 2 {
					// From another step's result (using pre-fetched data)
					if res, ok := stepResults[parts[0]]; ok && res.Data != nil {
						if m, ok := res.Data.(map[string]any); ok {
							val := m[parts[1]]
							resolvedInputs[arg] = val

							// ── Coordination Edge (arXiv:2608.16801) ──
							payloadSize := int64(0); if b, e := json.Marshal(val); e == nil { payloadSize = int64(len(b)) }
							s.recordCoordinationEdge(graph, parts[0], step.ID, "mapping", payloadSize)
						}
					}
				} else {
					// From GlobalWorkspace
					if val, ok := graph.GlobalWorkspace[path]; ok {
						resolvedInputs[arg] = val

						// ── Coordination Edge (arXiv:2608.16801) ──
						payloadSize := int64(0); if b, e := json.Marshal(val); e == nil { payloadSize = int64(len(b)) }
						s.recordCoordinationEdge(graph, "global_workspace", step.ID, "shared_vfs", payloadSize)
					}
				}
			}
			s.Mu.RUnlock()
		}

		// ── Coordination Edge for ContextRefs (arXiv:2608.16801) ──
		if len(step.ContextRefs) > 0 {
			for _, ref := range step.ContextRefs {
				parts := strings.SplitN(ref, ":", 2)
				if len(parts) == 2 {
					if res, err := s.taskStore.Get(ctx, taskID, parts[0]); err == nil && res.Data != nil {
						payloadSize := int64(0); if b, e := json.Marshal(res.Data); e == nil { payloadSize = int64(len(b)) }
						s.recordCoordinationEdge(graph, parts[0], step.ID, "reference", payloadSize)
					}
				}
			}
		}

		// ── Phase 0.8: Tracing & Spans ─────────────────────────────
		traceID := graph.TraceID
		if traceID == "" {
			traceID = NewTraceID()
		}
		spanID := NewSpanID()
		ctx = ContextWithTrace(ctx, traceID, spanID)

		result, err := s.spawner.Spawn(ctx, &SpawnRequest{
			TaskID:                   taskID,
			StepID:                   step.ID,
			TraceID:                  traceID,
			SpanID:                   spanID,
			RoleID:                   step.RoleID,
			Task:                     step.Task,
			AdditionalSkills:         step.AdditionalSkills,
			AdditionalMCPs:           step.AdditionalMCPs,
			AdditionalToolAllowlists: step.AdditionalToolAllowlists,
			ContextRefs:              step.ContextRefs,
			Temperature:              dynTemp,
			FrequencyPenalty:         nil,
			ProviderOverride:         step.ProviderOverride,
			RoutingMode:              step.RoutingMode,
			CompressionHint:          step.CompressionHint,
			TurnsBudgetBonus:         step.TurnsBudgetBonus,
			SessionRoot:              graph.WorkspaceRoot,
			Metadata: map[string]any{
				"context_tree_path": treeHistory,
			},
			Hub:                     NewContextHub(s.registry, graph, s.expStore),
			InputMapping:            resolvedInputs,
			Isolation:               step.Isolation,
			AdditionalPromptContext: step.AdditionalPromptContext,
		})

		if err == nil {
			// Record effective provider/model (ADDED 2026-07-24)
			step.ProviderID = result.ProviderID
			// FIX (2026-08-21, playbook follow-up): this block previously called
			// s.handleOutput(...) and returned immediately here on every successful
			// Spawn. That skipped the Sieve Guardian repetition/schema check, the
			// Context Tree transition logic, and the Phase 4 exit-criteria auto-
			// refine loop further down this same function -- none of which were
			// reachable from any error path either, making that code entirely dead
			// for the success case since whatever introduced this early return.
			// Removed the early return so execution now falls through to that
			// existing logic instead, matching what handleOutput/Sieve/exit-criteria
			// were actually written to handle. step.ModelID is set once, at its
			// original call site further down, to avoid a duplicate assignment.
		}

		if err != nil {
			// Check for Dynamic Escalation (Fork) signal
			if strings.Contains(err.Error(), "DYNAMIC_ESCALATION_REQUIRED") {
				s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
					"error":  "Dynamic Escalation Required",
					"reason": err.Error(),
				})
				step.Status = schemas.StepBlocked
				s.addDecision(graph, step, schemas.DecisionDelegationRequired, map[string]any{
					"error":           "Context overflow: task too complex for single model.",
					"action_required": "Please split this step into smaller sub-tasks.",
				}, []string{"skip", "abort"})
				return
			}

			// ── Max-Turns Suspension (ADDED 2026-09-07) ─────────────────
			// The Spawner's tool-execution loop hit maxTurns without a final
			// answer. Rather than hard-failing the step, suspend it as a
			// resumable pending decision so an operator or Main Agent can
			// resume (more turns), retry with context, or escalate the model.
			// Semantics mirror the DYNAMIC_ESCALATION_REQUIRED block above.
			if strings.Contains(err.Error(), ErrMaxTurnsExhausted) {
				step.Status = schemas.StepBlocked
				step.LastError = err.Error()
				s.logger.Log(EventDecisionRequired, taskID, step.ID, map[string]any{
					"reason":       "max_tool_turns_exhausted",
					"error":        err.Error(),
					"retry_count":  step.RetryCount,
					"resume_hint":  "choice 'retry' re-spawns this step from scratch; choose 'refine_and_retry' to inject additional_prompt_context carrying the partial progress",
				})
				s.addDecision(graph, step, schemas.DecisionMaxTurnsExhausted, map[string]any{
					"error":            err.Error(),
					"retry_count":      step.RetryCount,
					"action_required":  "Step exhausted its tool-turn budget before producing a final answer. Choose an option to proceed.",
					"partial_context":  "Prior tool interactions are recorded in this step's trace. Use refine_and_retry with feedback to carry that context forward.",
				}, []string{"resume_more_turns", "refine_and_retry", "escalate_model", "retry", "skip", "abort"})
				return
			}

			// ... existing error handling ...
			var rl *providers.RateLimitError
			if errors.As(err, &rl) {
				// FIX (2026-07-21): previously always used the fixed/role-configured
				// backoff schedule via retryWait, silently discarding rl.RetryAfter
				// even though it's parsed correctly one layer up (providers/provider.go's
				// parseRetryAfter). This could both under-wait (hammering the API again
				// before its own suggested cooldown elapsed, burning through retries/quota
				// faster) and over-wait (when the real RetryAfter was shorter than the
				// fixed schedule's next step). Now prefer the server-provided value when
				// present, falling back to the existing schedule when unknown (0).
				wait := s.retryWait(role, step.RetryCount)
				usedServerHint := false
				if rl.RetryAfter > 0 {
					wait = rl.RetryAfter
					// Defensive cap: don't let a misbehaving/huge Retry-After value stall
					// a step for an unreasonable amount of wall-clock time.
					if wait > 300 {
						wait = 300
					}
					usedServerHint = true
				}
				s.logger.Log(EventRateLimited, taskID, step.ID, map[string]any{
					"attempt": step.RetryCount, "wait_seconds": wait, "used_server_retry_after": usedServerHint,
					"provider_id": step.ProviderID, // ADDED (2026-08-16)
				})

				// Set global cooldown for this provider (ADDED 2026-08-16)
				if step.ProviderID != "" {
					s.registry.SetCooldown(step.ProviderID, time.Duration(wait)*time.Second)
				}

				step.RetryCount++
				if step.RetryCount > step.MaxRetries {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(wait) * time.Second):
				}
				continue
			}

			var br *providers.BadRequestError
			if errors.As(err, &br) {
				s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
					"error":   "Bad Request - triggering graceful degradation",
					"details": br.Msg,
				})
				step.Status = schemas.StepSkipped
				step.LastError = store.SanitizeError("degraded: " + br.Msg)
				return
			}

			step.LastError = store.SanitizeError(err.Error())
			if step.TriggerError == "" {
				step.TriggerError = step.LastError // Capture first failure signal (ADDED 2026-08-16)
			}

			// Circuit Breaker: Fail fast on authentication/permission errors
			if strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "PERMISSION_DENIED") || strings.Contains(err.Error(), "HTTP 401") {
				s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
					"error":   "Circuit Breaker triggered: Auth failure",
					"details": err.Error(),
				})
				break
			}

			// Pre-flight health check circuit breaker
			if strings.Contains(err.Error(), "missing dependency:") {
				s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
					"error":   "Environment check failed",
					"details": err.Error(),
				})
				step.Status = schemas.StepBlocked
				// FIX (2026-07-24): honor a step-level FailurePolicy override for
				// this failure class, if the caller declared one -- see
				// schemas/task.go's FailurePolicy type. A nil FailurePolicy (the
				// default) falls straight through to the unchanged decision_
				// required behavior below.
				if s.applyStepFailurePolicy(graph, step, taskID, FailureClassMissingDependency) {
					return
				}
				s.addDecision(graph, step, schemas.DecisionEnvironmentMissing, map[string]any{
					"missing_dependency": err.Error(),
				}, []string{"skip", "abort", "retry_with_skill:shell"})
				return
			}

			// Permanent Error Check: Role Not Found
			if strings.Contains(err.Error(), "role \"") && (strings.Contains(err.Error(), "\" not found") || strings.Contains(err.Error(), "generation failed") || strings.Contains(err.Error(), "generation is disabled")) {
				s.logger.Log(EventRoleMissing, taskID, step.ID, map[string]any{
					"error": err.Error(),
					"role":  step.RoleID,
				})
			// CRITICAL: Update both step and graph status to ensure complete stop
			s.setStepAndGraphStatus(step, schemas.StepBlocked, graph, schemas.GraphBlocked)

				// ACAIS: Deposit critical signal for role missing
				if s.SignalField != nil {
					s.SignalField.Deposit(types.LayerCritical, step.ID, 2.0)
				}

				// FIX (2026-07-24): honor a step-level FailurePolicy override for
				// this failure class, if the caller declared one -- see
				// schemas/task.go's FailurePolicy type. A nil FailurePolicy (the
				// default) falls straight through to the unchanged decision_
				// required behavior below. Note graph.Status was already set to
				// GraphBlocked above; applyStepFailurePolicy's "abort" case will
				// correctly override it to GraphFailed, matching what a caller
				// submitting choice="abort" against the decision below would
				// have produced.
				if s.applyStepFailurePolicy(graph, step, taskID, FailureClassRoleMissing) {
					s.persistGraph(graph)
					return
				}

				// NOTE: create_role is intentionally omitted from options.
				// Automatically enabling dynamic role generation is a security risk
				// (it globally flips EnableDynamicRoleGen and can persist LLM-authored
				// roles with AllowDynamicMCPs:true). Admins must use
				// orchestrator_invoke(subsystem="config", action="register_role") directly.
				s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
					"error":           "Role missing: " + err.Error(),
					"action_required": "Use orchestrator_invoke(subsystem=\"config\", action=\"register_role\") to create the role, then retry the task.",
				}, []string{"skip", "abort"})

				// FORCE PERSIST: Ensure Docker volumes see the blocked status immediately
				s.persistGraph(graph)
				return
			}

			// Permanent Error Check: Skill Not Found
			if strings.Contains(err.Error(), "skill \"") && strings.Contains(err.Error(), "\" not found") {
				s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
					"error": err.Error(),
					"role":  step.RoleID,
			})
			// Race fix: protect graph.Status with Mu.
			s.setStepAndGraphStatus(step, schemas.StepBlocked, graph, schemas.GraphBlocked)

			s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
					"error":           "Infrastructure failure: " + err.Error(),
					"action_required": "Please register the missing skill or remove it from the role's bound_skills",
				}, []string{"create_skill", "retry", "skip", "abort"})

				s.persistGraph(graph)
				return
			}

			step.RetryCount++
			s.logger.Log(EventStepRetrying, taskID, step.ID, map[string]any{
				"attempt": step.RetryCount, "error": err.Error(),
			})
			if step.RetryCount > step.MaxRetries {
				break
			}
			// Exponential Backoff: base 2s * 2^attempt
			wait := time.Duration(math.Pow(2, float64(step.RetryCount))) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}

		// ── Sieve: Guardian Check ──────────────────────────────────────────
		// Use Sieve to check for repetition or schema violations
		outputText := fmt.Sprintf("%v", result.Output.Result)
		requiredSchema := ""
		if step.ExitCriteria == "json" {
			requiredSchema = "json"
		}

		if valid, reason := s.sieve.Inspect(outputText, requiredSchema); !valid {
			s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
				"error":  "Sieve Guardian intercepted",
				"reason": reason,
			})
			step.LastError = store.SanitizeError("sieve_intercepted: " + reason)
			step.RetryCount++
			if step.RetryCount > step.MaxRetries {
				break
			}
			// Adjust parameters for retry to break loops
			dynTemp = setFloat32(0.8)

			// Trigger immediate retry without backoff
			continue
		}

		step.ModelID = result.ModelID
		s.handleOutput(graph, step, result)

		// ── StagedWorkspace post-step snapshot (success, WIRED 2026-08-21) ──
		var postSnapshot *SnapshotIndex
		if stagedWS != nil {
			if entry, post, serr := stagedWS.RecordPostStepSnapshot(taskID, step.ID, "completed", preSnapshot); serr != nil {
				s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "post_snapshot", "error": serr.Error()})
			} else {
				postSnapshot = post
				s.logger.Log("EventStagingSnapshotRecorded", taskID, step.ID, map[string]any{"diff_summary": entry.DiffSummary})
			}
		}

		// ── Read-Dependency Detection (WIRED 2026-08-21, see detectReadDeps'
		// own doc comment above for the full history) ───────────────────────
		// handleOutput's call chain persists this step's trace to the task
		// store (see doSpawn's taskStoreSet call in spawner.go), so it is
		// available here immediately after handleOutput returns.
		if stepRes, terr := s.taskStore.Get(ctx, taskID, step.ID); terr == nil && stepRes != nil {
			if deps := s.detectReadDeps(graph, step.ID, stepRes.Trace, postSnapshot); len(deps) > 0 {
				step.ReadDeps = deps
			}
		}

		// ── JITSessions cleanup (success, WIRED 2026-08-25) ─────────────────
		if s.JITSessions != nil {
			s.JITSessions.CloseSessionsForStep(taskID, step.ID)
		}

		// ── Phase 3.5: Context Tree Transition ───────────────────────────────
		s.Mu.Lock()
		currNode := graph.ContextTree[graph.CurrentNodeID]
		if currNode != nil {
			currNode.StepIDs = append(currNode.StepIDs, step.ID)

			// Detect Branch Point
			if step.Status == schemas.StepOK {
				needsNewNode := false
				reason := result.Output.Assumptions
				if len(reason) > 0 && strings.Contains(strings.Join(reason, " "), "new task") {
					needsNewNode = true
				}

				// Semantic Check (Semantic Pull)
				if !needsNewNode && len(currNode.Embedding) > 0 {
					// In a real execution, we would embed the result here
					// For now, we rely on the heuristic or trigger async embed
				}

				if needsNewNode {
					newNodeID := fmt.Sprintf("node_%s", uuid.New().String()[:6])
					newNode := &schemas.ContextNode{
						ID:       newNodeID,
						ParentID: currNode.ID,
						Intent:   "Adaptive Transition from " + step.ID,
						Status:   schemas.NodeActive,
						Metadata: make(map[string]any),
					}

					// Inherit root metadata (Behavior Contract requirement)
					for k, v := range currNode.Metadata {
						newNode.Metadata[k] = v
					}
					// Note: LocalSymbolIndex is intentionally left empty/nil to isolate interference.

					graph.ContextTree[newNodeID] = newNode
					graph.CurrentNodeID = newNodeID

				// Async Embedding of new intent
				s.goBackground(func() { s.updateNodeEmbedding(graph.TaskID, newNodeID, newNode.Intent) })

				// Async Folding of previous node
				s.goBackground(func() { s.foldNode(graph.TaskID, currNode.ID) })
				}
			}
		}
		s.Mu.Unlock()

		// ACAIS: Signal field deposit is handled in handleOutput based on quality.

		// ── Phase 4: Auto-Audit (Conditional Exit) ───────────────────────────
		if step.Status == schemas.StepOK && step.ExitCriteria != "" {
			if ok, reason, failType := s.verifyExitCriteria(ctx, graph, step, result); !ok {
				if step.AutoRefineCount < step.MaxAutoRefine {
					step.AutoRefineCount++
					step.Status = schemas.StepPending
					step.LastError = store.SanitizeError(fmt.Sprintf("[%s] Exit criteria not met: %s. Retrying...", failType, reason))
					s.logger.Log(EventStepRetrying, taskID, step.ID, map[string]any{
						"reason": "exit_criteria_fail", "details": reason, "refine_attempt": step.AutoRefineCount, "fail_type": failType,
					})

					// Specialized recovery prompts (Branching logic)
					recoveryAdvice := ""
					switch failType {
					case schemas.FailurePermission:
						recoveryAdvice = "It seems like a permission issue. Please check if you need to use a different tool or request elevated access."
					case schemas.FailureTimeout:
						recoveryAdvice = "The operation timed out. Consider breaking the task into smaller sub-tasks, increasing efficiency, or using a more performant tool if available."
					case schemas.FailureConflict:
						recoveryAdvice = "A state conflict (e.g. file modified by another process) was detected. Please RE-READ the current state (file or UI) before attempting to write again to ensure your changes are based on the latest version."
					case schemas.FailureSchemaViolation:
						recoveryAdvice = "Your output violates the required schema. Please strictly follow the formatting instructions provided in the tool definition or task description."
					default:
						recoveryAdvice = "Your output failed logical or semantic verification. Please carefully review the 'Reason' above and adjust your approach accordingly."
					}

					// Adjust prompt for refinement
					step.Task = fmt.Sprintf("%s\n\n[REFINEMENT REQUIRED]\nYour previous output failed verification.\nFailure Category: %s\nReason: %s\nAdvice: %s", step.Task, failType, reason, recoveryAdvice)
					continue
				} else {
					step.Status = schemas.StepBlocked
					s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
						"error":  "Failed exit criteria after max refinements",
						"reason": reason,
					}, []string{"skip", "abort", "retry"})
				}
			}
		}

		s.runInterceptors(step, result)
		s.persistGraph(graph)
		return
	}

	// Exhausted retries
	step.Status = schemas.StepFailed
	s.logger.Log(EventStepFailed, taskID, step.ID, map[string]any{
		"last_error": step.LastError,
	})

	// ── StagedWorkspace post-step snapshot + rollback (failure, WIRED 2026-08-21) ──
	if stagedWS != nil {
		if _, _, serr := stagedWS.RecordPostStepSnapshot(taskID, step.ID, "failed", preSnapshot); serr != nil {
			s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "post_snapshot_failed", "error": serr.Error()})
		}
		if preSnapshot != nil {
			if rerr := stagedWS.Rollback(preSnapshot); rerr != nil {
				s.logger.Log("EventStagingError", taskID, step.ID, map[string]any{"phase": "rollback", "error": rerr.Error()})
			} else {
				_ = stagedWS.MarkRolledBack(step.ID)
				s.logger.Log("EventStagingRolledBack", taskID, step.ID, nil)
			}
		}
	}

	// ── JITSessions cleanup (failure, WIRED 2026-08-25) ─────────────────
	if s.JITSessions != nil {
		s.JITSessions.CloseSessionsForStep(taskID, step.ID)
	}

	s.maybeBlock(graph, step)
}
