package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestCheckApprovalAuthority_ApproverLevels (Stage 1, 2026-10-24): an
// admin-level MCP (Approver="" / "admin") requires an admin resolver; an
// owner-level MCP (Approver="owner") allows the task owner (self-approve) or an
// admin. See docs/plan/DECISION_APPROVAL_AUTHORITY_DESIGN.md §7.
func TestCheckApprovalAuthority_ApproverLevels(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()
	s.registry.MCPs = map[string]*config.MCPDef{
		"admin_mcp": {ID: "admin_mcp"}, // Approver="" ⇒ admin
		"owner_mcp": {ID: "owner_mcp", Approver: "owner"},
	}

	graph := &schemas.TaskGraph{
		TaskID:  "task-auth",
		OwnerID: "owner-1",
		Steps:   map[string]*schemas.Step{},
		Status:  schemas.GraphBlocked,
		PendingDecisions: []*schemas.Decision{
			{ID: "dec-admin", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired, Created: time.Now(),
				Context: map[string]any{"approval_required_mounts": []string{"admin_mcp"}}, Options: []string{"approve", "reject"}},
			{ID: "dec-owner", StepID: "s2", Type: schemas.DecisionHumanApprovalRequired, Created: time.Now(),
				Context: map[string]any{"approval_required_mounts": []string{"owner_mcp"}}, Options: []string{"approve", "reject"}},
		},
	}
	s.Mu.Lock()
	s.graphs["task-auth"] = graph
	s.Mu.Unlock()

	// admin-level: non-admin rejected; admin OK.
	if err := s.checkApprovalAuthority("task-auth", "dec-admin", Resolver{IsAdmin: false}); err == nil {
		t.Fatal("non-admin must be rejected from an admin-level decision")
	}
	if err := s.checkApprovalAuthority("task-auth", "dec-admin", Resolver{IsAdmin: true}); err != nil {
		t.Fatalf("admin must approve admin-level, got: %v", err)
	}
	// owner-level: the task owner (matching OwnerID) OK; a different owner rejected; admin OK.
	if err := s.checkApprovalAuthority("task-auth", "dec-owner", Resolver{OwnerID: "owner-1"}); err != nil {
		t.Fatalf("task owner must self-approve owner-level, got: %v", err)
	}
	if err := s.checkApprovalAuthority("task-auth", "dec-owner", Resolver{OwnerID: "someone-else"}); err == nil {
		t.Fatal("a non-owner non-admin must be rejected from an owner-level decision")
	}
	if err := s.checkApprovalAuthority("task-auth", "dec-owner", Resolver{IsAdmin: true}); err != nil {
		t.Fatalf("admin must approve owner-level, got: %v", err)
	}
}

// TestCheckApprovalAuthority_Expiry (Stage 1, 2026-10-24): with expiry disabled
// (ApprovalExpiry=0) an old decision stays approvable; with a 30m expiry a 2h-
// old decision is rejected.
func TestCheckApprovalAuthority_Expiry(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()
	zero := 0
	s.registry.System.ApprovalExpiry = &zero // never expires
	s.registry.MCPs = map[string]*config.MCPDef{"m": {ID: "m", Approver: "owner"}}
	graph := &schemas.TaskGraph{
		TaskID:  "task-exp",
		OwnerID: "owner-1",
		Steps:   map[string]*schemas.Step{},
		Status:  schemas.GraphBlocked,
		PendingDecisions: []*schemas.Decision{{
			ID: "dec-old", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired,
			Created: time.Now().Add(-2 * time.Hour),
			Context: map[string]any{"approval_required_mounts": []string{"m"}},
			Options: []string{"approve", "reject"},
		}},
	}
	s.Mu.Lock()
	s.graphs["task-exp"] = graph
	s.Mu.Unlock()

	// expiry disabled → old decision still approvable.
	if err := s.checkApprovalAuthority("task-exp", "dec-old", Resolver{OwnerID: "owner-1"}); err != nil {
		t.Fatalf("with expiry disabled, an old decision must still be approvable, got: %v", err)
	}
	// enable 30m expiry → the 2h-old decision is expired.
	thirty := 1800
	s.registry.System.ApprovalExpiry = &thirty
	if err := s.checkApprovalAuthority("task-exp", "dec-old", Resolver{OwnerID: "owner-1"}); err == nil {
		t.Fatal("an expired decision must be rejected")
	}
}
