package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

	// audit C5: startGraphExecutor handles UseSwarm branching.
	s.startGraphExecutor(ctx, taskID)

	return nil
}

// ─── Poll ─────────────────────────────────────────────────────────────────

// ─── Poll ─────────────────────────────────────────────────────────────────

// GetTaskOwner returns the OwnerID of a task, or "" if not found.
// Used by tool handlers for ownership guard checks.
func (s *DirectedEngine) GetTaskOwner(taskID string) string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if graph, ok := s.graphs[taskID]; ok {
		return graph.OwnerID
	}
	return ""
}

func (s *DirectedEngine) GetStatus(taskID string, view ...string) (map[string]any, bool) {
	v := "summary"
	if len(view) > 0 {
		v = view[0]
	}

	s.Mu.RLock()
	graph := s.graphs[taskID]
	if graph != nil {
		result := graph.ToStatusDictView(v)
		now := time.Now()
		// Q4-6/O20: say so out loud when a delegation has gone unanswered past
		// the window, instead of leaving "running" as the only signal.
		s.attachDelegationWaitLocked(result, graph, now)
		// Q5-5: same for a step_failed decision nobody has answered in a day.
		s.attachStaleDecisionReportLocked(result, graph, now)
		s.Mu.RUnlock()
		return result, true
	}
	s.Mu.RUnlock()

	// Fallback: the graph may be owned by another process (Master/Proxy split) or
	// may have been lost to a restart. Without this, callers see "not found" for
	// tasks that are in fact running — the task-level rows do not exist in
	// task_steps, so nothing else can answer the query.
	tr := s.taskRegistry()
	if tr == nil {
		return nil, false
	}
	row, graphJSON, err := tr.LoadTask(s.lifecycleCtx, taskID)
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
			// Same report the in-memory path gives, so a host cannot get a
			// silent "running" from whichever side of the master/proxy split
			// happens to answer. g is a private copy, but the helper's contract
			// is "caller holds Mu", so take the read lock rather than fork the
			// contract for one path.
			now := time.Now()
			s.Mu.RLock()
			s.attachDelegationWaitLocked(status, &g, now)
			s.attachStaleDecisionReportLocked(status, &g, now)
			s.Mu.RUnlock()
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

	// audit L-N8: graph.Status, graph.PendingDecisions, and graph.ToStatusDictView
	// (which iterates graph.Steps map) must be read under RLock — MutateGraphTopology
	// can add steps concurrently, and concurrent map iteration + write can
	// fatal-panic. Use a snapshot helper to avoid repeating lock/unlock.
	snapshotStatus := func() (map[string]any, schemas.GraphStatus, int) {
		s.Mu.RLock()
		defer s.Mu.RUnlock()
		status := graph.ToStatusDictView(v)
		// wait_task is where a host parks itself, so the no-answer report has to
		// be on this path too, not only on get_task_status (Q4-6/O20). The
		// step_failed staleness report (Q5-5) rides along.
		now := time.Now()
		s.attachDelegationWaitLocked(status, graph, now)
		s.attachStaleDecisionReportLocked(status, graph, now)
		return status, graph.Status, len(graph.PendingDecisions)
	}

	// Fast path: check if already terminal or has pending decisions
	status, graphStatus, pendingDecs := snapshotStatus()
	if graphStatus == schemas.GraphCompleted || graphStatus == schemas.GraphFailed || graphStatus == schemas.GraphBlocked || pendingDecs > 0 {
		return status, true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
		status, _, _ := snapshotStatus()
		return status, true
	case <-ctx.Done():
		status, _, _ := snapshotStatus()
		return status, true
	case <-timer.C:
		status, graphStatus, pendingDecs := snapshotStatus()
		// Behavior Contract: return STILL_RUNNING if timeout reached and
		// still running with no pending decisions.
		if graphStatus == schemas.GraphRunning && pendingDecs == 0 {
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
	// L-N17 (2026-10-04): Move ToStatusDict() inside RLock to close a TOCTOU
	// window. Previously, RUnlock happened before ToStatusDict(), so another
	// goroutine could mutate the graph (add/remove steps, change status)
	// between the unlock and the read — a data race.
	var m map[string]any
	if graph != nil {
		m = graph.ToStatusDict()
	}
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
	m["manifest_path"] = filepath.Join(s.outputBase, taskID, "manifest.json")
	return m, true
}

func (s *DirectedEngine) ListGraphs() []map[string]any {
	now := time.Now()
	s.Mu.RLock()
	seen := make(map[string]bool, len(s.graphs))
	out := make([]map[string]any, 0, len(s.graphs))
	for id, g := range s.graphs {
		status := g.ToStatusDict()
		// /tasks reads this list, not GetStatus, so the Q5-5 staleness signals
		// have to be attached here as well or the task list stays silent.
		s.attachDelegationWaitLocked(status, g, now)
		s.attachStaleDecisionReportLocked(status, g, now)
		out = append(out, status)
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
			// Fix: tasks not in s.graphs have no active goroutine. If the
			// manifest says running/blocked, the task was interrupted by an
			// engine restart and will never complete. Override to orphaned
			// so the status reflects reality instead of showing "running"
			// forever.
			if g.Status == schemas.GraphRunning || g.Status == schemas.GraphBlocked {
				g.Status = schemas.GraphOrphaned
			}
			status := g.ToStatusDict()
			// A parked decision on an orphaned task is still answerable
			// (getOrReloadGraphLocked revives the manifest), so it gets the same
			// signals as the in-memory half.
			s.Mu.RLock()
			s.attachDelegationWaitLocked(status, &g, now)
			s.attachStaleDecisionReportLocked(status, &g, now)
			s.Mu.RUnlock()
			out = append(out, status)
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
		// audit L-21-2: distinguish "no such task" (normal — new task, no
		// manifest yet) from a real read failure (permissions, I/O) so operators
		// get a signal instead of a silent nil.
		if !os.IsNotExist(err) {
			slog.Warn("scheduler_status: manifest read failed", "taskID", taskID, "err", err)
		}
		return nil
	}
	var g schemas.TaskGraph
	if err := json.Unmarshal(data, &g); err != nil {
		slog.Warn("scheduler_status: corrupt manifest, returning nil graph", "taskID", taskID, "err", err)
		return nil
	}
	s.graphs[taskID] = &g
	return &g
}

// findPendingDecisionLocked returns the decision with the given ID and its
// index in graph.PendingDecisions, or (nil, -1) if not found. Caller must
// hold s.Mu.
func findPendingDecisionLocked(graph *schemas.TaskGraph, decisionID string) (*schemas.Decision, int) {
	for i, d := range graph.PendingDecisions {
		if d.ID == decisionID {
			return d, i
		}
	}
	return nil, -1
}

// misroutedFulfillError explains why a non-delegation decision reached the
// delegation-only FulfillStep entry point.
//
// O17 (2026-11-04): the tool layer used to route any submit_decision call
// carrying a non-empty `output` here (shouldRouteToFulfill in tools/tools_task.go),
// so a host that serialises an empty result object for skip|abort landed on this
// path. Q4-5 (2026-11-05) put the decision's type in front of that heuristic —
// see routeSubmitDecision in tools/submit_decision_route.go — so this error now
// mainly fires for a caller that named choice="fulfill" on the wrong decision.
// The wording used to talk about delegations only, which is what made me record
// a false finding ("a step_failed decision can be parked but never answered").
// Name the decision's real type and the payload rule that applies.
func misroutedFulfillError(decisionID string, decType schemas.DecisionType) error {
	return fmt.Errorf("decision %q is a %q decision, not a delegation: answer it with choice=skip|abort|retry and NO output payload (a non-empty output is what routed this call onto the delegation-only fulfill path)", decisionID, decType)
}

func (s *DirectedEngine) FulfillStep(taskID, decisionID, outputJSON string) error {
	// Phase 1: read under lock, collect DB write params.
	s.Mu.Lock()
	graph := s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}

	dec, decIdx := findPendingDecisionLocked(graph, decisionID)
	if dec == nil {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q not found", decisionID)
	}

	if dec.Type != schemas.DecisionDelegationRequired {
		s.Mu.Unlock()
		return misroutedFulfillError(decisionID, dec.Type)
	}

	step := graph.Steps[dec.StepID]
	if step == nil {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found", dec.StepID)
	}

	// D5 P1b (DELEGATION_HANDOVER_REDESIGN.md §4.5): when the delegation
	// kind is "plan", the host has produced a plan. Parse it via ParsePlanDAG
	// and inject the steps via graph surgery (mutateGraphTopologyLocked).
	if kind, _ := dec.Context["kind"].(string); kind == DelegationKindPlan {
		planned, err := schemas.ParsePlanDAG(outputJSON, schemas.PlanParseOptions{})
		if err != nil {
			s.Mu.Unlock()
			return fmt.Errorf("plan delegation: failed to parse plan: %w", err)
		}
		if len(planned) == 0 {
			s.Mu.Unlock()
			return fmt.Errorf("plan delegation: parsed plan has zero steps")
		}
		// Inject: deprecate the planning step, add the planned steps.
		if err := s.mutateGraphTopologyLocked(graph, SurgeryData{
			DeprecatedStepID: dec.StepID,
			ReplacementSteps: planned,
		}); err != nil {
			s.Mu.Unlock()
			return fmt.Errorf("plan delegation: graph surgery failed: %w", err)
		}
		// Clear the decision.
		graph.PendingDecisions = append(graph.PendingDecisions[:decIdx], graph.PendingDecisions[decIdx+1:]...)
		// The graph is now a real DAG, not smart-routed.
		graph.IsSmartRouted = false
		graph.Status = schemas.GraphRunning
		taskID := graph.TaskID
		s.Mu.Unlock()
		s.persistGraph(graph)
		s.logger.Log("plan_delegation_fulfilled", taskID, dec.StepID, map[string]any{
			"steps_injected": len(planned),
		})
		// Start the executor so the new steps run.
		s.startGraphExecutor(s.lifecycleCtx, taskID)
		return nil
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
	dec, decIdx = findPendingDecisionLocked(graph, decisionID)
	if dec == nil {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q was removed by another operation", decisionID)
	}

	step = graph.Steps[dec.StepID]
	if step == nil {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found after DB write", dec.StepID)
	}

	// D3 fix (DELEGATION_HANDOVER_REDESIGN.md §4.3): step status (StepOK /
	// LastError / ResultRef), EventStepCompleted, and the full post-success
	// pipeline (GlobalWorkspace merge, AutoDepositResult→artifacts, archiveStep,
	// SignalField, output_files) are now applied by handleStepSuccess below —
	// reusing the same code path as normal spawn completion. Previously
	// FulfillStep only wrote taskStore + set StepOK manually, silently dropping
	// all side-effects (merge_strategy, artifacts, archive, output_files, events).
	step.Confidence = &output.Confidence
	step.LastWorkedOn = time.Now()

	// R1 fix (DELEGATION_HANDOVER_REDESIGN.md §10.1): set StepOK inside the
	// lock to close the transient illegal-state window. Without this, there
	// is a gap between clearing PendingDecisions and handleStepSuccess (which
	// runs lock-free below) where step=blocked + pendingDecs==0. run() can be
	// woken by any task's Broadcast (notifyChan is engine-global) and would
	// see the step as "upstream terminated non-OK" → spurious decision or
	// premature finalize. handleStepSuccess sets these again (idempotent).
	step.Status = schemas.StepOK
	step.LastError = ""

	// Clear the decision
	graph.PendingDecisions = append(graph.PendingDecisions[:decIdx], graph.PendingDecisions[decIdx+1:]...)

	// Resume graph if no other pending decisions
	if len(graph.PendingDecisions) == 0 {
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
			// Graph is already GraphRunning (step-level blocking never set
			// GraphBlocked). Create a fresh done channel so WaitTask callers
			// can wait for the next terminal state, and Broadcast to wake
			// the run() loop which is waiting for decision resolution.
			s.doneChans[taskID] = make(chan struct{})
		}
	}

	s.Mu.Unlock()

	// D3 fix: reuse the full step-success pipeline. We call handleStepSuccess
	// directly (not handleOutput) to skip the confidence threshold gate — the
	// host's self-reported confidence must not turn a fulfilled step into a
	// retry decision. State is determined by the fulfill action (always OK);
	// post-processing (workspace/artifacts/archive/signal/output_files) is
	// reused from the normal spawn path.
	s.handleStepSuccess(graph, step, &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:         schemas.StatusOK,
			Confidence:     output.Confidence,
			Result:         output.Result,
			Capability:     output.Capability,
			MissingContext: output.MissingContext,
			Attachments:    output.Attachments,
			Usage:          output.Usage, // O56: handleStepSuccess reads this for cost + event; do not drop it again.
		},
		Ref: fmt.Sprintf("%s:%s", taskID, stepID),
	})

	s.persistGraph(graph)
	// Wake the run() loop to check for newly-ready steps after decision resolution.
	s.Broadcast()
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

