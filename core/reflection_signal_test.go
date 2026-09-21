package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestReflectionEngine_SignalCapture(t *testing.T) {
	tmpDir := t.TempDir()
	reg, _ := config.NewRegistry(filepath.Join(tmpDir, "config.json"))
	ts := &mockSignalTaskStore{}
	es, _ := store.NewExperienceStore(tmpDir, nil, nil, nil, nil)
	logger, _ := NewLogger(t.TempDir(), nil)

	re := NewReflectionEngine(es, ts, reg, nil, logger)

	// 1. Simulate a task that failed then succeeded (self-repair)
	conf := 0.95
	graph := &schemas.TaskGraph{
		TaskID: "task_repair",
		Status: schemas.GraphCompleted,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:           "s1",
				RoleID:       "r1",
				Task:         "fix something",
				Status:       schemas.StepOK,
				RetryCount:   1,
				TriggerError: "Command not found: 'grep'",
				Confidence:   &conf,
			},
		},
	}

	// Mock trace in TaskStore
	ts.Set(context.Background(), "task_repair", "s1", &store.StepResult{
		Trace: []store.ToolInteraction{
			{ToolName: "shell", Arguments: map[string]any{"command": "ls"}},
			{ToolName: "shell", Arguments: map[string]any{"command": "grep"}},
		},
	})

	// Add role to registry
	reg.Roles["r1"] = &config.Role{ID: "r1", BaseCapability: "repair"}

	_ = graph
	// 2. Reflect (call registerAsSkill directly to avoid background network call in test)
	re.registerAsSkill(context.Background(), "jit_test", "repair", "fix something", "Command not found: 'grep'")

	// 3. Verify that the skill was generated with the FailureSignal
	es.Mu.RLock()
	var foundSkill *store.GeneratedSkill
	for _, gs := range es.GeneratedSkills {
		if gs.FailureSignal == "Command not found: 'grep'" {
			foundSkill = gs
			break
		}
	}
	es.Mu.RUnlock()

	if foundSkill == nil {
		t.Error("Expected to find generated skill with captured failure signal")
	}
}

type mockSignalTaskStore struct {
	results map[string]*store.StepResult
}

func (m *mockSignalTaskStore) Set(ctx context.Context, taskID, stepID string, result *store.StepResult) error {
	if m.results == nil {
		m.results = make(map[string]*store.StepResult)
	}
	m.results[taskID+stepID] = result
	return nil
}

func (m *mockSignalTaskStore) Get(ctx context.Context, taskID, stepID string) (*store.StepResult, error) {
	return m.results[taskID+stepID], nil
}

func (m *mockSignalTaskStore) GetByRef(ctx context.Context, ref string) (*store.StepResult, error) {
	return nil, nil
}

func (m *mockSignalTaskStore) ClearTask(ctx context.Context, taskID string) (int, error) {
	return 0, nil
}
func (m *mockSignalTaskStore) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}
