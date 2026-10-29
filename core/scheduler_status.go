package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
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

	// audit L-N8: graph.Status, graph.PendingDecisions, and graph.ToStatusDictView
	// (which iterates graph.Steps map) must be read under RLock — MutateGraphTopology
	// can add steps concurrently, and concurrent map iteration + write can
	// fatal-panic. Use a snapshot helper to avoid repeating lock/unlock.
	snapshotStatus := func() (map[string]any, schemas.GraphStatus, int) {
		s.Mu.RLock()
		defer s.Mu.RUnlock()
		return graph.ToStatusDictView(v), graph.Status, len(graph.PendingDecisions)
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

func (s *DirectedEngine) SubmitDecision(taskID, decisionID, choice string) error {
	return s.SubmitDecisionWithPayload(taskID, decisionID, choice, "")
}

func (s *DirectedEngine) SubmitDecisionWithPayload(taskID, decisionID, choice, payload string) error {
	s.Mu.Lock()
	unlocked := false
	defer func() { if !unlocked { s.Mu.Unlock() } }()

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
	}

	// #15: Audit hash chain — record decision submission (tamper-evident).
	if s.auditLogger != nil {
		s.auditLogger.Log("submitDecision", taskID, dec.StepID, decisionID, string(dec.Type), choice, dec.Options)
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

	s.logger.Log(EventDecisionSubmitted, taskID, dec.StepID, map[string]any{
		"decision_id": decisionID,
		"choice":      choice,
	})
	// Wake the run() loop to check for newly-ready steps after decision resolution.
	s.Broadcast()
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