// applyApproveChoice handles SubmitDecision "approve" (G2, 2026-10-22; skills
// added D3). A mount-policy approval records the approved MCPs/skills on the
// graph (so the next ResolveFinalMCPs/ResolveFinalSkills allows them) and
// re-queues the step — it was blocked at prepare and never ran. A plain
// post-completion checkpoint just advances the already-run step. Extracted to
// keep SubmitDecisionWithPayload under ratchet.
func (s *DirectedEngine) applyApproveChoice(graph *schemas.TaskGraph, step *schemas.Step, dec *schemas.Decision) {
	mounts := decisionApprovalMounts(dec)
	skills := decisionApprovalIDs(dec, "approval_required_skills")
	if len(mounts) > 0 || len(skills) > 0 {
		approverID, _ := dec.Context["_resolver_owner_id"].(string)
		channel, _ := dec.Context["_resolver_channel"].(string)
		graph.ApproveMCPMounts(mounts, approverID, channel)
		graph.ApproveSkillMounts(skills, approverID, channel)
		step.Status = schemas.StepPending
		step.RetryCount = 0
		step.LastError = ""
		return
	}
	step.Status = schemas.StepOK
}

// decisionApprovalMounts extracts the MCP ids named by a
// human_approval_required mount decision (G2).
func decisionApprovalMounts(dec *schemas.Decision) []string {
	return decisionApprovalIDs(dec, "approval_required_mounts")
}

// decisionApprovalIDs extracts string ids from a decision context key (G2 for
// MCPs; skills D3, 2026-10-22). The context value is []string in-process and
// []any after a JSON reload, so both shapes are handled.
func decisionApprovalIDs(dec *schemas.Decision, key string) []string {
	if dec == nil || dec.Context == nil {
		return nil
	}
	switch v := dec.Context[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
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
	s.registry.Mu.RLock()
	role := s.registry.Roles[targetStep.RoleID]
	if role != nil {
		taskType = role.BaseCapability
	}
	s.registry.Mu.RUnlock()

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
