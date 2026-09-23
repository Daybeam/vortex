package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ApprovePlan transitions a task graph from GraphPendingReview to GraphRunning
// and starts execution. As of 2026-09-06, plan review workflows are handled
// via decision submission rather than explicit graph approval, leaving this
// method without an active caller in core/ or tools/. It remains for future
// human-in-the-loop plan review pipelines.
func (s *DirectedEngine) ApprovePlan(taskID string) error {
	s.Mu.Lock()
	graph, ok := s.graphs[taskID]
	if !ok {
		s.Mu.Unlock()
		return fmt.Errorf("task %s not found", taskID)
	}

	if graph.Status != schemas.GraphPendingReview {
		s.Mu.Unlock()
		return fmt.Errorf("task %s is not pending review (status: %s)", taskID, graph.Status)
	}

	graph.Status = schemas.GraphRunning
	ctx, cancel := context.WithCancel(s.lifecycleCtx) // audit C5: derive from lifecycleCtx so Stop() cancels tasks
	s.cancelFuncs[taskID] = cancel
	s.Mu.Unlock()

	s.persistGraph(graph)
	s.Broadcast()

	s.logger.Log("task_approved", taskID, "", nil)

	if !s.UseSwarm {
		s.goBackground(func() { s.run(ctx, taskID) }) // audit C5: use goBackground so Stop() drains
	} else {
		s.goBackground(func() { s.swarmWatchdog(ctx, taskID) }) // audit C5: use goBackground so Stop() drains
	}

	return nil
}

// ─── Poll ─────────────────────────────────────────────────────────────────

// ─── Poll ─────────────────────────────────────────────────────────────────

func (s *DirectedEngine) GetStatus(taskID string, view ...string) (map[string]any, bool) {
	v := "summary"
	if len(view) > 0 {
		v = view[0]
	}

	s.Mu.RLock()
	graph := s.graphs[taskID]
	if graph != nil {
		result := graph.ToStatusDictView(v)
		s.Mu.RUnlock()
		return result, true
	}
	s.Mu.RUnlock()

	// Fallback: the graph may be owned by another process (Master/Proxy split) or
	// may have been lost to a restart. Without this, callers see "not found" for
	// tasks that are in fact running — the task-level rows do not exist in
	// task_steps, so nothing else can answer the query.
	if s.TaskRegistry == nil {
		return nil, false
	}
	row, graphJSON, err := s.TaskRegistry.LoadTask(s.lifecycleCtx, taskID)
	if err != nil || row == nil {
		return nil, false
	}

	status := map[string]any{
		"task_id":    row.TaskID,
		"status":     row.Status,
		"updated_at": row.UpdatedAt,
		"source":     "persisted",
	}
	if len(graphJSON) > 0 && v == "full" {
		var g schemas.TaskGraph
		if err := json.Unmarshal(graphJSON, &g); err == nil {
			sanitized := g.ToStatusDictView(v)
			for k, val := range sanitized {
				if _, exists := status[k]; !exists {
					status[k] = val
				}
			}
		}
	}
	return status, true
}

// persistTaskState writes task-level lifecycle state so a task stays queryable
// outside the process that owns it. No-op when no registry is configured.

func (s *DirectedEngine) WaitTask(ctx context.Context, taskID string, timeout time.Duration, view ...string) (map[string]any, bool) {
	v := "summary"
	if len(view) > 0 {
		v = view[0]
	}

	s.Mu.RLock()
	ch, ok := s.doneChans[taskID]
	graph := s.graphs[taskID]
	s.Mu.RUnlock()

	if !ok || graph == nil {
		return nil, false
	}

	// Fast path: check if already terminal
	if graph.Status == schemas.GraphCompleted || graph.Status == schemas.GraphFailed || graph.Status == schemas.GraphBlocked {
		return graph.ToStatusDictView(v), true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
		return graph.ToStatusDictView(v), true
	case <-ctx.Done():
		return graph.ToStatusDictView(v), true
	case <-timer.C:
		status := graph.ToStatusDictView(v)
		// Behavior Contract: return STILL_RUNNING if timeout reached and still running
		if graph.Status == schemas.GraphRunning {
			status["wait_status"] = "STILL_RUNNING"
		}
		return status, true
	}
}

// setGraphStatus atomically updates graph.Status under Mu.Lock.
// Use this instead of direct field assignment to prevent data races
// between run()'s reader (RLock) and step-goroutine writers.
func (s *DirectedEngine) setGraphStatus(graph *schemas.TaskGraph, status schemas.GraphStatus) {
	s.Mu.Lock()
	graph.Status = status
	s.Mu.Unlock()
}

