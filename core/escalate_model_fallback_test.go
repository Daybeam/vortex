package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestResolveCandidateConfig_EscalateModelByName is the regression test for
// the P0-2 bug: escalate_model:gpt-4o silently fell back to the role's
// default provider (gemini) instead of switching to gpt-4o.
//
// Root cause: escalate_model:MODEL_NAME sets step.ProviderOverride = "gpt-4o"
// (a model name), but resolveCandidateConfig treats ProviderOverride as a
// provider ID (a key in Registry.Providers). Since no provider has ID
// "gpt-4o" (the ID is "openai"), GetProviderConfig returns nil and the code
// falls back to ResolveProviderConfig(role) → role's default (gemini).
//
// Fix: when GetProviderConfig(pID) returns nil and pID is non-empty, search
// all providers for one whose Model field matches pID via FindProviderByModel.
//
// Before fix: this test fails — resolveCandidateConfig returns gemini.
// After fix: this test passes — resolveCandidateConfig returns openai/gpt-4o.
func TestResolveCandidateConfig_EscalateModelByName(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o", APIKeyEnv: "OPENAI_API_KEY"},
			"gemini": {Provider: "gemini", Model: "gemini-flash-lite-latest", APIKeyEnv: "GEMINI_API_KEY"},
		},
		DefaultProvider: "gemini",
		Roles: map[string]*config.Role{
			"analyst": {ID: "analyst", Provider: "gemini"},
		},
	}

	graph := &schemas.TaskGraph{TaskID: "task_escalate_test"}
	hub := NewContextHub(reg, graph, nil)

	s := &Spawner{}
	role := reg.Roles["analyst"]

	// Simulate escalate_model:gpt-4o: providerID = "gpt-4o" (model name, not
	// provider ID). Before the fix, this returns gemini (role default).
	pc := s.resolveCandidateConfig(hub, role, "gpt-4o", "")

	if pc == nil {
		t.Fatal("expected non-nil ProviderConfig for escalate_model:gpt-4o")
	}
	if pc.Model != "gpt-4o" {
		t.Errorf("escalate_model:gpt-4o should resolve to model gpt-4o, got %q (silent fallback to role default)", pc.Model)
	}
	if pc.Provider == "gemini" {
		t.Error("escalate_model:gpt-4o silently fell back to gemini — the P0-2 bug is still present")
	}
}

// TestResolveCandidateConfig_ProviderIDStillWorks verifies that passing a
// real provider ID (not a model name) still works after the fix. This
// ensures the model-name fallback is purely additive and doesn't break
// existing behavior.
func TestResolveCandidateConfig_ProviderIDStillWorks(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o", APIKeyEnv: "OPENAI_API_KEY"},
			"gemini": {Provider: "gemini", Model: "gemini-flash-lite-latest", APIKeyEnv: "GEMINI_API_KEY"},
		},
		DefaultProvider: "gemini",
	}
	graph := &schemas.TaskGraph{TaskID: "task_provider_id_test"}
	hub := NewContextHub(reg, graph, nil)

	s := &Spawner{}

	// Pass a real provider ID "openai" — should resolve directly.
	pc := s.resolveCandidateConfig(hub, nil, "openai", "")
	if pc == nil {
		t.Fatal("expected non-nil ProviderConfig for provider ID openai")
	}
	if pc.Provider != "openai" {
		t.Errorf("expected provider openai, got %q", pc.Provider)
	}
}

// TestResolveCandidateConfig_UnknownModelFallsBackToRole verifies that when
// neither a provider ID nor a model name matches, the existing fallback to
// the role's default provider still works. This ensures the fix doesn't
// break the safety-net behavior.
func TestResolveCandidateConfig_UnknownModelFallsBackToRole(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o"},
			"gemini": {Provider: "gemini", Model: "gemini-flash-lite-latest"},
		},
		DefaultProvider: "gemini",
		Roles: map[string]*config.Role{
			"analyst": {ID: "analyst", Provider: "gemini"},
		},
	}
	graph := &schemas.TaskGraph{TaskID: "task_unknown_test"}
	hub := NewContextHub(reg, graph, nil)

	s := &Spawner{}
	role := reg.Roles["analyst"]

	// "claude-3-opus" is neither a provider ID nor a configured model.
	// Should fall back to role default (gemini).
	pc := s.resolveCandidateConfig(hub, role, "claude-3-opus", "")
	if pc == nil {
		t.Fatal("expected non-nil ProviderConfig from role fallback")
	}
	if pc.Provider != "gemini" {
		t.Errorf("expected fallback to gemini (role default), got %q", pc.Provider)
	}
}

