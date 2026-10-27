package core

import (
	"errors"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// mount_gate.go — submitter-tier + approval short-circuit helpers for the
// MCP/skill mount gates (G2 for MCPs, D3 for skills).

// effectiveSubmitterIsAdmin reports whether the caller who submitted the task
// owning this spawn is admin tier.
//
// SECURITY FIX (2026-10-21, issue R1): the mount gate previously received a
// hardcoded `true` from the execution path, so planner/replan/surgery steps
// could mount arbitrary MCPs regardless of the submitter. The submitter's tier
// now travels on the task graph (TaskGraph.SubmitterIsAdmin, recorded once at
// Submit time from StepInput.SubmitterIsAdmin, which is json:"-" so a caller
// cannot forge it). Reading it here covers every spawn — including
// engine-internal sub-spawns (planner/auditor/arbiter/replay) that build their
// own SpawnRequest without touching this field.
//
// ⚠ CORRECTION (2026-10-26): the last sentence above was FALSE and cost a
// 5.5 h eval round. Building a SpawnRequest "without touching the field" only
// stays safe if the spawn also carries a Hub — and the internal sub-spawns do
// not. prepareSpawn then substitutes NewContextHub(reg, nil, …) whose Graph is
// nil, so this function returned false and every internal sub-spawn was judged
// a public caller. Use submitterIsAdminFor(req) instead of calling this
// directly, so a sub-spawn can carry its tier on the request.
func effectiveSubmitterIsAdmin(hub *ContextHub) bool {
	return hub != nil && hub.Graph != nil && hub.Graph.SubmitterIsAdmin
}

// submitterIsAdminFor is the mount gate's single tier lookup: the request's own
// SubmitterIsAdmin (set by engine sub-spawns from the owning graph) wins, and
// the hub's graph is the fallback for externally-driven spawns.
//
// Fail-closed is preserved: a request with neither yields false.
func submitterIsAdminFor(req *SpawnRequest) bool {
	if req == nil {
		return false
	}
	if req.SubmitterIsAdmin {
		return true
	}
	return effectiveSubmitterIsAdmin(req.Hub)
}

// approvalResult converts a *MountApprovalRequiredError into the
// StatusApprovalRequired short-circuit SpawnResult (G2; skills added D3,
// 2026-10-22), or nil when err is some other failure.
func (s *Spawner) approvalResult(role *config.Role, err error) *SpawnResult {
	var ae *MountApprovalRequiredError
	if !errors.As(err, &ae) {
		return nil
	}
	return &SpawnResult{Output: schemas.SubagentOutput{
		Status:                 schemas.StatusApprovalRequired,
		Confidence:             1.0,
		Capability:             role.BaseCapability,
		ApprovalRequiredMounts: ae.MCPIDs,
		ApprovalRequiredSkills: ae.SkillIDs,
	}}
}

// resolveSkills resolves the spawn's final skills with the submitter-tier
// gate. On a hard gate error it returns (nil, nil, err); on an `approval`
// skill it returns (nil, result, nil) — the same short-circuit shape as
// resolveMCPBindings (D3, 2026-10-22).
func (s *Spawner) resolveSkills(req *SpawnRequest, role *config.Role, additionalSkills []string) ([]string, *SpawnResult, error) {
	final, err := ResolveFinalSkills(req.Hub, req.RoleID, additionalSkills, submitterIsAdminFor(req))
	if err == nil {
		return final, nil, nil
	}
	if sr := s.approvalResult(role, err); sr != nil {
		return nil, sr, nil
	}
	return nil, nil, err
}
