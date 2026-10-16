package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
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

// estimateJSONSize approximates the JSON byte length of a value without
// allocating a buffer (audit H-4: replaces json.Marshal in the per-step hot
// loop, which was allocating + traversing the value tree purely to compute
// len(bytes) for coordination-edge bookkeeping). Falls back to json.Marshal
// only for uncommon types, preserving correctness for edge cases.
func estimateJSONSize(v any) int64 {
	switch val := v.(type) {
	case nil:
		return 4 // "null"
	case string:
		return int64(len(val) + 2) // quotes
	case bool:
		if val {
			return 4 // "true"
		}
		return 5 // "false"
	case int:
		return int64(len(fmt.Sprintf("%d", val)))
	case int64:
		return int64(len(fmt.Sprintf("%d", val)))
	case float64:
		return int64(len(fmt.Sprintf("%v", val)))
	case map[string]any:
		size := int64(2) // {}
		first := true
		for k, vv := range val {
			if !first {
				size++ // comma
			}
			first = false
			size += int64(len(k) + 3) // "key":
			size += estimateJSONSize(vv)
		}
		return size
	case []any:
		size := int64(2) // []
		first := true
		for _, vv := range val {
			if !first {
				size++ // comma
			}
			first = false
			size += estimateJSONSize(vv)
		}
		return size
	default:
		// Fallback: marshal for unknown types (correctness over speed).
		if b, err := json.Marshal(val); err == nil {
			return int64(len(b))
		}
		return 0
	}
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
			// audit H-6: wait for a broadcast event instead of busy-wait polling.
			// The notify channel wakes instantly on state change; the 5s safety
			// timeout handles edge cases where Broadcast isn't called.
			// audit P-8-sibling: use NewTimer + Stop instead of time.After to avoid timer leak.
			notify := s.GetNotifyChan()
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-notify:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}

		if len(ready) == 0 {
			s.Mu.RLock()
			isTerminal := graph.IsTerminal()
			pendingDecs := len(graph.PendingDecisions)
			s.Mu.RUnlock()
			if isTerminal && pendingDecs == 0 {
				s.finalize(graph)
				return
			}
			// If there are pending decisions, wait for resolution rather than
			// firing detectUpstreamInsufficient (which would create spurious
			// decisions for steps that are simply waiting for a decision to
			// unblock their upstream).
			if pendingDecs > 0 {
				notify := s.GetNotifyChan()
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-notify:
					timer.Stop()
				case <-timer.C:
				}
				continue
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
					[]string{"skip", "abort"})
				s.persistGraph(graph)
				s.Broadcast()
				continue
			}
			// audit H-6: wait for broadcast event instead of 200ms busy-wait.
			// audit P-8-sibling: use NewTimer + Stop instead of time.After.
			notify := s.GetNotifyChan()
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-notify:
				timer.Stop()
			case <-timer.C:
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

		// audit H-1: hoist graph-status check out of the loop. Previously this
		// acquired s.Mu.RLock/RUnlock on EVERY iteration (N lock cycles per
		// scheduling tick), amplifying global mutex contention (C-1). The status
		// can only change via a writer holding Lock; if it transitions to
		// non-running mid-loop, the per-step ctx.Done() check (line 132) and
		// executeStep's context propagation handle graceful bailout.
		s.Mu.RLock()
		graphRunning := graph.Status == schemas.GraphRunning
		s.Mu.RUnlock()
		if !graphRunning {
			break
		}

		for _, step := range ready {
			wg.Add(1)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Done()
				// audit C-10: wait for in-flight goroutines with grace period before
				// returning, so previously launched steps can bail out cleanly.
				wd := make(chan struct{})
				go func() { wg.Wait(); close(wd) }()
				select {
				case <-wd:
				case <-time.After(5 * time.Second):
				}
				return
			}
			go func(st *schemas.Step) {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					if r := recover(); r != nil {
						log.Printf("PANIC in executeStep %s: %v\n%s", st.ID, r, debug.Stack())
					}
				}()
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

// HandleSwarmOutput processes a result submitted by a remote swarm worker.
func (s *DirectedEngine) HandleSwarmOutput(taskID, stepID string, output *schemas.SubagentOutput) {
	// audit L-1.7: combine graph + step lookup into a single Lock section to
	// prevent TOCTOU (graph could be evicted between the two lock acquisitions).
	s.Mu.Lock()
	graph := s.graphs[taskID]
	if graph == nil {
		s.Mu.Unlock()
		return
	}
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

func (s *DirectedEngine) ReleaseTask(stepID string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	for _, g := range s.graphs {
		if g.Status != schemas.GraphRunning {
			continue
		}
		if step, ok := g.Steps[stepID]; ok && step.Status == schemas.StepRunning {
			step.Status = schemas.StepPending
			return
		}
	}
}

func (s *DirectedEngine) ExecuteTask(ctx context.Context, stepID string) (*schemas.SubagentOutput, error) {
	// audit L-4.1: lookup both graph and step under RLock with nil check.
	// Previously, step was dereferenced after RUnlock without ok check,
	// risking nil dereference and concurrent map access.
	s.Mu.RLock()
	var targetGraph *schemas.TaskGraph
	var step *schemas.Step
	for _, g := range s.graphs {
		if st, ok := g.Steps[stepID]; ok {
			targetGraph = g
			step = st
			break
		}
	}
	s.Mu.RUnlock()

	if targetGraph == nil || step == nil {
		return nil, fmt.Errorf("task step %s not found", stepID)
	}

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
