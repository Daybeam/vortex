package tools

import (
	"context"
	"path/filepath"
	"strings"
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

// TestUpdateSystem_RejectsUnknownKeys is a regression test for the surviving
// half of the 2026-08-21 update_system bug (see
// TestUpdateSystem_PartialUpdatePreservesUntouchedFields above). The
// 2026-08-21 fix stopped a one-field call from zeroing every other setting,
// but a *misspelled or unsupported* field name still reported success: it
// matched no whitelist entry, so `ops` stayed empty and the handler returned
// {"ok":true,"message":"no changes"}. An operator who typed
// "confidences_threshold" therefore believed the setting had been changed
// while nothing was written to config.json -- a silently-ignored write on a
// config-mutating admin action. Same failure class as the HTTP
// /api/config/update-system silent-clear bug.
func TestUpdateSystem_RejectsUnknownKeys(t *testing.T) {
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg.System.StagingEnabled = false

	app := &App{Registry: reg}
	RegisterSubsystems(app)

	action, ok := findUpdateSystemAction()
	if !ok {
		t.Fatalf("update_system action not registered")
	}

	// A body whose only key is unknown must be an error, not "no changes".
	res, err := action.Handler(context.Background(), app, map[string]any{"confidences_threshold": 0.9})
	if err == nil {
		t.Fatalf("unknown key accepted with result %+v -- a typo'd field must be a hard failure, not a silent no-op", res)
	}
	if !strings.Contains(err.Error(), "confidences_threshold") {
		t.Errorf("error should name the offending key, got: %v", err)
	}

	// An unknown key alongside a valid one must abort the whole call; the
	// valid half must not be committed.
	if _, err := action.Handler(context.Background(), app, map[string]any{"staging_enabled": true, "max_contextkeep": 50}); err == nil {
		t.Errorf("mixed known/unknown body succeeded -- known fields must not be partially applied")
	}
	if reg.System.StagingEnabled {
		t.Errorf("staging_enabled was committed despite the unknown key in the same body")
	}

	// The dispatcher strips every _-prefixed key before handlers run
	// (tools/subsystems.go), so these are only reachable by direct Go
	// callers -- they must not be flagged as unknown.
	if _, err := action.Handler(context.Background(), app, map[string]any{"_actor": "ops-bot", "_task_id": "t1", "staging_enabled": true}); err != nil {
		t.Errorf("reserved _actor/_task_id keys were rejected as unknown: %v", err)
	}

	// An entirely empty body stays the documented no-op success.
	if _, err := action.Handler(context.Background(), app, map[string]any{}); err != nil {
		t.Errorf("empty body should remain a no-op success, got: %v", err)
	}

	// The same rejection has to survive the real orchestrator_invoke path:
	// Invoke() wraps handlers in a timeout goroutine, so a handler error must
	// still surface as an error rather than a success-shaped reply.
	// audit O29 (2026-11-04): as written this passed vacuously. Invoke() gates on
	// the tier before the handler runs (subsystems.go:135) and a bare context has
	// no tier, so the call returned "permission denied" and never reached the
	// unknown-key check. Passing an admin tier and matching the message makes the
	// assertion test what its comment claims.
	invokeCtx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)
	res, err = registry.Invoke(invokeCtx, app, "config", "update_system", map[string]any{"swarm_fallback_delayy": 3})
	if err == nil {
		t.Errorf("Invoke() swallowed the unknown-key rejection, got %+v", res)
	} else if !strings.Contains(err.Error(), "swarm_fallback_delayy") {
		t.Errorf("Invoke() failed for the wrong reason: %v", err)
	}

	// The positive half, through the same path: a known field must still commit,
	// and the _actor that Invoke() now injects for the audit trail (O4) must not
	// be treated as an unknown field.
	if _, err := registry.Invoke(invokeCtx, app, "config", "update_system", map[string]any{"staging_enabled": true}); err != nil {
		t.Errorf("Invoke() rejected a known field: %v", err)
	}
	if !reg.System.StagingEnabled {
		t.Errorf("Invoke() reported success but staging_enabled was not applied")
	}
}

// TestUpdateSystem_WhitelistedFieldsRemainAccepted guards the other
// direction: the unknown-key check must not lock out any field the handler
// actually supports.
func TestUpdateSystem_WhitelistedFieldsRemainAccepted(t *testing.T) {
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

	accepted := map[string]any{
		"confidence_threshold":  0.5,
		"max_decision_outcomes": float64(10),
		"max_context_keep":      float64(10),
		"default_jit_ttl":       float64(60),
		"swarm_fallback_delay":  float64(5),
		"staging_enabled":       true,
		"bookmarks":             []any{"example.com"},
		"external_interceptors": []any{},
		"swarm_mode_enabled":    true,
	}
	for field, val := range accepted {
		if _, err := action.Handler(context.Background(), app, map[string]any{field: val}); err != nil {
			t.Errorf("supported field %q rejected: %v", field, err)
		}
	}
}

// TestUpdateSystem_DoesNotAcceptAPIKeys pins the deliberate asymmetry from the
// O24-3 follow-up: named keys are writable only through the operator-facing
// HTTP route (POST /api/config/update-system on adminMux), never through the
// model-facing action, so a confused or compromised model cannot mint itself
// an admin credential.
func TestUpdateSystem_DoesNotAcceptAPIKeys(t *testing.T) {
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

	res, err := action.Handler(context.Background(), app, map[string]any{
		"api_keys": []any{map[string]any{"key": "model-minted", "owner_id": "model", "tier": "admin"}},
	})
	if err == nil {
		t.Fatalf("update_system accepted api_keys with %+v -- a model must not mint named keys", res)
	}
	if !strings.Contains(err.Error(), "api_keys") {
		t.Errorf("error should name api_keys, got: %v", err)
	}
	if len(reg.System.APIKeys) != 0 {
		t.Errorf("rejected call still wrote keys: %+v", reg.System.APIKeys)
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
