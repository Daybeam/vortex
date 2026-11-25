package schemas

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// ─── TaskGraph methods ─────────────────────────────────────────────────────

// RecordSubstitution appends a Substitution entry to the ledger. Thread-safe.
// Call this at every fallback/degradation point alongside the existing log event.
func (g *TaskGraph) RecordSubstitution(kind, requested, actual, reason, stepID string) {
	g.substitutionMu.Lock()
	g.Substitutions = append(g.Substitutions, Substitution{
		Kind:      kind,
		Requested: requested,
		Actual:    actual,
		Reason:    reason,
		StepID:    stepID,
		At:        time.Now(),
	})
	g.substitutionMu.Unlock()
}

// GetSubstitutions returns a snapshot of the substitution ledger. Thread-safe.
func (g *TaskGraph) GetSubstitutions() []Substitution {
	g.substitutionMu.Lock()
	defer g.substitutionMu.Unlock()
	cp := make([]Substitution, len(g.Substitutions))
	copy(cp, g.Substitutions)
	return cp
}

// SubstitutionCount returns the number of recorded substitutions. Thread-safe.
// Use this instead of len(g.Substitutions) in read-only contexts (e.g. status
// summary views) to avoid racing with RecordSubstitution's append.
func (g *TaskGraph) SubstitutionCount() int {
	g.substitutionMu.Lock()
	defer g.substitutionMu.Unlock()
	return len(g.Substitutions)
}

// GetSessionRole returns the raw stored value for a session-scoped role (either
// an already-typed *config.Role or a map[string]any, depending on whether the
// graph was freshly created or restored from a persisted manifest), and
// whether it exists. Safe for concurrent use across the parallel step
// goroutines DirectedEngine.executeStep can spawn for one graph.
func (g *TaskGraph) GetSessionRole(id string) (any, bool) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionRoles == nil {
		return nil, false
	}
	v, ok := g.SessionRoles[id]
	return v, ok
}

// SetSessionRole stores a value for a session-scoped role id -- typically used
// to cache back an already-unmarshaled *config.Role after converting a raw
// map[string]any entry, so later lookups skip the JSON round-trip. Safe for
// concurrent use.
func (g *TaskGraph) SetSessionRole(id string, v any) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionRoles == nil {
		g.SessionRoles = make(map[string]any)
	}
	g.SessionRoles[id] = v
}

// GetSessionSkill / SetSessionSkill: same contract as the Role variants above,
// for SessionSkills.
func (g *TaskGraph) GetSessionSkill(id string) (any, bool) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionSkills == nil {
		return nil, false
	}
	v, ok := g.SessionSkills[id]
	return v, ok
}

func (g *TaskGraph) SetSessionSkill(id string, v any) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionSkills == nil {
		g.SessionSkills = make(map[string]any)
	}
	g.SessionSkills[id] = v
}

// ForEachSessionSkill executes a callback for each session-scoped skill.
// The callback receives the skill ID and its raw value (map[string]any or *config.Skill).
// This provides a thread-safe way to iterate over session skills from other packages.
func (g *TaskGraph) ForEachSessionSkill(fn func(id string, v any)) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	for id, v := range g.SessionSkills {
		fn(id, v)
	}
}

// GetSessionProvider / SetSessionProvider: same contract as the Role variants above,
// for SessionProviders.
func (g *TaskGraph) GetSessionProvider(id string) (any, bool) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionProviders == nil {
		return nil, false
	}
	v, ok := g.SessionProviders[id]
	return v, ok
}

func (g *TaskGraph) SetSessionProvider(id string, v any) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionProviders == nil {
		g.SessionProviders = make(map[string]any)
	}
	g.SessionProviders[id] = v
}

func (g *TaskGraph) ListSessionProviderIDs() []string {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	ids := make([]string, 0, len(g.SessionProviders))
	for id := range g.SessionProviders {
		ids = append(ids, id)
	}
	return ids
}

