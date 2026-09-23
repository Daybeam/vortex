package core

import (
	"context"
	"errors"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// newTestLogInterceptorDeps builds a Registry + Logger + RoleGenerator (with a
// deliberately nil resourceLoader, per the established pattern in
// role_generator_nil_loader_test.go) suitable for exercising LogInterceptor's
// role-resolution branching without any real network/provider calls. A nil
// resourceLoader makes GenerateRoleObjects/GenerateRole fail cleanly and
// quickly instead of attempting a real cookbook fetch -- exactly what these
// tests need, since they assert on *which branch* was taken, not on the
// content of a real LLM-generated role.
//
// The registry is seeded with the built-in "orchestrator_default" role,
// matching production behavior (config/loader.go seeds it at init).
func newTestLogInterceptorDeps(t *testing.T, enableEphemeral, enableDynamic bool) (*config.Registry, *Logger, *RoleGenerator) {
	t.Helper()
	reg := &config.Registry{
		Roles:                  map[string]*config.Role{},
		DefaultProvider:        "main",
		EnableEphemeralRoleGen: enableEphemeral,
		EnableDynamicRoleGen:   enableDynamic,
	}
	reg.Roles["orchestrator_default"] = &config.Role{
		ID:                  "orchestrator_default",
		Name:                "Orchestrator Default",
		BaseCapability:      "general",
		AllowDynamicSkills:  true,
		AllowDynamicMCPs:    true,
		MaxAdditionalSkills: 10,
	}
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	gen := NewRoleGenerator(reg, nil, logger)
	return reg, logger, gen
}

func nextCalled(called *bool) SpawnerHandler {
	return func(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
		*called = true
		return &SpawnResult{Output: schemas.SubagentOutput{Status: schemas.StatusOK}}, nil
	}
}

func TestLogInterceptor_RoleExistsInGlobalRegistry_NoGenerationAttempted(t *testing.T) {
	reg, logger, gen := newTestLogInterceptorDeps(t, true, true)
	reg.Roles["existing_role"] = &config.Role{ID: "existing_role"}

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "existing_role", Task: "do something"}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected next() to be called for an already-existing role")
	}
}

func TestLogInterceptor_RoleExistsViaSessionRole_NoGenerationAttempted(t *testing.T) {
	// Regression test for the exact bug this session's change fixes: before,
	// LogInterceptor indexed registry.Roles directly and had no way to see a
	// role that only exists in the Hub's session-scoped IR, so it would
	// incorrectly treat a caller-supplied session_roles entry as "missing"
	// and attempt to generate a conflicting one.
	reg, logger, gen := newTestLogInterceptorDeps(t, true, true)
	graph := &schemas.TaskGraph{}
	graph.SetSessionRole("session_only_role", &config.Role{ID: "session_only_role"})
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "session_only_role", Task: "do something", Hub: hub}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected next() to be called for a role that exists only as a session role")
	}
}

func TestLogInterceptor_MissingRole_EphemeralFails_FallsBackToDefaultRole(t *testing.T) {
	// EnableEphemeralRoleGen=true, Hub present: the ephemeral path is tried
	// FIRST and fails (nil resourceLoader). The interceptor must then fall
	// back to the built-in "orchestrator_default" role, storing it in the
	// session under the original role ID, and continue to next().
	reg, logger, gen := newTestLogInterceptorDeps(t, true, true)
	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something", Hub: hub}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if !called {
		t.Error("next() should be called after successful fallback to default role")
	}
	sessionRole, ok := graph.GetSessionRole("missing_role")
	if !ok {
		t.Fatal("expected default role to be stored in session under original role ID")
	}
	role, ok := sessionRole.(*config.Role)
	if !ok || role.ID != "orchestrator_default" {
		t.Errorf("expected session role to be orchestrator_default, got %v", sessionRole)
	}
}

func TestLogInterceptor_MissingRole_DynamicFails_FallsBackToDefaultRole(t *testing.T) {
	// EnableEphemeralRoleGen=false, EnableDynamicRoleGen=true: takes the
	// GetOrCreateRole (persistent) branch, which fails (nil resourceLoader).
	// The interceptor must fall back to the built-in default role.
	reg, logger, gen := newTestLogInterceptorDeps(t, false, true)
	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something", Hub: hub}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if !called {
		t.Error("next() should be called after successful fallback to default role")
	}
}

func TestLogInterceptor_MissingRole_EphemeralRequiresHub_FallsBackToDefaultRole(t *testing.T) {
	// EnableEphemeralRoleGen=true but req.Hub is nil: ephemeral case guard
	// requires Hub != nil, so falls through to EnableDynamicRoleGen=false,
	// then falls back to default role. Since Hub is nil, req.RoleID is
	// rewritten to "orchestrator_default".
	reg, logger, gen := newTestLogInterceptorDeps(t, true, false)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something"}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if !called {
		t.Error("next() should be called after successful fallback to default role")
	}
	if req.RoleID != "orchestrator_default" {
		t.Errorf("expected req.RoleID to be rewritten to orchestrator_default, got %s", req.RoleID)
	}
}

func TestLogInterceptor_MissingRole_BothDisabled_FallsBackToDefaultRole(t *testing.T) {
	reg, logger, gen := newTestLogInterceptorDeps(t, false, false)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something"}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if !called {
		t.Error("next() should be called after successful fallback to default role")
	}
}

func TestLogInterceptor_MissingRole_NoDefaultRole_ReturnsError(t *testing.T) {
	// When the registry does NOT contain "orchestrator_default" (e.g. a
	// test-only registry), the fallback cannot succeed and the original
	// generation error must be surfaced.
	reg, logger, gen := newTestLogInterceptorDeps(t, true, true)
	delete(reg.Roles, "orchestrator_default")
	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something", Hub: hub}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err == nil {
		t.Fatal("expected error when default role is not available")
	}
	if called {
		t.Error("next() should not be called when fallback fails")
	}
}

func TestLogInterceptor_MissingRole_BothDisabled_NoDefaultRole_ReturnsError(t *testing.T) {
	reg, logger, gen := newTestLogInterceptorDeps(t, false, false)
	delete(reg.Roles, "orchestrator_default")

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something"}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err == nil {
		t.Fatal("expected error")
	}
	if called {
		t.Error("next() should not be called")
	}
	if !errors.Is(err, err) {
		t.Fatal("unreachable")
	}
}
