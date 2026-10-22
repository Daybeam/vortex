package tools

import (
	"context"
	"testing"
)

// Security regression tests for subsystem_config tier guards.
//
// These tests pin the TIER_GATED_WORKSPACE_SCOPE_DESIGN.md Change 1 fix (2026-10-21):
// public tier callers must NOT be able to modify global workspace assets
// (roles, SOPs, skills) via subsystem_config actions.

func TestSubsysConfig_PublicTierCannotUpdateSOP(t *testing.T) {
	app := &App{
		Registry: emptyRegistryForTierTest(t),
		Tier:     TierPublic,
	}
	registerConfigSubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	_, err := registry.Invoke(ctx, app, "config", "update_sop", map[string]any{
		"id":          "test_sop",
		"description": "test",
	})
	if err == nil {
		t.Fatal("SECURITY: public tier must NOT be able to update SOPs")
	}
}

func TestSubsysConfig_PublicTierCannotRegisterRole(t *testing.T) {
	app := &App{
		Registry: emptyRegistryForTierTest(t),
		Tier:     TierPublic,
	}
	registerConfigSubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	_, err := registry.Invoke(ctx, app, "config", "register_role", map[string]any{
		"id":              "test_role",
		"name":            "Test",
		"base_capability": "code",
	})
	if err == nil {
		t.Fatal("SECURITY: public tier must NOT be able to register roles")
	}
}

func TestSubsysConfig_PublicTierCannotRegisterSkill(t *testing.T) {
	app := &App{
		Registry: emptyRegistryForTierTest(t),
		Tier:     TierPublic,
	}
	registerConfigSubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)

	_, err := registry.Invoke(ctx, app, "config", "register_skill", map[string]any{
		"id":         "test_skill",
		"name":       "Test",
		"capability": "code",
	})
	if err == nil {
		t.Fatal("SECURITY: public tier must NOT be able to register skills")
	}
}