// RecordDecision adds a decision node to DecisionHistory under decisionMu.
// (P-2.1 Step 2: replaces direct map access + GraphMu.Lock in Spawner)
func (g *TaskGraph) RecordDecision(id string, node *DecisionNode) {
	g.decisionMu.Lock()
	defer g.decisionMu.Unlock()
	if g.DecisionHistory == nil {
		g.DecisionHistory = make(map[string]*DecisionNode)
	}
	g.DecisionHistory[id] = node
}

// UpdateDecisionOutcome sets the outcome on an existing decision node.
// (P-2.1 Step 2: replaces direct map access + GraphMu.Lock in Spawner)
func (g *TaskGraph) UpdateDecisionOutcome(id, outcome string) bool {
	g.decisionMu.Lock()
	defer g.decisionMu.Unlock()
	if node, ok := g.DecisionHistory[id]; ok {
		node.Outcome = outcome
		return true
	}
	return false
}

// LockDecisionHistory acquires a read lock on DecisionHistory. Callers must
// call UnlockDecisionHistory when done. Used by persistGraph to safely marshal
// the graph while the Spawner may be appending decisions.
// (P-2.1 Step 2)
func (g *TaskGraph) LockDecisionHistory()   { g.decisionMu.RLock() }
func (g *TaskGraph) UnlockDecisionHistory() { g.decisionMu.RUnlock() }

// ComputeConfidence returns geometric mean of confidence across completed steps.
func (g *TaskGraph) ComputeConfidence() float64 {
	var scores []float64
	for _, s := range g.Steps {
		if s.Confidence != nil && (s.Status == StepOK || s.Status == StepPartial) {
			scores = append(scores, *s.Confidence)
		}
	}
	if len(scores) == 0 {
		return 0
	}
	product := 1.0
	for _, sc := range scores {
		product *= sc
	}
	return math.Pow(product, 1.0/float64(len(scores)))
}

// ReadySteps returns steps whose dependencies are all satisfied.
func (g *TaskGraph) ReadySteps() []*Step {
	var ready []*Step
	for _, s := range g.Steps {
		if s.Status != StepPending {
			continue
		}
		allDone := true
		for _, depID := range s.DependsOn {
			dep, ok := g.Steps[depID]
			if !ok {
				continue
			}
			if dep.Status != StepOK && dep.Status != StepPartial && dep.Status != StepSkipped {
				allDone = false
				break
			}
		}
		if allDone {
			ready = append(ready, s)
		}
	}
	return ready
}

// FallbackFor returns the fallback step for a given step ID, if one exists.
func (g *TaskGraph) FallbackFor(stepID string) *Step {
	for _, s := range g.Steps {
		if s.FallbackFor == stepID {
			return s
		}
	}
	return nil
}

// IsTerminal returns true when all steps have reached a terminal state.
func (g *TaskGraph) IsTerminal() bool {
	for _, s := range g.Steps {
		if !s.IsTerminal() {
			return false
		}
	}
	return true
}

// ToStatusDict produces a serialisable status snapshot.
func (g *TaskGraph) ToStatusDict() map[string]any {
	return g.ToStatusDictView("full")
}