// setStepAndGraphStatus atomically updates both step.Status and graph.Status.
// Use this when a failure blocks both the step and the graph simultaneously.
func (s *DirectedEngine) setStepAndGraphStatus(step *schemas.Step, stepStatus schemas.StepStatus, graph *schemas.TaskGraph, graphStatus schemas.GraphStatus) {
	s.Mu.Lock()
	step.Status = stepStatus
	graph.Status = graphStatus
	s.Mu.Unlock()
}

func (s *DirectedEngine) broadcastDone(taskID string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if ch, ok := s.doneChans[taskID]; ok {
		select {
		case <-ch:
			// Already closed
		default:
			close(ch)
		}
	}
}

func (s *DirectedEngine) GetManifest(taskID string) (map[string]any, bool) {
	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()
	if graph == nil {
		// FALLBACK: Read from disk if task is no longer in memory
		manifestPath := filepath.Join(s.outputBase, taskID, "manifest.json")
		b, err := os.ReadFile(manifestPath)
		if err == nil {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err == nil {
				return m, true
			}
		}
		return nil, false
	}
	m := graph.ToStatusDict()
	m["manifest_path"] = filepath.Join(s.outputBase, taskID, "manifest.json")
	return m, true
}

func (s *DirectedEngine) ListGraphs() []map[string]any {
	s.Mu.RLock()
	seen := make(map[string]bool, len(s.graphs))
	out := make([]map[string]any, 0, len(s.graphs))
	for id, g := range s.graphs {
		out = append(out, g.ToStatusDict())
		seen[id] = true
	}
	s.Mu.RUnlock()

	// Also include persisted tasks from disk that aren't in memory (e.g. after restart).
	entries, err := os.ReadDir(s.outputBase)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() || seen[entry.Name()] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.outputBase, entry.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var g schemas.TaskGraph
		if json.Unmarshal(data, &g) == nil {
			out = append(out, g.ToStatusDict())
		}
	}
	return out
}

// getOrReloadGraphLocked returns the in-memory graph for taskID, or attempts
// to reload it from the persisted manifest.json on disk. This fixes the bug
// where FulfillStep/SubmitDecisionWithPayload returned "not found" for tasks
// that were evicted from s.graphs but still persisted — GetStatus already had
// this fallback via TaskRegistry, but the decision handlers did not.
// Caller MUST hold s.Mu (write lock).
func (s *DirectedEngine) getOrReloadGraphLocked(taskID string) *schemas.TaskGraph {
	graph := s.graphs[taskID]
	if graph != nil {
		return graph
	}
	manifestPath := filepath.Join(s.outputBase, taskID, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var g schemas.TaskGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil
	}
	s.graphs[taskID] = &g
	return &g
}

func (s *DirectedEngine) FulfillStep(taskID, decisionID, outputJSON string) error {
	// Phase 1: read under lock, collect DB write params.
	s.Mu.Lock()
	graph := s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}

	var dec *schemas.Decision
	var decIdx int = -1
	for i, d := range graph.PendingDecisions {
		if d.ID == decisionID {
			dec = d
			decIdx = i
			break
		}
	}
	if dec == nil {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q not found", decisionID)
	}

	if dec.Type != schemas.DecisionDelegationRequired {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q is not a delegation", decisionID)
	}

	step := graph.Steps[dec.StepID]
	if step == nil {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found", dec.StepID)
	}

	// Parse the output as a SubagentOutput
	output := schemas.ParseOutput(outputJSON, step.RoleID)
	if output.Status == schemas.StatusFailed {
		s.Mu.Unlock()
		return fmt.Errorf("failed to parse fulfillment output as valid JSON")
	}

	// Snapshot the data needed for DB write (will happen outside lock).
	stepID := step.ID
	result := &store.StepResult{
		Data:           output.Result,
		Confidence:     output.Confidence,
		MissingContext: output.MissingContext,
		Capability:     output.Capability,
		Attachments:    output.Attachments,
		CreatedAt:      time.Now(),
	}
	s.Mu.Unlock()

	// Phase 2: DB write outside lock — avoids blocking all status reads
	// during the SQLite write.
	if err := s.taskStore.Set(s.lifecycleCtx, taskID, stepID, result); err != nil {
		return fmt.Errorf("failed to save result to task store: %w", err)
	}

	// Phase 3: re-acquire lock, re-validate, and update graph.
	s.Mu.Lock()

	graph = s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found after DB write", taskID)
	}

	// Re-find the decision (it might have been removed by another goroutine).
	decIdx = -1
	for i, d := range graph.PendingDecisions {
		if d.ID == decisionID {
			dec = d
			decIdx = i
			break
		}
	}
	if dec == nil {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q was removed by another operation", decisionID)
	}

	step = graph.Steps[dec.StepID]
	if step == nil {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found after DB write", dec.StepID)
	}

	// Update step status
	step.Status = schemas.StepOK
	step.Confidence = &output.Confidence
	step.LastError = ""
	step.LastWorkedOn = time.Now()

	// Clear the decision
	graph.PendingDecisions = append(graph.PendingDecisions[:decIdx], graph.PendingDecisions[decIdx+1:]...)

	// Resume graph if no other pending decisions
	if len(graph.PendingDecisions) == 0 && graph.Status == schemas.GraphBlocked {
		if graph.IsSmartRouted {
			graph.Status = schemas.GraphCompleted
			if ch, ok := s.doneChans[taskID]; ok {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
		} else {
			graph.Status = schemas.GraphRunning
			s.doneChans[taskID] = make(chan struct{})
		}
	}

	s.Mu.Unlock()
	s.persistGraph(graph)
	return nil
}

