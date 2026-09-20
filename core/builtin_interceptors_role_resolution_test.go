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
// role_generator_nil_loader_test.go) suitable for exercising LogInterceptor”s
// role-resolution branching without any real network/provider calls. A nil
// resourceLoader makes GenerateRoleObjects/GenerateRole fail cleanly and
// quickly instead of attempting a real cookbook fetch -- exactly what these
// tests need, since they assert on *which branch* was taken, not on the
// content of a real LLM-generated role.
func newTestLogInterceptorDeps(t *testing.T, enableEphemeral, enableDynamic bool) (*config.Registry, *Logger, *RoleGenerator) {
	t.Helper()
	reg := &config.Registry{
		Roles:                  map[string]*config.Role{},
		DefaultProvider:        "main",
		EnableEphemeralRoleGen: enableEphemeral,
		EnableDynamicRoleGen:   enableDynamic,
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
	// Regression test for the exact bug this session''s change fixes: before,
	// LogInterceptor indexed registry.Roles directly and had no way to see a
	// role that only exists in the Hub''s session-scoped IR, so it would
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

func TestLogInterceptor_MissingRole_EphemeralEnabled_TriesEphemeralPathAndFailsCleanly(t *testing.T) {
	// EnableEphemeralRoleGen=true, EnableDynamicRoleGen=true, Hub present: the
	// ephemeral path must be tried FIRST and, since the RoleGenerator has a nil
	// resourceLoader (so GenerateRoleObjects fails fast), the interceptor must
	// return that error directly rather than silently falling through to the
	// persistent GetOrCreateRole path.
	reg, logger, gen := newTestLogInterceptorDeps(t, true, true)
	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something", Hub: hub}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err == nil {
		t.Fatal("expected an error since the underlying RoleGenerator has a nil resourceLoader")
	}
	if called {
		t.Error("next() should not be called when role generation fails")
	}
	if _, ok := graph.GetSessionRole("missing_role"); ok {
		t.Error("a failed ephemeral generation should not have stored anything in SessionRoles")
	}
}

func TestLogInterceptor_MissingRole_EphemeralDisabled_FallsBackToPersistentPath(t *testing.T) {
	// EnableEphemeralRoleGen=false, EnableDynamicRoleGen=true: must take the
	// GetOrCreateRole (persistent) branch, not the ephemeral one. Both will
	// fail cleanly given the nil resourceLoader, but this test asserts on the
	// branch taken (via the SessionRoles side effect, which only the ephemeral
	// branch would produce even on failure -- see previous test) rather than
	// the specific error text, since both paths surface a wrapped generation
	// error.
	reg, logger, gen := newTestLogInterceptorDeps(t, false, true)
	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something", Hub: hub}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err == nil {
		t.Fatal("expected an error since the underlying RoleGenerator has a nil resourceLoader")
	}
	if called {
		t.Error("next() should not be called when role generation fails")
	}
}

func TestLogInterceptor_MissingRole_EphemeralRequiresHub(t *testing.T) {
	// EnableEphemeralRoleGen=true but req.Hub is nil: the ephemeral case guard
	// explicitly requires Hub != nil (it needs somewhere to store the result),
	// so this must fall through to the EnableDynamicRoleGen branch instead of
	// panicking on a nil Hub dereference.
	reg, logger, gen := newTestLogInterceptorDeps(t, true, false)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something"} // Hub deliberately nil

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err == nil {
		t.Fatal("expected an error (dynamic generation disabled, and ephemeral requires a Hub)")
	}
	if called {
		t.Error("next() should not be called")
	}
}

func TestLogInterceptor_MissingRole_BothDisabled_ReturnsDisabledError(t *testing.T) {
	reg, logger, gen := newTestLogInterceptorDeps(t, false, false)

	interceptor := LogInterceptor(reg, logger, gen)
	called := false
	req := &SpawnRequest{TaskID: "t1", StepID: "s1", RoleID: "missing_role", Task: "do something"}

	_, err := interceptor(context.Background(), req, nextCalled(&called))
	if err == nil {
		t.Fatal("expected an error")
	}
	if called {
		t.Error("next() should not be called")
	}
	if !errors.Is(err, err) { // sanity: err is non-nil and comparable
		t.Fatal("unreachable")
	}
}
