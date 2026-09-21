package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestUpdateSystem_PartialUpdatePreservesUntouchedFields is a direct
// regression test for a severe bug found live (2026-08-21): update_system's
// handler used to json.Unmarshal args into a fresh, zero-valued local
// struct and then unconditionally overwrite EVERY field on
// app.Registry.System with it. A caller who only wanted to change one
// setting (e.g. {"staging_enabled": true}) would silently zero out
// confidence_threshold, max_decision_outcomes, max_context_keep,
// default_jit_ttl, swarm_fallback_delay, and flip swarm_mode_enabled to
// false -- discovered against real production config, not a hypothetical.
func TestUpdateSystem_PartialUpdatePreservesUntouchedFields(t *testing.T) {
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg.System.ConfidenceThreshold = 0.7
	reg.System.MaxDecisionOutcomes = 2000
	reg.System.MaxContextKeep = 100
	reg.System.DefaultJITTTL = 3600
	reg.System.SwarmFallbackDelay = 10
	reg.SwarmModeEnabled = true

	app := &App{Registry: reg}
	RegisterSubsystems(app)

	action, ok := findUpdateSystemAction()
	if !ok {
		t.Fatalf("update_system action not registered")
	}

	// Caller only wants to flip staging_enabled -- must NOT touch anything else.
	_, err = action.Handler(context.Background(), app, map[string]any{"staging_enabled": true})
	if err != nil {
		t.Fatalf("update_system handler: %v", err)
	}

	if !reg.System.StagingEnabled {
		t.Errorf("staging_enabled should now be true")
	}
	if reg.System.ConfidenceThreshold != 0.7 {
		t.Errorf("confidence_threshold should be untouched (0.7), got %v", reg.System.ConfidenceThreshold)
	}
	if reg.System.MaxDecisionOutcomes != 2000 {
		t.Errorf("max_decision_outcomes should be untouched (2000), got %v", reg.System.MaxDecisionOutcomes)
	}
	if reg.System.MaxContextKeep != 100 {
		t.Errorf("max_context_keep should be untouched (100), got %v", reg.System.MaxContextKeep)
	}
	if reg.System.DefaultJITTTL != 3600 {
		t.Errorf("default_jit_ttl should be untouched (3600), got %v", reg.System.DefaultJITTTL)
	}
	if reg.System.SwarmFallbackDelay != 10 {
		t.Errorf("swarm_fallback_delay should be untouched (10), got %v", reg.System.SwarmFallbackDelay)
	}
	if !reg.SwarmModeEnabled {
		t.Errorf("swarm_mode_enabled should be untouched (true)")
	}
}

// TestUpdateSystem_ExplicitFieldsAreApplied confirms fields that ARE
// present in args are actually applied (the fix isn't a no-op).
func TestUpdateSystem_ExplicitFieldsAreApplied(t *testing.T) {
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	app := &App{Registry: reg}
	RegisterSubsystems(app)

	action, ok := findUpdateSystemAction()
	if !ok {
		t.Fatalf("update_system action not registered")
	}

	_, err = action.Handler(context.Background(), app, map[string]any{
		"confidence_threshold":  0.85,
		"max_decision_outcomes": float64(500),
		"swarm_mode_enabled":    true,
		"bookmarks":             []any{"example.com", "trusted.org"},
	})
	if err != nil {
		t.Fatalf("update_system handler: %v", err)
	}

	if reg.System.ConfidenceThreshold != 0.85 {
		t.Errorf("confidence_threshold not applied, got %v", reg.System.ConfidenceThreshold)
	}
	if reg.System.MaxDecisionOutcomes != 500 {
		t.Errorf("max_decision_outcomes not applied, got %v", reg.System.MaxDecisionOutcomes)
	}
	if !reg.SwarmModeEnabled {
		t.Errorf("swarm_mode_enabled not applied")
	}
	if len(reg.System.Bookmarks) != 2 || reg.System.Bookmarks[0] != "example.com" {
		t.Errorf("bookmarks not applied correctly, got %+v", reg.System.Bookmarks)
	}
}

// findUpdateSystemAction locates the registered update_system Action via
// the package-level global `registry` (tools/subsystems.go), which is what
// RegisterSubsystems actually populates -- not anything hung off App or
// config.Registry.
func findUpdateSystemAction() (Action, bool) {
	sub, ok := registry.Subsystems["config"]
	if !ok {
		return Action{}, false
	}
	act, ok := sub.Actions["update_system"]
	return act, ok
}
