package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// --- NewBudgetGate ---

func TestNewBudgetGate_DisabledReturnsNil(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	cfg := config.BudgetGateConfig{Enabled: false, PerTaskTokens: 1000}
	if g := NewBudgetGate(cfg, s); g != nil {
		t.Fatal("disabled BudgetGate should return nil")
	}
}

func TestNewBudgetGate_NilSchedulerReturnsNil(t *testing.T) {
	cfg := config.BudgetGateConfig{Enabled: true, PerTaskTokens: 1000}
	if g := NewBudgetGate(cfg, nil); g != nil {
		t.Fatal("nil scheduler should return nil")
	}
}

func TestNewBudgetGate_EnabledReturnsNonNil(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	cfg := config.BudgetGateConfig{Enabled: true, PerTaskTokens: 1000}
	g := NewBudgetGate(cfg, s)
	if g == nil {
		t.Fatal("enabled BudgetGate should return non-nil")
	}
}

// --- extractTokens ---

func TestExtractTokens_FromUsage(t *testing.T) {
	payload := map[string]any{
		"usage": map[string]any{
			"total_tokens":      500,
			"prompt_tokens":     300,
			"completion_tokens": 200,
		},
	}
	if got := extractTokens(payload); got != 500 {
		t.Fatalf("expected 500 tokens, got %d", got)
	}
}

func TestExtractTokens_NoUsage(t *testing.T) {
	if got := extractTokens(nil); got != 0 {
		t.Fatalf("nil payload: expected 0, got %d", got)
	}
	if got := extractTokens(map[string]any{}); got != 0 {
		t.Fatalf("empty payload: expected 0, got %d", got)
	}
	if got := extractTokens(map[string]any{"confidence": 0.9}); got != 0 {
		t.Fatalf("no usage key: expected 0, got %d", got)
	}
}

func TestExtractTokens_FloatValues(t *testing.T) {
	// JSON unmarshal produces float64 for numbers.
	payload := map[string]any{
		"usage": map[string]any{
			"total_tokens": float64(750),
		},
	}
	if got := extractTokens(payload); got != 750 {
		t.Fatalf("float total_tokens: expected 750, got %d", got)
	}
}

func TestExtractTokens_FallbackToPromptPlusCompletion(t *testing.T) {
	payload := map[string]any{
		"usage": map[string]any{
			"prompt_tokens":     400,
			"completion_tokens": 100,
		},
	}
	if got := extractTokens(payload); got != 500 {
		t.Fatalf("fallback: expected 500, got %d", got)
	}
}

// --- extractRoleID ---

func TestExtractRoleID(t *testing.T) {
	if got := extractRoleID(map[string]any{"role_id": "invest"}); got != "invest" {
		t.Fatalf("expected 'invest', got %q", got)
	}
	if got := extractRoleID(nil); got != "" {
		t.Fatalf("nil payload: expected '', got %q", got)
	}
	if got := extractRoleID(map[string]any{}); got != "" {
		t.Fatalf("empty payload: expected '', got %q", got)
	}
}

// --- BudgetGate integration ---

// setupRunningTask creates a running task graph in the scheduler for testing.
func setupRunningTask(t *testing.T, s *DirectedEngine, taskID string) {
	t.Helper()
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, Task: "do work"},
		},
		Status: schemas.GraphRunning,
	}
	_, cancel := context.WithCancel(context.Background())
	s.Mu.Lock()
	s.graphs[taskID] = graph
	s.cancelFuncs[taskID] = cancel
	s.doneChans[taskID] = make(chan struct{})
	s.Mu.Unlock()
}

func TestBudgetGate_PerTaskThresholdSuspends(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	taskID := "budget-task-1"
	setupRunningTask(t, s, taskID)

	cfg := config.BudgetGateConfig{
		Enabled:       true,
		PerTaskTokens: 1000,
	}
	g := NewBudgetGate(cfg, s)
	defer g.Wire()()

	// Step 1: 600 tokens → under threshold, no suspend.
	DefaultBus.Publish(NewAgentEvent(taskID, "s1", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 600},
		"role_id": "invest",
	}))
	if g.IsSuspended(taskID) {
		t.Fatal("task should not be suspended at 600 tokens (threshold 1000)")
	}
	if got := g.GetTaskUsage(taskID); got != 600 {
		t.Fatalf("expected 600 usage, got %d", got)
	}

	// Step 2: 500 tokens → cumulative 1100 > 1000 → suspend.
	DefaultBus.Publish(NewAgentEvent(taskID, "s2", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 500},
		"role_id": "invest",
	}))
	if !g.IsSuspended(taskID) {
		t.Fatal("task should be suspended at 1100 tokens (threshold 1000)")
	}

	// Verify the graph status changed to suspended.
	s.Mu.RLock()
	status := s.graphs[taskID].Status
	s.Mu.RUnlock()
	if status != schemas.GraphSuspended {
		t.Fatalf("expected graph status %q, got %q", schemas.GraphSuspended, status)
	}
}

