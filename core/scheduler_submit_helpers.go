package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── Submit helpers ────────────────────────────────────────────────────────

// taskSubmittedDetail is the task_submitted log payload. It carries a compact
// dependency snapshot so replay viewers can redraw the execution DAG without
// reading the (much larger) persisted manifest.
func taskSubmittedDetail(stepCount int, requireReview bool, graph *schemas.TaskGraph) map[string]any {
	detail := map[string]any{
		"step_count":     stepCount,
		"require_review": requireReview,
	}
	if deps := stepDependencySnapshot(graph); len(deps) > 0 {
		detail["deps"] = deps
	}
	// A fully parallel plan has no dependency edges at all, so `deps` alone
	// cannot tell replay that its steps are one explicit fan-out. Record the
	// complete planned node set: an edge-less graph is still a graph, and its
	// dependency-free nodes are exactly the parallel roots.
	if ids := stepIDSnapshot(graph); len(ids) > 0 {
		detail["step_ids"] = ids
	}
	return detail
}

// stepIDSnapshot lists every planned step, including dependency-free roots
// that stepDependencySnapshot deliberately omits. Sorted so the log payload is
// stable across runs with the same plan.
func stepIDSnapshot(graph *schemas.TaskGraph) []string {
	if graph == nil {
		return nil
	}
	ids := make([]string, 0, len(graph.Steps))
	for id, step := range graph.Steps {
		if step == nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// stepDependencySnapshot keeps only steps that actually declare upstream
// dependencies; fully parallel plans would otherwise log an empty map.
func stepDependencySnapshot(graph *schemas.TaskGraph) map[string][]string {
	var deps map[string][]string
	for id, step := range graph.Steps {
		if step == nil || len(step.DependsOn) == 0 {
			continue
		}
		if deps == nil {
			deps = make(map[string][]string, len(graph.Steps))
		}
		deps[id] = append([]string(nil), step.DependsOn...)
	}
	return deps
}

// applySessionAssets records the caller-supplied roles, skills and provider
// configs on the graph so later steps resolve them without re-reading config.
// Nil entries are skipped.
func applySessionAssets(graph *schemas.TaskGraph, roles []*config.Role, skills []*config.Skill, providers []*config.ProviderConfig) {
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

// pinBaseline records the SOP/Role version at task creation time so resume
// uses the pinned version instead of the live registry. Nil-safe: skips
// when BaselineStore is nil or no SOP/Role is referenced.
// See docs/BASELINE_VERSION_PINNING_DESIGN.md Step 4.
func (s *DirectedEngine) pinBaseline(taskID string, inputs []schemas.StepInput) {
	if s.BaselineStore == nil || len(inputs) == 0 {
		return
	}

	inp := inputs[0]
	var sopID, sopVer, roleID, roleVer string

	if inp.SOPRef != "" {
		s.registry.Mu.RLock()
		if sop, ok := s.registry.SOPs[inp.SOPRef]; ok {
			sopID, sopVer = sop.ID, sop.Version
		}
		s.registry.Mu.RUnlock()
	}
	if inp.RoleID != "" {
		s.registry.Mu.RLock()
		if role, ok := s.registry.Roles[inp.RoleID]; ok {
			roleID, roleVer = role.ID, role.Version
		}
		s.registry.Mu.RUnlock()
	}

	if sopID == "" && roleID == "" {
		return
	}

	ctx := ContextWithTrace(s.lifecycleCtx, "", "")
	if err := s.BaselineStore.PinBaseline(ctx, taskID, sopID, sopVer, roleID, roleVer); err != nil {
		s.logger.Log("EventBaselinePinError", taskID, "", map[string]any{"error": err.Error()})
	}
}

// validateWorkspaceScope is the Complete Mediation (I3) defense-in-depth
// check. When workspace policy is enabled and an owner is present, it
// verifies that workspaceRoot is within the allowed roots.
//
// This catches direct API handler bypasses (pkg/api/handlers.go,
// pkg/api/chat_handlers.go) that skip the tools layer's
// ResolveWorkspaceRoot. Nil-safe: when policy is disabled or owner is
// empty (local mode), the check is a no-op.
func (s *DirectedEngine) validateWorkspaceScope(workspaceRoot, ownerID string) error {
	if workspaceRoot == "" || ownerID == "" {
		return nil // empty root or local mode → no check
	}
	wp := s.registry.GetWorkspacePolicy(ownerID)
	if wp == nil || !wp.Enabled {
		return nil // policy disabled → no check
	}
	if len(wp.AllowedRoots) == 0 {
		return nil // no whitelist configured → allow
	}
	for _, allowed := range wp.AllowedRoots {
		cleanAllowed := filepath.Clean(allowed)
		cleanRoot := filepath.Clean(workspaceRoot)
		if cleanRoot == cleanAllowed ||
			strings.HasPrefix(cleanRoot, cleanAllowed+string(filepath.Separator)) {
			return nil // within allowed root
		}
	}
	return fmt.Errorf("workspace %s not in allowed roots for owner %s", workspaceRoot, ownerID)
}

// anySubmitterIsAdmin reports whether any submitted step carries the admin
// tier. All steps in one submission share the caller's tier, so this records
// it once on the task graph (see TaskGraph.SubmitterIsAdmin).
func anySubmitterIsAdmin(inputs []schemas.StepInput) bool {
	for i := range inputs {
		if inputs[i].SubmitterIsAdmin {
			return true
		}
	}
	return false
}

// defaultProjectID normalizes a project identifier. Empty values fall back to
// the "default" project space, which preserves backward compatibility for
// callers that do not opt into project scoping (Phase 2 of the Project Space
// & Memory Tiering design).
func defaultProjectID(pid string) string {
	if pid == "" {
		return "default"
	}
	return pid
}

// resolveProjectID extracts the project identifier from a batch of submitted
// steps. The first non-empty ProjectID wins; if none is set, the task is
// assigned to the "default" project. This mirrors anySubmitterIsAdmin's
// single-submission-tier assumption.
func resolveProjectID(inputs []schemas.StepInput) string {
	for i := range inputs {
		if inputs[i].ProjectID != "" {
			return inputs[i].ProjectID
		}
	}
	return "default"
}
