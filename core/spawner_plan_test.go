package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// --- DetermineComplexity tests ---

func TestDetermineComplexity_MultiFileKeywords(t *testing.T) {
	tests := []struct {
		task string
		want bool
	}{
		{"Refactor auth module across files", true},
		{"Update multiple files for new API", true},
		{"Sync multi-file configuration", true},
		{"Fix cross-file imports", true},
	}
	for _, tt := range tests {
		if got := schemas.DetermineComplexity(tt.task, 1); got != tt.want {
			t.Errorf("DetermineComplexity(%q) = %v, want %v", tt.task, got, tt.want)
		}
	}
}

func TestDetermineComplexity_MultiPhaseKeywords(t *testing.T) {
	tests := []struct {
		task string
		want bool
	}{
		{"Read config then parse it then validate", true},
		{"Read data then process then output", true},
	}
	for _, tt := range tests {
		if got := schemas.DetermineComplexity(tt.task, 1); got != tt.want {
			t.Errorf("DetermineComplexity(%q) = %v, want %v", tt.task, got, tt.want)
		}
	}
}

func TestDetermineComplexity_RefactorKeywords(t *testing.T) {
	tests := []struct {
		task string
		want bool
	}{
		{"Refactor the auth module", true},
		{"Migrate from v1 to v2", true},
		{"Rewrite the parser", true},
		{"Restructure the directory layout", true},
	}
	for _, tt := range tests {
		if got := schemas.DetermineComplexity(tt.task, 1); got != tt.want {
			t.Errorf("DetermineComplexity(%q) = %v, want %v", tt.task, got, tt.want)
		}
	}
}

func TestDetermineComplexity_LongTask(t *testing.T) {
	longTask := "This is a very long task description that exceeds the 500 character threshold and should therefore be considered complex enough to warrant a step plan because it clearly involves multiple steps and considerations that a simple one-line task would not have, and we need to make sure the planner phase kicks in for such detailed task descriptions. The task involves reading multiple configuration files, parsing their contents, validating against a schema, transforming the data, and writing output in a new format. Additionally, error handling must be added for each step, and the entire process should be logged for debugging purposes when verbose mode is enabled."
	if len(longTask) <= 500 {
		t.Fatalf("test task too short: %d", len(longTask))
	}
	if got := schemas.DetermineComplexity(longTask, 1); !got {
		t.Error("DetermineComplexity should return true for tasks > 500 chars")
	}
}

func TestDetermineComplexity_SimpleTask(t *testing.T) {
	tests := []struct {
		task string
		want bool
	}{
		{"Write a hello world program", false},
		{"Fix typo in README", false},
		{"Add a comment", false},
		{"List files in directory", false},
	}
	for _, tt := range tests {
		if got := schemas.DetermineComplexity(tt.task, 1); got != tt.want {
			t.Errorf("DetermineComplexity(%q) = %v, want %v", tt.task, got, tt.want)
		}
	}
}

// --- ProbeTaskComplexity tests ---

// mockProbeProvider implements providers.Provider for probe testing.
type mockProbeProvider struct {
	toolCalls []schemas.ToolCall
	text      string
}

func (m *mockProbeProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: m.text, ToolCalls: m.toolCalls}, nil
}
func (m *mockProbeProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return m.Complete(ctx, req)
}
func (m *mockProbeProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (m *mockProbeProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (m *mockProbeProvider) Name() string                                               { return "mockprobe" }

func TestProbeTaskComplexity_Yes(t *testing.T) {
	probe := &mockProbeProvider{
		toolCalls: []schemas.ToolCall{
			{Name: "complexity", Arguments: map[string]any{"choice": "yes"}},
		},
	}
	complex, err := ProbeTaskComplexity(probe, "refactor the auth module")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !complex {
		t.Error("expected complex=true for choice=yes")
	}
}

func TestProbeTaskComplexity_No(t *testing.T) {
	probe := &mockProbeProvider{
		toolCalls: []schemas.ToolCall{
			{Name: "complexity", Arguments: map[string]any{"choice": "no"}},
		},
	}
	complex, err := ProbeTaskComplexity(probe, "fix a typo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complex {
		t.Error("expected complex=false for choice=no")
	}
}

func TestProbeTaskComplexity_Abstention(t *testing.T) {
	// Empty ToolCalls -> abstention (score_threshold filtered the answer)
	probe := &mockProbeProvider{}
	_, err := ProbeTaskComplexity(probe, "ambiguous task")
	if err == nil {
		t.Error("expected error for abstained probe")
	}
}

func TestProbeTaskComplexity_NilProvider(t *testing.T) {
	_, err := ProbeTaskComplexity(nil, "any task")
	if err == nil {
		t.Error("expected error for nil provider")
	}
}

// --- shouldEnableStepPlan tests ---

func TestShouldEnableStepPlan_AlreadyEnabled(t *testing.T) {
	s := &Spawner{}
	req := &SpawnRequest{
		Task:           "simple task",
		EnableStepPlan: true,
	}
	if !s.shouldEnableStepPlan(req, nil) {
		t.Error("should return true when EnableStepPlan is already true")
	}
}

func TestShouldEnableStepPlan_KeywordComplex(t *testing.T) {
	s := &Spawner{}
	req := &SpawnRequest{
		Task: "Refactor the auth module across files",
	}
	if !s.shouldEnableStepPlan(req, nil) {
		t.Error("should return true for keyword-complex task")
	}
}

func TestShouldEnableStepPlan_ShortSimpleTask(t *testing.T) {
	s := &Spawner{}
	req := &SpawnRequest{
		Task: "Fix typo",
	}
	if s.shouldEnableStepPlan(req, nil) {
		t.Error("should return false for short simple task without keywords")
	}
}

func TestShouldEnableStepPlan_NilProbeFuzzyZone(t *testing.T) {
	s := &Spawner{}
	// Long task without keywords -> fuzzy zone, but nil probe -> false
	req := &SpawnRequest{
		Task: "This is a moderately long task description that doesn't hit any keywords but is long enough to be in the fuzzy zone for complexity assessment.",
	}
	if s.shouldEnableStepPlan(req, nil) {
		t.Error("should return false for fuzzy-zone task with nil probe")
	}
}

func TestShouldEnableStepPlan_ProbeYes(t *testing.T) {
	s := &Spawner{}
	probe := &mockProbeProvider{
		toolCalls: []schemas.ToolCall{
			{Name: "complexity", Arguments: map[string]any{"choice": "yes"}},
		},
	}
	req := &SpawnRequest{
		Task: "This is a moderately long task description that doesn't hit any keywords but is long enough to be in the fuzzy zone for complexity assessment.",
	}
	if !s.shouldEnableStepPlan(req, probe) {
		t.Error("should return true when probe says yes")
	}
}

func TestShouldEnableStepPlan_ProbeNo(t *testing.T) {
	s := &Spawner{}
	probe := &mockProbeProvider{
		toolCalls: []schemas.ToolCall{
			{Name: "complexity", Arguments: map[string]any{"choice": "no"}},
		},
	}
	req := &SpawnRequest{
		Task: "This is a moderately long task description that doesn't hit any keywords but is long enough to be in the fuzzy zone for complexity assessment.",
	}
	if s.shouldEnableStepPlan(req, probe) {
		t.Error("should return false when probe says no")
	}
}
