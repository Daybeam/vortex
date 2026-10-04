package core

import (
	"context"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"strings"
	"testing"
	"time"
)

type mockTaskStore struct {
	store.ITaskStore
}

func (m *mockTaskStore) Get(ctx context.Context, taskID, stepID string) (*store.StepResult, error) {
	return &store.StepResult{Data: "mock result"}, nil
}
func (m *mockTaskStore) GetBatch(ctx context.Context, taskID string, stepIDs []string) (map[string]*store.StepResult, error) {
	result := make(map[string]*store.StepResult)
	for _, sid := range stepIDs {
		result[sid] = &store.StepResult{Data: "mock result"}
	}
	return result, nil
}
func (m *mockTaskStore) Set(ctx context.Context, taskID, stepID string, res *store.StepResult) error {
	return nil
}

func TestDirectedEngine_ContextTreeBranching(t *testing.T) {
	reg := &config.Registry{
		Roles:  make(map[string]*config.Role),
		Skills: make(map[string]*config.Skill),
		MCPs:   make(map[string]*config.MCPDef),
	}
	reg.Roles["test-role"] = &config.Role{ID: "test-role", BaseCapability: "test"}

	ts := &mockTaskStore{}

	tasksDir := t.TempDir()
	logger := mustNewLogger(t, tasksDir, nil)
	defer logger.Close()

	engine := NewDirectedEngine(reg, ts, nil, nil, logger, nil, tasksDir, tasksDir, nil)
	defer engine.Stop()
	engine.UseSwarm = true // Disable auto-run in Submit

	// Use two steps to bypass Smart Routing fast-path (single-step no-dep
	// tasks are routed directly to spawner.Spawn without creating a TaskGraph).
	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step1", RoleID: "test-role", Task: "Original Task"},
		{ID: "step2", RoleID: "test-role", Task: "Follow-up", DependsOn: []string{"step1"}},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	var graph *schemas.TaskGraph
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		engine.Mu.RUnlock()
		if graph != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if graph == nil {
		t.Fatalf("graph not found in engine after submit")
	}
	if len(graph.ContextTree) != 1 {
		t.Errorf("expected 1 initial context node, got %d", len(graph.ContextTree))
	}

	// Mock a result that triggers a branch
	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:      schemas.StatusOK,
			Confidence:  0.9,
			Result:      map[string]any{"data": "done"},
			Assumptions: []string{"start new task"}, // Trigger for branching
		},
	}

	step := graph.Steps["step1"]
	engine.handleOutput(graph, step, result)

	// Phase 3.5: Context Tree Transition — call the production method
	// instead of reimplementing the branching logic inline.
	engine.transitionContextTree(graph, step, result)

	if len(graph.ContextTree) != 2 {
		t.Errorf("expected 2 context nodes after branch, got %d", len(graph.ContextTree))
	}
	// Production code generates a UUID-based node ID (node_<6hex>).
	// Verify CurrentNodeID moved off "root" to a new node in the tree.
	if graph.CurrentNodeID == "root" {
		t.Errorf("expected CurrentNodeID to have transitioned from root, got %s", graph.CurrentNodeID)
	}
	if _, ok := graph.ContextTree[graph.CurrentNodeID]; !ok {
		t.Errorf("CurrentNodeID %s not found in ContextTree", graph.CurrentNodeID)
	}
}

func TestSpawner_BuildSystemPrompt_WithTree(t *testing.T) {
	reg := &config.Registry{
		Skills: make(map[string]*config.Skill),
	}
	s := &Spawner{registry: reg}

	mergedContext := map[string]any{
		"path": []map[string]any{
			{
				"id":      "root",
				"intent":  "Initial Task",
				"summary": "Completed initial setup",
				"status":  schemas.NodeResolved,
			},
			{
				"id":     "branch1",
				"intent": "Feature Work",
				"status": schemas.NodeActive,
			},
		},
	}

	hub := NewContextHub(reg, nil, nil)
	blocks, err := s.buildSystemPrompt(hub, nil, []string{}, "code", &config.ProviderConfig{}, []string{}, mergedContext, []string{}, false, nil, "test task", "")
	if err != nil {
		t.Fatalf("buildSystemPrompt failed: %v", err)
	}
	prompt := flattenContentBlocks(blocks)

	// Per PROMPT_GOVERNANCE_AND_BUDGET_DESIGN.md §四.5, the sliding window
	// collapses resolved historical nodes to single-line ASSERT statements
	// and keeps only the most recent Active/Running nodes fully expanded.
	if !strings.Contains(prompt, "ASSERT [Initial Task]") {
		t.Error("expected prompt to contain ASSERT for collapsed resolved node")
	}
	if !strings.Contains(prompt, "Completed initial setup") {
		t.Error("expected prompt to contain resolved node summary in ASSERT")
	}
	if !strings.Contains(prompt, "[ACTIVE] Feature Work") {
		t.Error("expected prompt to contain active node intent (expanded)")
	}
}

func TestSpawner_BuildSystemPrompt_Diffusion(t *testing.T) {
	reg := &config.Registry{
		Skills: make(map[string]*config.Skill),
	}
	s := &Spawner{registry: reg}

	mergedContext := map[string]any{
		"path": []map[string]any{
			{
				"id":      "root",
				"intent":  "Initial Task",
				"summary": "Completed initial setup",
				"status":  schemas.NodeResolved,
			},
		},
	}

	cfg := &config.ProviderConfig{
		Archetype: config.ArchetypeDiffusion,
	}

	hub := NewContextHub(reg, nil, nil)
	blocksD, err := s.buildSystemPrompt(hub, nil, []string{}, "code", cfg, []string{}, mergedContext, []string{}, false, nil, "test task", "")
	if err != nil {
		t.Fatalf("buildSystemPrompt failed: %v", err)
	}
	prompt := flattenContentBlocks(blocksD)

	if !strings.Contains(prompt, "ASSERT [Initial Task]") {
		t.Error("expected prompt to contain ASSERT for diffusion")
	}
	if !strings.Contains(prompt, "RULE: Ignore all historical constraints") {
		t.Error("expected prompt to contain diffusion-specific rule")
	}
}