// TestEscalateModel_UnavailableModel_ReturnsError is the regression test for
// the P0-2 bug (user feedback): when escalate_model:MODEL is submitted and
// MODEL is genuinely not available (not a provider ID, not a configured
// model name), SubmitDecision must return an error instead of silently
// falling back to the role's default provider.
//
// Before fix: SubmitDecision succeeds, step.ProviderOverride is set, and the
// spawn silently falls back to gemini — the user sees a repetition loop.
// After fix: SubmitDecision returns an error immediately.
func TestEscalateModel_UnavailableModel_ReturnsError(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o"},
			"gemini": {Provider: "gemini", Model: "gemini-flash-lite-latest"},
		},
		DefaultProvider: "gemini",
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		MCPs:            make(map[string]*config.MCPDef),
		System:          config.SystemSettings{},
	}
	ts := &mockTaskStore{}
	tasksDir := t.TempDir()
	logger := mustNewLogger(t, tasksDir, nil)
	defer logger.Close()

	engine := NewDirectedEngine(reg, ts, nil, nil, logger, nil, tasksDir, tasksDir, nil)
	engine.lifecycleCancel()

	taskID := "task_escalate_unavailable"
	stepID := "step_1"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			stepID: {ID: stepID, Status: schemas.StepPending, Task: "test"},
		},
		PendingDecisions: []*schemas.Decision{
			{ID: "dec_1", StepID: stepID, Options: []string{"escalate_model"}},
		},
	}
	engine.Mu.Lock()
	engine.graphs[taskID] = graph
	engine.Mu.Unlock()

	// "claude-3-opus" is not a provider ID and no provider uses this model.
	err := engine.SubmitDecision(taskID, "dec_1", "escalate_model:claude-3-opus")
	if err == nil {
		t.Fatal("expected error for unavailable model, got nil — escalate_model silently accepted an unknown model")
	}
}

// TestEscalateModel_AvailableModel_Succeeds verifies that escalate_model
// with a model that IS available (as a model name) succeeds without error.
func TestEscalateModel_AvailableModel_Succeeds(t *testing.T) {
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"openai": {Provider: "openai", Model: "gpt-4o"},
			"gemini": {Provider: "gemini", Model: "gemini-flash-lite-latest"},
		},
		DefaultProvider: "gemini",
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		MCPs:            make(map[string]*config.MCPDef),
		System:          config.SystemSettings{},
	}
	ts := &mockTaskStore{}
	tasksDir := t.TempDir()
	logger := mustNewLogger(t, tasksDir, nil)
	defer logger.Close()

	engine := NewDirectedEngine(reg, ts, nil, nil, logger, nil, tasksDir, tasksDir, nil)
	engine.lifecycleCancel()

	taskID := "task_escalate_available"
	stepID := "step_1"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			stepID: {ID: stepID, Status: schemas.StepPending, Task: "test"},
		},
		PendingDecisions: []*schemas.Decision{
			{ID: "dec_1", StepID: stepID, Options: []string{"escalate_model"}},
		},
	}
	engine.Mu.Lock()
	engine.graphs[taskID] = graph
	engine.Mu.Unlock()

	// "gpt-4o" is available as a model name (provider "openai" uses it).
	err := engine.SubmitDecision(taskID, "dec_1", "escalate_model:gpt-4o")
	if err != nil {
		t.Fatalf("expected success for available model gpt-4o, got error: %v", err)
	}

	step := graph.Steps[stepID]
	if step.ProviderOverride != "gpt-4o" {
		t.Errorf("expected ProviderOverride=gpt-4o, got %q", step.ProviderOverride)
	}
}
