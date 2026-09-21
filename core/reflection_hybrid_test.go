package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// MockHybridProvider implements providers.Provider (interfaces.Provider) for testing
type MockHybridProvider struct {
	response string
}

func (m *MockHybridProvider) Name() string { return "mock-hybrid" }
func (m *MockHybridProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (m *MockHybridProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: m.response}, nil
}
func (m *MockHybridProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return nil, nil
}
func (m *MockHybridProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func TestReflectionEngine_ScanForHybridMerge(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := t.TempDir()

	// 1. Setup Registry and Provider
	providerCfg := &config.ProviderConfig{
		Provider: "mock-hybrid",
	}

	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"default": providerCfg,
		},
		DefaultProvider: "default",
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		DynamicMCPs:     make(map[string]*config.MCPDef),
	}

	mock := &MockHybridProvider{
		response: "```lua\n-- unified expert skill\nfunction expert() end\n```",
	}

	// Inject into GlobalRouter and Cache
	providers.GlobalRouter.Register("default", providerCfg, mock)
	t.Cleanup(func() { providers.GlobalRouter.Deregister("default") })

	logger, _ := NewLogger(logDir, nil)
	jit := NewJITManager(reg, tmpDir)

	// Experience store
	es, _ := store.NewExperienceStore(tmpDir, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jit, logger)

	// 2. Prepare two verified variants
	id1, _ := jit.RegisterTool("-- variant 1", "lua", 1*time.Hour, true)
	id2, _ := jit.RegisterTool("-- variant 2", "lua", 1*time.Hour, true)

	es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: id1, Capability: "merge-test", SuccessRate: 1.0, IsVerified: true, UsageCount: 5,
	})
	es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: id2, Capability: "merge-test", SuccessRate: 1.0, IsVerified: true, UsageCount: 5,
	})

	// 3. Trigger Merge
	re.ScanForHybridMerge(context.Background())

	// 4. Verify result
	snapshot := es.GetGeneratedSkillsSnapshot()
	found := false
	for _, gs := range snapshot {
		if strings.Contains(gs.Description, "Unified Expert Skill") && gs.Capability == "merge-test" && gs.IsVerified {
			found = true
			src, _ := jit.GetSource(gs.ID)
			if !strings.Contains(src, "function expert()") {
				t.Errorf("expected expert code, got %q", src)
			}
			break
		}
	}
	if !found {
		t.Error("Expert skill not found or not verified after hybrid merge")
	}
}

func TestReflectionEngine_CrystallizeStepToSkill_Additive(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := t.TempDir()

	providerCfg := &config.ProviderConfig{
		Provider: "mock-hybrid",
	}
	reg := &config.Registry{
		Providers: map[string]*config.ProviderConfig{
			"default": providerCfg,
		},
		DefaultProvider: "default",
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		DynamicMCPs:     make(map[string]*config.MCPDef),
	}

	mock := &MockHybridProvider{
		response: "```lua\n-- additive extension\n```",
	}
	providers.GlobalRouter.Register("default", providerCfg, mock)
	t.Cleanup(func() { providers.GlobalRouter.Deregister("default") })

	logger, _ := NewLogger(logDir, nil)
	jit := NewJITManager(reg, tmpDir)
	es, _ := store.NewExperienceStore(tmpDir, nil, nil, nil, nil)

	// Mock TaskStore
	ts := &mockSignalTaskStore{}
	re := NewReflectionEngine(es, ts, reg, jit, logger)

	// 1. Create a baseline skill
	baselineID, _ := jit.RegisterTool("-- baseline", "lua", 1*time.Hour, true)
	es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: baselineID, Capability: "addon-test", SuccessRate: 1.0, IsVerified: true,
	})

	// 2. Simulate a new successful task step for the same capability
	rec := store.StepRecord{
		StepID:     "s1",
		RoleID:     "r1",
		Capability: "addon-test",
		Status:     "OK",
		Confidence: 0.95,
		Task:       "new task variant",
		Trace: []store.ToolInteraction{
			{ToolName: "shell", Arguments: map[string]any{"c": "cmd"}},
		},
	}

	// 3. Crystallize
	re.CrystallizeStepToSkill(context.Background(), rec)

	// 4. Verification
	found := false
	for _, gs := range es.GetGeneratedSkillsSnapshot() {
		if gs.Description == "new task variant" && gs.Capability == "addon-test" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Skill from additive crystallization not found")
	}
}
