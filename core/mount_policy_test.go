package core

import (
	"errors"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// mountPolicyHub builds a hub whose role binds one MCP and exposes one MCP per
// mount policy. All MCPs are known (present in the registry), so the only gate
// under test is the mount policy, not "unknown MCP".
func mountPolicyHub() *ContextHub {
	return NewContextHub(&config.Registry{
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BoundMCPBindings: []config.MCPBinding{{MCPID: "bound_mcp"}}},
		},
		MCPs: map[string]*config.MCPDef{
			"bound_mcp":   {ID: "bound_mcp", MountPolicy: config.MountPolicyOpen}, // role "worker"'s own MCP — safe for any tier (audit #3)
			"open_mcp":    {ID: "open_mcp", MountPolicy: config.MountPolicyOpen},
			"approv_mcp":  {ID: "approv_mcp", MountPolicy: config.MountPolicyApproval},
			"admin_mcp":   {ID: "admin_mcp", MountPolicy: config.MountPolicyAdminOnly},
			"default_mcp": {ID: "default_mcp"}, // empty policy ⇒ admin_only
		},
	}, nil, nil)
}

// Regression (security audit R3/G7, 2026-10-21): an MCP already bound to the
// role is NOT a dynamic mount and must be allowed for any submitter tier —
// this is the G1/G11 side effect fix (public callers blocked from re-specifying
// their role's own MCPs).
func TestResolveFinalMCPs_BoundMCPAllowedForNonAdmin(t *testing.T) {
	hub := mountPolicyHub()
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"bound_mcp"}, nil, false); err != nil {
		t.Fatalf("bound MCP must be allowed for a non-admin submitter, got: %v", err)
	}
}

// Regression (security audit R3/G7, 2026-10-21): mount_policy=open allows a
// non-admin submitter to mount an unbound MCP.
func TestResolveFinalMCPs_OpenPolicyAllowsNonAdmin(t *testing.T) {
	hub := mountPolicyHub()
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"open_mcp"}, nil, false); err != nil {
		t.Fatalf("open-policy MCP must be mountable by non-admin, got: %v", err)
	}
}

// Regression (security audit R3/G7, 2026-10-21): mount_policy=approval gates a
// non-admin mount behind human approval (recognizable sentinel), but admin
// mounts it directly.
func TestResolveFinalMCPs_ApprovalPolicy(t *testing.T) {
	hub := mountPolicyHub()

	if _, err := ResolveFinalMCPs(hub, "worker", []string{"approv_mcp"}, nil, false); !errors.Is(err, ErrMountApprovalRequired) {
		t.Fatalf("non-admin approval-policy mount must return ErrMountApprovalRequired, got: %v", err)
	}
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"approv_mcp"}, nil, true); err != nil {
		t.Fatalf("admin must mount an approval-policy MCP without approval, got: %v", err)
	}
}

// Regression (security audit R3/G7, 2026-10-21): admin_only (and the empty
// default) reject a non-admin dynamic mount and allow an admin one.
func TestResolveFinalMCPs_AdminOnlyPolicy(t *testing.T) {
	hub := mountPolicyHub()

	for _, mcp := range []string{"admin_mcp", "default_mcp"} {
		if _, err := ResolveFinalMCPs(hub, "worker", []string{mcp}, nil, false); err == nil {
			t.Fatalf("%s must reject a non-admin dynamic mount", mcp)
		}
		if _, err := ResolveFinalMCPs(hub, "worker", []string{mcp}, nil, true); err != nil {
			t.Fatalf("admin must mount %s, got: %v", mcp, err)
		}
	}
}

// Regression (security audit R1, 2026-10-21): the submitter tier is read from
// the task graph so engine-internal sub-spawns inherit it instead of being
// hardcoded to admin.
func TestEffectiveSubmitterIsAdmin(t *testing.T) {
	if effectiveSubmitterIsAdmin(nil) {
		t.Fatal("nil hub must not be admin")
	}
	if effectiveSubmitterIsAdmin(&ContextHub{}) {
		t.Fatal("hub without graph must not be admin")
	}
	if !effectiveSubmitterIsAdmin(&ContextHub{Graph: &schemas.TaskGraph{SubmitterIsAdmin: true}}) {
		t.Fatal("graph marking an admin submitter must be admin")
	}
	if effectiveSubmitterIsAdmin(&ContextHub{Graph: &schemas.TaskGraph{SubmitterIsAdmin: false}}) {
		t.Fatal("public submitter must not be admin")
	}
}

// Regression (security audit R1/G1, 2026-10-22): the execution-path mount gate
// reads the submitter tier FROM THE GRAPH (spawner_prepare.go calls
// ResolveFinalMCPs with effectiveSubmitterIsAdmin(hub)). This test pins that
// wiring end-to-end: a PUBLIC-submitted graph must NOT be able to mount an
// admin_only MCP, while an admin-submitted graph must. The two halves are
// covered in isolation above; this ties them together so a future change to
// either side (graph read or policy gate) can't silently reopen the hole.
func TestResolveFinalMCPs_PublicGraphCannotMountAdminOnlyMCP(t *testing.T) {
	hub := mountPolicyHub()
	hub.Graph = &schemas.TaskGraph{SubmitterIsAdmin: false}

	admin := effectiveSubmitterIsAdmin(hub)
	if admin {
		t.Fatal("public-submitted graph must not be treated as admin")
	}
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"admin_mcp"}, nil, admin); err == nil {
		t.Fatal("public submitter must not mount an admin_only MCP via the graph-driven gate")
	}

	// Admin-submitted graph inherits admin through the same path.
	hub.Graph = &schemas.TaskGraph{SubmitterIsAdmin: true}
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"admin_mcp"}, nil, effectiveSubmitterIsAdmin(hub)); err != nil {
		t.Fatalf("admin-submitted graph must mount an admin_only MCP, got: %v", err)
	}
}