// GetSignalState returns the raw ACAIS SignalField intensity map (layer →
// entity → intensity). As of 2026-09-06, the signal field is used internally
// for step scheduling and cross-step coordination, but this accessor is not
// called by any tool or admin endpoint. It remains for debugging and future
// observability UI.
func (s *DirectedEngine) GetSignalState() map[string]map[string]float64 {
	if s.SignalField == nil {
		return nil
	}
	s.SignalField.Mu.RLock()
	defer s.SignalField.Mu.RUnlock()

	state := make(map[string]map[string]float64)
	for layer, tasks := range s.SignalField.Layers {
		lName := string(layer)
		state[lName] = make(map[string]float64)
		for id, intensity := range tasks {
			state[lName][id] = intensity.Value
		}
	}
	return state
}

// ─── Decision ─────────────────────────────────────────────────────────────

// ─── Decision ─────────────────────────────────────────────────────────────

func (s *DirectedEngine) SubmitDecision(taskID, decisionID, choice string) error {
	return s.SubmitDecisionWithPayload(taskID, decisionID, choice, "")
}

func (s *DirectedEngine) SubmitDecisionWithPayload(taskID, decisionID, choice, payload string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()

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
			if dec.Context != nil {
				if feedback, ok := dec.Context["feedback"].(string); ok {
					step.AdditionalPromptContext = feedback
				}
			}
	case "approve":
		step.Status = schemas.StepOK
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
				step.ProviderOverride = model
				step.Status = schemas.StepPending
				step.RetryCount = 0
			default:
				return fmt.Errorf("unknown choice %q", choice)
			}
		}
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
	if graph.Status == schemas.GraphBlocked && len(graph.PendingDecisions) == 0 {
		graph.Status = schemas.GraphRunning
		// Create a new done channel for the resumed task session
		s.doneChans[taskID] = make(chan struct{})
	}

	s.logger.Log(EventDecisionSubmitted, taskID, dec.StepID, map[string]any{
		"decision_id": decisionID,
		"choice":      choice,
	})
	return nil
}

// ─── Clear ────────────────────────────────────────────────────────────────

func (s *DirectedEngine) GetRoleAffinity(roleID, stepID string) float64 {
	s.Mu.RLock()
	var targetStep *schemas.Step
	for _, g := range s.graphs {
		if st, ok := g.Steps[stepID]; ok {
			targetStep = st
			break
		}
	}
	s.Mu.RUnlock()

	if targetStep == nil {
		return 0.5
	}

	// Determine task type (capability)
	taskType := "unknown"
	s.Mu.RLock()
	role := s.registry.Roles[targetStep.RoleID]
	if role != nil {
		taskType = role.BaseCapability
	}
	s.Mu.RUnlock()

	return s.expStore.QueryRoleAffinity(s.lifecycleCtx, roleID, taskType)
}

func (s *DirectedEngine) GetSignalField() interfaces.SignalFieldInterface {
	return s.SignalField
}

// GetGraphs returns a snapshot of all active task graphs.

// GetGraphs returns a snapshot of all active task graphs.
func (s *DirectedEngine) GetGraphs() map[string]*schemas.TaskGraph {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	res := make(map[string]*schemas.TaskGraph)
	for k, v := range s.graphs {
		res[k] = v
	}
	return res
}

// HandleSwarmOutput processes a result submitted by a remote swarm worker.