func TestBudgetGate_PerRoleThresholdSuspends(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	taskID := "budget-task-role"
	setupRunningTask(t, s, taskID)

	cfg := config.BudgetGateConfig{
		Enabled:       true,
		PerRoleTokens: 800,
	}
	g := NewBudgetGate(cfg, s)
	defer g.Wire()()

	// Step 1: 500 tokens for role "invest" → under threshold.
	DefaultBus.Publish(NewAgentEvent(taskID, "s1", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 500},
		"role_id": "invest",
	}))
	if g.IsSuspended(taskID) {
		t.Fatal("task should not be suspended at 500 role tokens (threshold 800)")
	}

	// Step 2: 400 tokens for same role → cumulative 900 > 800 → suspend.
	DefaultBus.Publish(NewAgentEvent(taskID, "s2", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 400},
		"role_id": "invest",
	}))
	if !g.IsSuspended(taskID) {
		t.Fatal("task should be suspended at 900 role tokens (threshold 800)")
	}
	if got := g.GetRoleUsage("invest"); got != 900 {
		t.Fatalf("expected 900 role usage, got %d", got)
	}
}

func TestBudgetGate_WithinBudgetNoSuspend(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	taskID := "budget-task-ok"
	setupRunningTask(t, s, taskID)

	cfg := config.BudgetGateConfig{
		Enabled:       true,
		PerTaskTokens: 10000,
	}
	g := NewBudgetGate(cfg, s)
	defer g.Wire()()

	// Multiple steps, all within budget.
	for i := 0; i < 5; i++ {
		DefaultBus.Publish(NewAgentEvent(taskID, "s1", string(EventStepCompleted), map[string]any{
			"usage":   map[string]any{"total_tokens": 100},
			"role_id": "invest",
		}))
	}
	if g.IsSuspended(taskID) {
		t.Fatal("task should not be suspended at 500 tokens (threshold 10000)")
	}
	if got := g.GetTaskUsage(taskID); got != 500 {
		t.Fatalf("expected 500 usage, got %d", got)
	}
}

func TestBudgetGate_NoDoubleSuspend(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	taskID := "budget-task-double"
	setupRunningTask(t, s, taskID)

	cfg := config.BudgetGateConfig{
		Enabled:       true,
		PerTaskTokens: 100,
	}
	g := NewBudgetGate(cfg, s)
	defer g.Wire()()

	// First event exceeds threshold → suspend.
	DefaultBus.Publish(NewAgentEvent(taskID, "s1", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 200},
		"role_id": "invest",
	}))
	if !g.IsSuspended(taskID) {
		t.Fatal("task should be suspended")
	}

	// Second event also exceeds → should NOT try to suspend again
	// (SuspendTask would error since status is already GraphSuspended).
	// The BudgetGate should handle this gracefully.
	DefaultBus.Publish(NewAgentEvent(taskID, "s2", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 300},
		"role_id": "invest",
	}))
	// Should still be suspended, no panic.
	if !g.IsSuspended(taskID) {
		t.Fatal("task should still be suspended")
	}
	if got := g.GetTaskUsage(taskID); got != 500 {
		t.Fatalf("expected 500 total usage, got %d", got)
	}
}

func TestBudgetGate_MultipleTasksIsolated(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	task1 := "budget-task-a"
	task2 := "budget-task-b"
	setupRunningTask(t, s, task1)
	setupRunningTask(t, s, task2)

	cfg := config.BudgetGateConfig{
		Enabled:       true,
		PerTaskTokens: 1000,
	}
	g := NewBudgetGate(cfg, s)
	defer g.Wire()()

	// task1 uses 1200 tokens → suspended.
	DefaultBus.Publish(NewAgentEvent(task1, "s1", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 1200},
		"role_id": "invest",
	}))
	if !g.IsSuspended(task1) {
		t.Fatal("task1 should be suspended")
	}

	// task2 uses 500 tokens → NOT suspended (separate budget).
	DefaultBus.Publish(NewAgentEvent(task2, "s1", string(EventStepCompleted), map[string]any{
		"usage":   map[string]any{"total_tokens": 500},
		"role_id": "research",
	}))
	if g.IsSuspended(task2) {
		t.Fatal("task2 should NOT be suspended (500 < 1000)")
	}
	if got := g.GetTaskUsage(task1); got != 1200 {
		t.Fatalf("task1 usage: expected 1200, got %d", got)
	}
	if got := g.GetTaskUsage(task2); got != 500 {
		t.Fatalf("task2 usage: expected 500, got %d", got)
	}
}

func TestBudgetGate_ZeroTokensSkipped(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	taskID := "budget-task-zero"
	setupRunningTask(t, s, taskID)

	cfg := config.BudgetGateConfig{
		Enabled:       true,
		PerTaskTokens: 100,
	}
	g := NewBudgetGate(cfg, s)
	defer g.Wire()()

	// Event with no usage data → should be skipped (0 tokens).
	DefaultBus.Publish(NewAgentEvent(taskID, "s1", string(EventStepCompleted), map[string]any{
		"confidence": 0.9,
	}))
	if got := g.GetTaskUsage(taskID); got != 0 {
		t.Fatalf("expected 0 usage, got %d", got)
	}
	if g.IsSuspended(taskID) {
		t.Fatal("task should not be suspended with 0 tokens")
	}
}
