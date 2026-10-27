package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// mockRoleGenProvider returns a canned valid role JSON so GenerateRoleObjects
// succeeds without a real LLM. Registered via providers.GlobalRouter.
type mockRoleGenProvider struct{}

func (m *mockRoleGenProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: `{"name":"Regression Test Role","capability":"testing","system_prompt":"You are a test role."}`}, nil
}
func (m *mockRoleGenProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	onChunk(`{"name":"Regression Test Role","capability":"testing","system_prompt":"You are a test role."}`)
	return &schemas.ProviderResponse{Text: `{"name":"Regression Test Role","capability":"testing","system_prompt":"You are a test role."}`}, nil
}
func (m *mockRoleGenProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (m *mockRoleGenProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (m *mockRoleGenProvider) Name() string { return "mock-role-gen" }

// TestLogInterceptor_EmptyRoleID_EphemeralSuccess_PropagatesRoleID is the
// regression test for the 2026-10-27 fix in builtin_interceptors.go.
//
// Bug: when a task arrived with RoleID="" (the "let the system pick a role"
// flow) and ephemeral generation SUCCEEDED, the generated role was stored
// under the empty key "" and req.RoleID was never updated. GetRole("")
// returns nil unconditionally (hub.go:103), so the spawner died with
// `role "" not found` right after generation "succeeded" — observed in
// task_68d6fe80 / task_7e8d85f3 (2026-10-27).
//
// Fix: after successful generation, if req.RoleID == "" && role.ID != "",
// set req.RoleID = role.ID and store under that ID.
//
// This test verifies the next handler receives a NON-empty RoleID that
// resolves via GetRole.
func TestLogInterceptor_EmptyRoleID_EphemeralSuccess_PropagatesRoleID(t *testing.T) {
	// Set up a mock provider via GlobalRouter so GenerateRoleObjects succeeds.
	providers.ClearCache()
	t.Cleanup(providers.ClearCache)

	cfg := &config.ProviderConfig{
		Provider: "mock-role-gen",
		Model:    "test-model",
	}
	providers.GlobalRouter.Register(providers.PoolIDFor(cfg), cfg, &mockRoleGenProvider{})

	// Create a test cookbook file so FetchCookbook succeeds.
 cookbookDir := t.TempDir()
	cookbookPath := filepath.Join(cookbookDir, "cookbook.md")
	if err := os.WriteFile(cookbookPath, []byte("# Test Cookbook\nBest practice: write tests."), 0644); err != nil {
		t.Fatalf("write cookbook: %v", err)
	}

	reg := &config.Registry{
		Roles:                  map[string]*config.Role{},
		Providers:              map[string]*config.ProviderConfig{"mock-role-gen": cfg},
		DefaultProvider:        "mock-role-gen",
		EnableEphemeralRoleGen: true,
		RoleCookbookSource:     cookbookPath,
	}
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	loader := NewResourceLoader()
	gen := NewRoleGenerator(reg, loader, logger)

	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	interceptor := LogInterceptor(reg, logger, gen)

	// The next handler captures the RoleID it receives.
	var capturedRoleID string
	next := func(_ context.Context, req *SpawnRequest) (*SpawnResult, error) {
		capturedRoleID = req.RoleID
		return &SpawnResult{Output: schemas.SubagentOutput{Status: schemas.StatusOK}}, nil
	}

	// SpawnRequest with EMPTY RoleID — the "pick a role from the task" flow.
	req := &SpawnRequest{
		TaskID: "regression-test",
		StepID: "s1",
		RoleID: "", // ← the bug: this stayed "" after generation
		Task:   "Generate a regression test for the role ID propagation bug",
		Hub:    hub,
	}

	_, err := interceptor(context.Background(), req, next)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}

	// ASSERTION 1: req.RoleID must be non-empty after the fix.
	if capturedRoleID == "" {
		t.Fatal("REGRESSION: next() received RoleID=\"\" — the generated role's ID was not propagated. " +
			"GetRole(\"\") returns nil, so the spawner would fail with `role \"\" not found`.")
	}

	// ASSERTION 2: the role must be resolvable via GetRole under the propagated ID.
	role := hub.GetRole(capturedRoleID)
	if role == nil {
		t.Fatalf("REGRESSION: GetRole(%q) returned nil — the generated role was not stored under its own ID. "+
			"It was likely stored under the empty key \"\".", capturedRoleID)
	}

	// ASSERTION 3: GetRole("") must still return nil (the sentinel is intact).
	if hub.GetRole("") != nil {
		t.Fatal("GetRole(\"\") returned non-nil — the empty-string sentinel is broken")
	}

	t.Logf("FIX VERIFIED: RoleID propagated as %q, role resolves via GetRole", capturedRoleID)
}
