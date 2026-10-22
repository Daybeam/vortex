package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestRegisterRole_RejectsNonexistentBoundSkill is a direct regression test

// TestRegisterGroup_RejectsNonexistentMemberRole is a direct regression
// test mirroring TestRegisterRole_RejectsNonexistentBoundSkill above, for
// register_group's equivalent dropped validation: every member's role_id
// must reference an already-registered role.
func TestRegisterGroup_RejectsNonexistentMemberRole(t *testing.T) {
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	app := &App{Registry: reg}
	RegisterSubsystems(app)

	action, ok := findConfigAction("register_group")
	if !ok {
		t.Fatalf("register_group action not registered")
	}

	_, err = action.Handler(context.Background(), app, map[string]any{
		"id":     "test_group_bad_member",
		"name":   "Test Group",
		"policy": "sequential",
		"members": []any{
			map[string]any{"role_id": "this_role_does_not_exist"},
		},
	})
	if err == nil {
		t.Fatal("expected an error registering a group with a nonexistent member role_id, got nil")
	}

	reg.Mu.RLock()
	_, exists := reg.RoleGroups["test_group_bad_member"]
	reg.Mu.RUnlock()
	if exists {
		t.Fatal("group should not have been registered when validation failed")
	}
}

// TestRegisterGroup_AllowsExistingMemberRole confirms the fix is not a
// blanket rejection -- a group whose member references a real,
// already-registered role must still register successfully.
func TestRegisterGroup_AllowsExistingMemberRole(t *testing.T) {
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	app := &App{Registry: reg}
	RegisterSubsystems(app)

	// register_role action was removed (feat f41bcbf34, task-routing update);
	// insert the member role directly instead of registering it via the API.
	reg.Mu.Lock()
	reg.Roles["test_group_member_role"] = &config.Role{
		ID: "test_group_member_role", Name: "Test Role", BaseCapability: "code",
	}
	reg.Mu.Unlock()

	groupAction, ok := findConfigAction("register_group")
	if !ok {
		t.Fatalf("register_group action not registered")
	}
	_, err = groupAction.Handler(context.Background(), app, map[string]any{
		"id":     "test_group_good_member",
		"name":   "Test Group",
		"policy": "sequential",
		"members": []any{
			map[string]any{"role_id": "test_group_member_role"},
		},
	})
	if err != nil {
		t.Fatalf("register_group handler: %v", err)
	}

	reg.Mu.RLock()
	_, exists := reg.RoleGroups["test_group_good_member"]
	reg.Mu.RUnlock()
	if !exists {
		t.Fatal("group should have been registered")
	}
}

// findConfigAction locates a registered Action within the "config"
// subsystem via the package-level global `registry` (tools/subsystems.go),
// mirroring subsystems_update_system_test.go's findUpdateSystemAction.
func findConfigAction(name string) (Action, bool) {
	sub, ok := registry.Subsystems["config"]
	if !ok {
		return Action{}, false
	}
	act, ok := sub.Actions[name]
	return act, ok
}
