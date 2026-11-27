package core

import (
	"context"
	"strings"
	"time"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// executeStep owns the path lock and retry lifecycle. Phase implementations are
// split into cohesive companions without changing state, security, or event
// ordering.
func (s *DirectedEngine) executeStep(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step) {
	s.restoreTaskLocalClosure(graph, step)

	// Serialize steps declaring the same target path; different paths stay parallel.
	if step.TargetFile != "" && s.pathLocks != nil {
		unlock := s.pathLocks.Acquire(step.TargetFile, true)
		defer unlock()
	}
	if !s.precheckStepBudget(graph, step) {
		return
	}
	s.observeBudgetSentinel(graph, step)

	s.Mu.Lock()
	step.Status = schemas.StepRunning
	step.LastWorkedOn = time.Now()
	s.Mu.Unlock()
	s.persistGraph(graph) // Checkpoint state change
	stagedWS, preSnapshot := s.beginStagedStep(graph, step)

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
		req, spawnCtx := s.buildStepSpawnRequest(ctx, graph, step, dynTemp)
		ctx = spawnCtx
		result, err := s.spawner.Spawn(ctx, req)
		if err != nil {
			switch s.handleStepSpawnError(ctx, graph, step, role, err) {
			case stepErrorReturn:
				return
			case stepErrorRetry:
				continue
			case stepErrorExhausted:
				s.finishExhaustedStep(graph, step, stagedWS, preSnapshot)
				return
			}
		}

		s.Mu.Lock()
		step.ProviderID = result.ProviderID
		s.Mu.Unlock()
		switch s.finishSuccessfulStep(ctx, graph, step, result, stagedWS, preSnapshot) {
		case stepSuccessRetry:
			continue
		case stepSuccessReturn:
			return
		case stepSuccessExhausted:
			s.finishExhaustedStep(graph, step, stagedWS, preSnapshot)
			return
		default:
			return
		}
	}
	s.finishExhaustedStep(graph, step, stagedWS, preSnapshot)
}

// resolveStepInputs resolves step.InputMapping into the argument values handed
// to the spawner and records a coordination edge per mapped value. Paths that
// are not "<step>.<field>" pairs are read from graph.GlobalWorkspace instead.
// DB reads happen outside s.Mu so the batch query cannot stall parallel steps;
// only the coordination-edge bookkeeping runs under the read lock.
func (s *DirectedEngine) resolveStepInputs(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, taskID string) map[string]any {
	resolvedInputs := make(map[string]any)
	if len(step.InputMapping) == 0 {
		return resolvedInputs
	}
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
	// (audit P-H1: was N+1 queries — now batched via GetBatch)
	neededStepIDs := make([]string, 0, len(neededSteps))
	for sid := range neededSteps {
		neededStepIDs = append(neededStepIDs, sid)
	}
	stepResults, _ := s.taskStore.GetBatch(ctx, taskID, neededStepIDs) // audit P-H1: single batch query replaces N

	// P-H7 fix: pre-compute payload sizes outside RLock to avoid
	// json.Marshal allocations in the hot loop under lock.
	type mappingEntry struct {
		arg         string
		val         any
		sourceStep  string
		edgeType    string
		payloadSize int64
	}
	var mappings []mappingEntry
	for arg, path := range step.InputMapping {
		parts := strings.SplitN(path, ".", 2)
		if len(parts) == 2 {
			if res, ok := stepResults[parts[0]]; ok && res.Data != nil {
				if m, ok := res.Data.(map[string]any); ok {
					val := m[parts[1]]
					payloadSize := estimateJSONSize(val) // audit H-4: no allocation
					mappings = append(mappings, mappingEntry{arg, val, parts[0], "mapping", payloadSize})
				}
			}
		}
	}

	// Phase 3: Under RLock, set resolvedInputs + record coordination edges
	s.Mu.RLock()
	for _, me := range mappings {
		resolvedInputs[me.arg] = me.val
		s.recordCoordinationEdge(graph, me.sourceStep, step.ID, me.edgeType, me.payloadSize)
	}
	// GlobalWorkspace reads need the lock; sizes computed here are
	// for less-common shared-vfs edges (not the hot mapping path).
	for arg, path := range step.InputMapping {
		parts := strings.SplitN(path, ".", 2)
		if len(parts) != 2 {
			if val, ok := graph.GlobalWorkspace[path]; ok {
				resolvedInputs[arg] = val
				payloadSize := estimateJSONSize(val) // audit H-4: no allocation
				s.recordCoordinationEdge(graph, "global_workspace", step.ID, "shared_vfs", payloadSize)
			}
		}
	}
	s.Mu.RUnlock()
	return resolvedInputs
}

// estimateStepPromptTokens returns a conservative prompt-side token estimate
// for a step about to run. It is the look-ahead projection for the budget
// gate (Phase 0.1b): only the INPUT side (task text + a fixed system-prompt
// overhead) is estimated, because output tokens cannot be predicted without
// a tokenizer and are already covered by BudgetGuard's safeMargin. A
// conservative fixed overhead keeps the estimate from being wildly low on
// steps whose prompt is mostly role system-prompt rather than task text.
//
// Returns 0 when there is nothing to estimate (empty task), which the caller
// treats as "unknown cost → skip the look-ahead, rely on CheckBudget".
func estimateStepPromptTokens(step *schemas.Step) int {
	if step == nil || step.Task == "" {
		return 0
	}
	// Task text dominates the input side; add a fixed conservative allowance
	// for the role system prompt + tool schema + context blocks. 2000 is a
	// deliberate floor, not an exact model — it only needs to be in the right
	// ballpark for the overrun check to be useful, and underestimating here is
	// bounded by safeMargin on the other side.
	const systemPromptOverhead = 2000
	return providers.EstimateTokens(step.Task) + systemPromptOverhead
}