// ToStatusDictView produces a serialisable status snapshot based on the requested view.
// Supported views: "summary", "full" (default).
func (g *TaskGraph) ToStatusDictView(view string) map[string]any {
	if view == "summary" {
		completed := 0
		var activeStepID string
		var lastError string
		for _, s := range g.Steps {
			if s.Status == StepOK || s.Status == StepPartial || s.Status == StepSkipped {
				completed++
			}
			if s.Status == StepRunning || s.Status == StepBlocked {
				activeStepID = s.ID
			}
			if s.LastError != "" {
				lastError = s.LastError
			}
		}

		return map[string]any{
			"task_id":        g.TaskID,
			"status":         string(g.Status),
			"progress":       fmt.Sprintf("%d/%d", completed, len(g.Steps)),
			"active_step_id": activeStepID,
			"last_error":     lastError,
			"created_at":     g.CreatedAt.Format(time.RFC3339),
			// Issue-1 (2026-10-23): the engine keeps task status "running" while
			// a step is blocked awaiting a decision — see scheduler_decision_
			// gate.go ("we do NOT set graph.Status = GraphBlocked"). A
			// summary-only caller therefore cannot tell "still working" from
			// "waiting for me" and may misreport a stalled task as live. This
			// non-sensitive count is the disambiguator (the decisions themselves
			// stay in the full view). Safe: every summary caller holds the
			// engine mutex or passed a locally-unmarshalled graph.
			"pending_decision_count": len(g.PendingDecisions),
			// Substitution count: non-zero means actual execution diverged from
			// the request (role fallback, MCP degradation, etc.). See
			// docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P3.
			"substitution_count": g.SubstitutionCount(),
			// Charter present: non-nil means the task has an explicit intent /
			// non-goals / constraints charter. See §P1.
			"has_charter": g.Charter != nil,
			"fastpath":    g.FastPath,
		}
	}

	steps := make(map[string]any, len(g.Steps))
	for id, s := range g.Steps {
		steps[id] = map[string]any{
			"id":                s.ID,
			"role_id":           s.RoleID,
			"task":              s.Task,
			"output_contract":   s.OutputContract,
			"depends_on":        s.DependsOn,
			"read_deps":         s.ReadDeps,
			"additional_skills": s.AdditionalSkills,
			"status":            string(s.Status),
			"confidence":        s.Confidence,
			"retry_count":       s.RetryCount,
			"result_ref":        s.ResultRef,
			"missing_context":   s.MissingCtx,
			"last_error":        s.LastError,
			"trigger_error":     s.TriggerError,
			"last_worked_on":    s.LastWorkedOn.Format(time.RFC3339),
			"merge_strategy":    s.MergeStrategy,
			"target_key":        s.TargetKey,
			"input_mapping":     s.InputMapping,
			"isolation":         s.Isolation,
			"failure_policy":    s.FailurePolicy,
		}
	}

	decisions := make([]map[string]any, len(g.PendingDecisions))
	for i, d := range g.PendingDecisions {
		// Sanitize context for decisions
		safeCtx := make(map[string]any)
		for k, v := range d.Context {
			if k != "embedding" && k != "embedding_model" {
				safeCtx[k] = v
			}
		}

		decisions[i] = map[string]any{
			"id":      d.ID,
			"step_id": d.StepID,
			"type":    d.Type,
			"context": safeCtx,
			"options": d.Options,
			"created": d.Created.Format(time.RFC3339),
		}
	}

	files := make([]map[string]any, len(g.OutputFiles))
	for i, f := range g.OutputFiles {
		files[i] = map[string]any{
			"step_id":    f.StepID,
			"path":       f.Path,
			"format":     f.Format,
			"summary":    f.Summary,
			"size_bytes": f.SizeBytes,
			"is_primary": f.IsPrimary,
			"created":    f.CreatedAt.Format(time.RFC3339),
		}
	}

	skippedStepsCount := 0
	for _, s := range g.Steps {
		if s.Status == StepSkipped {
			skippedStepsCount++
		}
	}

	// Sanitize DecisionHistory for status reports (hide heavy embeddings)
	sanitizedHistory := make(map[string]map[string]any)
	g.decisionMu.RLock()
	for id, node := range g.DecisionHistory {
		reasoning := node.Reasoning
		if len(reasoning) > 200 {
			reasoning = reasoning[:200] + "..."
		}
		sanitizedHistory[id] = map[string]any{
			"id":        node.ID,
			"step_id":   node.StepID,
			"turn":      node.Turn,
			"type":      node.Type,
			"reasoning": reasoning,
			"action":    node.Action,
			"outcome":   node.Outcome,
			"causes":    node.Causes,
			"timestamp": node.Timestamp.Format(time.RFC3339),
		}
	}
	g.decisionMu.RUnlock()

	// Sanitize ContextTree for status reports
	sanitizedTree := make(map[string]map[string]any)
	for id, node := range g.ContextTree {
		// Sanitize metadata
		safeMeta := make(map[string]any)
		for k, v := range node.Metadata {
			if k != "embedding" && k != "embedding_model" {
				safeMeta[k] = v
			}
		}

		sanitizedTree[id] = map[string]any{
			"id":        node.ID,
			"parent_id": node.ParentID,
			"intent":    node.Intent,
			"goal":      node.Goal,
			"status":    string(node.Status),
			"summary":   node.Summary,
			"step_ids":  node.StepIDs,
			"metadata":  safeMeta,
		}
	}

	// Sanitize GlobalWorkspace: strip heavy system_prompt/user_prompt from
	// each entry unless the corresponding step is in delegation_required status.
	// These prompts (anti-pattern warnings, JIT suggestions, output contracts)
	// can be 5000+ chars each and are only needed for delegation fulfillment.
	sanitizedWorkspace := make(map[string]any, len(g.GlobalWorkspace))
	delegationSteps := make(map[string]bool)
	for _, s := range g.Steps {
		if s.Status == StepBlocked && s.TriggerError == "delegation_required" {
			delegationSteps[s.ID] = true
		}
	}
	anyDelegation := len(delegationSteps) > 0
	for k, v := range g.GlobalWorkspace {
		keepPrompts := false
		if k == "result" {
			keepPrompts = anyDelegation
		} else if delegationSteps[k] {
			keepPrompts = true
		}
		if !keepPrompts {
			if entry, ok := v.(map[string]any); ok {
				stripped := make(map[string]any, len(entry))
				for ek, ev := range entry {
					if ek == "system_prompt" || ek == "user_prompt" {
						continue
					}
					stripped[ek] = ev
				}
				sanitizedWorkspace[k] = stripped
				continue
			}
		}
		sanitizedWorkspace[k] = v
	}

	res := map[string]any{
		"task_id":             g.TaskID,
		"status":              string(g.Status),
		"overall_confidence":  g.OverallConfidence,
		"skipped_steps_count": skippedStepsCount,
		"steps":               steps,
		"pending_decisions":   decisions,
		"output_files":        files,
		"artifacts":           g.Artifacts,
		"global_workspace":    sanitizedWorkspace,
		"created_at":          g.CreatedAt.Format(time.RFC3339),
		"session_id":          g.SessionID,
		// WorkspaceRoot is where the step agents' core tools resolved relative
		// paths. Callers that want to read a task's deliverables need it: the
		// files land under this root, not under outputs/<taskID>. Empty means
		// the task ran with the legacy per-task directory.
		"workspace_root":     g.WorkspaceRoot,
		"session_roles":      g.SessionRoles,
		"session_skills":     g.SessionSkills,
		"coordination_edges": g.CoordinationEdges,
		"decision_history":   sanitizedHistory,
		"context_tree":       sanitizedTree,
		"current_node_id":    g.CurrentNodeID,
		// Substitution ledger: every instance where actual execution diverged
		// from the request. See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P3.
		"substitutions": g.GetSubstitutions(),
		// Task charter: intent, non-goals, constraints. See §P1.
		"charter": g.Charter,
		// FastPath: true when the task took the single-step inline branch
		// (no planner expansion, no review gate). See §M of
		// DELEGATION_INTENT_TRIGGER_ANALYSIS.md (now in docs/completed/2026-11-24/).
		"fastpath": g.FastPath,
	}
	if g.CompletedAt != nil {
		res["completed_at"] = g.CompletedAt.Format(time.RFC3339)
	}
	if g.Delivery != nil {
		res["delivery"] = g.Delivery
	}
	return res
}

// ─── ID generators ────────────────────────────────────────────────────────

func NewTaskID() string {
	return fmt.Sprintf("task_%s", uuid.New().String()[:8])
}

func NewDecisionID() string {
	return fmt.Sprintf("dec_%s", uuid.New().String()[:6])
}