// Regression (security audit #3, 2026-10-22): a role BOUND to an admin_only MCP
// must not be usable by a non-admin submitter — otherwise naming any role_id
// hands a public caller that role's privileged tools. This flips the previous
// "bound ⇒ always allowed" rule for privileged MCPs; safe MCPs must be marked
// mount_policy=open explicitly.
func TestResolveFinalMCPs_RoleBoundToAdminOnlyMCPRejectedForNonAdmin(t *testing.T) {
	hub := NewContextHub(&config.Registry{
		Roles: map[string]*config.Role{
			"privileged": {ID: "privileged", BoundMCPBindings: []config.MCPBinding{{MCPID: "machine_mcp"}}},
		},
		MCPs: map[string]*config.MCPDef{
			"machine_mcp": {ID: "machine_mcp"}, // empty policy ⇒ admin_only
		},
	}, nil, nil)

	if _, err := ResolveFinalMCPs(hub, "privileged", nil, nil, false); err == nil {
		t.Fatal("SECURITY: non-admin must not use a role bound to an admin_only MCP")
	}
	if _, err := ResolveFinalMCPs(hub, "privileged", nil, nil, true); err != nil {
		t.Fatalf("admin must use the role, got: %v", err)
	}
}

// Regression (G2 producer, 2026-10-22): an `approval` mount for a non-admin
// returns a *MountApprovalRequiredError naming the MCPs (instead of a bare
// failure), and a graph-recorded human approval unblocks the same mount.
func TestResolveFinalMCPs_ApprovalRequiredErrorAndRecordedApproval(t *testing.T) {
	hub := mountPolicyHub()

	_, err := ResolveFinalMCPs(hub, "worker", []string{"approv_mcp"}, nil, false)
	var approvalErr *MountApprovalRequiredError
	if !errors.As(err, &approvalErr) {
		t.Fatalf("expected *MountApprovalRequiredError, got: %v", err)
	}
	if len(approvalErr.MCPIDs) != 1 || approvalErr.MCPIDs[0] != "approv_mcp" {
		t.Fatalf("expected MCPIDs [approv_mcp], got %v", approvalErr.MCPIDs)
	}
	if !errors.Is(err, ErrMountApprovalRequired) {
		t.Fatal("typed error must still satisfy errors.Is(ErrMountApprovalRequired)")
	}

	// A human approval recorded on the graph unblocks the same mount.
	hub.Graph = &schemas.TaskGraph{}
	hub.Graph.ApproveMCPMounts([]string{"approv_mcp"})
	if _, err := ResolveFinalMCPs(hub, "worker", []string{"approv_mcp"}, nil, false); err != nil {
		t.Fatalf("approved mount must be allowed, got: %v", err)
	}
}

// Regression (G2 producer, 2026-10-22): an approval-required spawn result must
// raise a human_approval_required decision naming the blocked MCPs, instead of
// failing the step outright.
func TestHandleStepFailure_ApprovalEmitsHumanApprovalDecision(t *testing.T) {
	s := newFailEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-g2-approval",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning, RoleID: "worker", Task: "deploy"}
	graph.Steps["s1"] = step
	s.Mu.Lock()
	s.graphs["task-g2-approval"] = graph
	s.Mu.Unlock()

	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:                 schemas.StatusApprovalRequired,
		Confidence:             1.0,
		ApprovalRequiredMounts: []string{"approv_mcp"},
	}}
	s.handleStepFailure(graph, step, result)

	s.Mu.RLock()
	defer s.Mu.RUnlock()
	var found *schemas.Decision
	for _, d := range graph.PendingDecisions {
		if d.Type == schemas.DecisionHumanApprovalRequired {
			found = d
		}
	}
	if found == nil {
		t.Fatal("expected a human_approval_required decision")
	}
	mounts, _ := found.Context["approval_required_mounts"].([]string)
	if len(mounts) != 1 || mounts[0] != "approv_mcp" {
		t.Fatalf("decision must name [approv_mcp], got %v", found.Context["approval_required_mounts"])
	}
}

// Regression (G2 recovery, 2026-10-22): SubmitDecision "approve" on a mount
// decision records the approved MCPs on the graph and re-queues the blocked
// step (which never ran) — so the next ResolveFinalMCPs allows the mount.
func TestSubmitDecision_ApproveMountRecordsAndRequeues(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-g2-submit",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphBlocked,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepBlocked, RoleID: "worker", Task: "deploy"}
	graph.Steps["s1"] = step
	graph.PendingDecisions = []*schemas.Decision{{
		ID:      "dec-g2",
		StepID:  "s1",
		Type:    schemas.DecisionHumanApprovalRequired,
		Context: map[string]any{"approval_required_mounts": []string{"approv_mcp"}},
		Options: []string{"approve", "reject"},
	}}
	s.Mu.Lock()
	s.graphs["task-g2-submit"] = graph
	s.Mu.Unlock()

	if err := s.SubmitDecision("task-g2-submit", "dec-g2", "approve"); err != nil {
		t.Fatalf("SubmitDecision approve: %v", err)
	}

	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if !graph.IsMCPMountApproved("approv_mcp") {
		t.Fatal("approve must record the approved MCP on the graph")
	}
	if step.Status != schemas.StepPending {
		t.Fatalf("approve must re-queue the blocked step, got %q", step.Status)
	}
}
