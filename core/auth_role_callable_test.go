package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestSubmit_NonAdminCannotInvokeAdminOnlyRole is the regression test for
// AUTH-1 / G11 (2026-10-23): a role that declares `CallableTiers` excluding the
// public tier must not be invocable by a non-admin submitter. Before the fix
// there was no role-level callable declaration, so a public caller could name
// any role_id and inherit that role's identity.
func TestSubmit_NonAdminCannotInvokeAdminOnlyRole(t *testing.T) {
	engine := newLifecycleEngine(t)

	engine.registry.Mu.Lock()
	engine.registry.Roles["admin_only_role"] = &config.Role{
		ID:            "admin_only_role",
		BaseCapability: "text",
		CallableTiers: []string{"admin"},
	}
	engine.registry.Mu.Unlock()

	// Non-admin: rejected at the engine chokepoint, before any spawn.
	_, err := engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "admin_only_role", Task: "do it"}})
	if err == nil || !strings.Contains(err.Error(), "not callable") {
		t.Fatalf("non-admin submit of an admin-only role must be rejected, got %v", err)
	}

	// Admin: passes the tier gate (no "not callable" rejection).
	_, err = engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "admin_only_role", Task: "do it", SubmitterIsAdmin: true}})
	if err != nil && strings.Contains(err.Error(), "not callable") {
		t.Fatalf("admin submit must pass the tier gate, got %v", err)
	}

	// An ordinary role (no CallableTiers) stays open to non-admins.
	_, err = engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "worker", Task: "do it"}})
	if err != nil && strings.Contains(err.Error(), "not callable") {
		t.Fatalf("an open role must remain callable by non-admins, got %v", err)
	}
}