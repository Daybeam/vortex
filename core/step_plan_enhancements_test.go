package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ─── #4: Plan Drift Detection ──────────────────────────────────────────────

func TestSieve_SetPlan_AndLoadPlan(t *testing.T) {
	s := NewSieve(100)
	taskID := "task-plan-1"

	// No plan initially.
	if got := s.LoadPlan(taskID); got != "" {
		t.Errorf("expected empty plan, got %q", got)
	}

	// Set and load.
	plan := "1. Read config.json\n2. Edit auth.go\n3. Run tests"
	s.SetPlan(taskID, plan)
	if got := s.LoadPlan(taskID); got != plan {
		t.Errorf("expected plan %q, got %q", plan, got)
	}
}

func TestSieve_DetectPlanDrift_NoPlan(t *testing.T) {
	s := NewSieve(100)
	// No plan set → no drift detection.
	tc := schemas.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "auth.go"}}
	if msg := s.DetectPlanDrift("task-no-plan", tc); msg != "" {
		t.Errorf("expected no drift without plan, got %q", msg)
	}
}

func TestSieve_DetectPlanDrift_ReadToolNeverDrifts(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "1. Edit auth.go\n2. Run tests")

	// Read tool not in plan → no drift (reads are always safe).
	tc := schemas.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "unknown.go"}}
	if msg := s.DetectPlanDrift("task-1", tc); msg != "" {
		t.Errorf("read tool should never drift, got %q", msg)
	}
}

func TestSieve_DetectPlanDrift_ToolNameInPlan(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "1. Read config.json\n2. edit auth.go\n3. Run tests")

	// Tool name "edit" appears in plan → no drift.
	tc := schemas.ToolCall{Name: "edit", Arguments: map[string]any{"file": "auth.go"}}
	if msg := s.DetectPlanDrift("task-1", tc); msg != "" {
		t.Errorf("tool name in plan should not drift, got %q", msg)
	}
}

func TestSieve_DetectPlanDrift_ArgValueInPlan(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "1. Read config.json\n2. Write auth.go\n3. Run tests")

	// Tool name "write_file" not in plan, but arg value "auth.go" is → no drift.
	tc := schemas.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "auth.go"}}
	if msg := s.DetectPlanDrift("task-1", tc); msg != "" {
		t.Errorf("arg value in plan should not drift, got %q", msg)
	}
}

func TestSieve_DetectPlanDrift_MutatingToolNotInPlan(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "1. Read config.json\n2. Run tests")

	// "write_file" is a mutating tool, neither name nor args in plan → drift.
	tc := schemas.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "unknown.go"}}
	msg := s.DetectPlanDrift("task-1", tc)
	if msg == "" {
		t.Error("expected drift for mutating tool not in plan")
	}
	if msg != "" && msg[:12] != "[PLAN DRIFT]" {
		t.Errorf("expected [PLAN DRIFT] prefix, got %q", msg[:12])
	}
}

func TestSieve_DetectPlanDrift_PollerToolNeverDrifts(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "1. Read config.json")

	// Poller tool → never drifts.
	tc := schemas.ToolCall{Name: "poll_status", Arguments: map[string]any{}}
	if msg := s.DetectPlanDrift("task-1", tc); msg != "" {
		t.Errorf("poller tool should never drift, got %q", msg)
	}
}

func TestSieve_ClearPlan(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "some plan")
	s.ClearPlan("task-1")
	if got := s.LoadPlan("task-1"); got != "" {
		t.Errorf("expected empty plan after clear, got %q", got)
	}
}

func TestSieve_ClearHistoryAlsoClearsPlan(t *testing.T) {
	s := NewSieve(100)
	s.SetPlan("task-1", "some plan")
	s.ClearHistory("task-1")
	if got := s.LoadPlan("task-1"); got != "" {
		t.Errorf("expected empty plan after ClearHistory, got %q", got)
	}
}

// ─── #10: PendingSteer ─────────────────────────────────────────────────────

func TestChatHarness_PendingSteer_DefaultEmpty(t *testing.T) {
	h := &ChatHarness{}
	h.SteerLock.Lock()
	if h.PendingSteer != "" {
		t.Error("PendingSteer should default to empty")
	}
	h.SteerLock.Unlock()
}

// ─── #13: Adaptive Threshold ───────────────────────────────────────────────

func TestSieve_AdaptiveThreshold_TightensOnDistinctIntercepts(t *testing.T) {
	s := NewSieve(100)
	// Default threshold is 3.
	if s.ToolRepetitionThreshold != 3 {
		t.Fatalf("expected default threshold 3, got %d", s.ToolRepetitionThreshold)
	}

	taskID := "task-adaptive-1"
	// Generate two distinct intercepts.
	callA := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "a.go"}}
	callB := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "b.go"}}

	// Fill history to trigger intercepts.
	for i := 0; i < 4; i++ {
		s.InspectToolCalls(taskID, []store.ToolInteraction{callA})
	}
	// callA should have been intercepted (threshold=3, 4th call triggers).
	// Now trigger with callB.
	for i := 0; i < 4; i++ {
		s.InspectToolCalls(taskID, []store.ToolInteraction{callB})
	}

	// Threshold is NOT tightened yet — tightening is deferred to ClearHistory
	// to avoid changing behavior mid-task.
	if s.ToolRepetitionThreshold != 3 {
		t.Errorf("threshold should not change mid-task, got %d", s.ToolRepetitionThreshold)
	}

	// ClearHistory triggers the tightening (≥2 distinct intercepts).
	s.ClearHistory(taskID)

	// With ≥2 distinct intercepts, threshold should have tightened to 4.
	if s.ToolRepetitionThreshold != 4 {
		t.Errorf("expected threshold 4 after ClearHistory with 2 distinct intercepts, got %d", s.ToolRepetitionThreshold)
	}
}

func TestSieve_AdaptiveThreshold_RespectsMax(t *testing.T) {
	s := NewSieve(100)
	// Force threshold to max.
	s.ToolRepetitionThreshold = adaptiveThresholdMax

	taskID := "task-max-1"
	callA := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "a.go"}}
	callB := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "b.go"}}

	for i := 0; i < 10; i++ {
		s.InspectToolCalls(taskID, []store.ToolInteraction{callA})
		s.InspectToolCalls(taskID, []store.ToolInteraction{callB})
	}

	// ClearHistory would try to tighten, but threshold is already at max.
	s.ClearHistory(taskID)

	// Should not exceed max.
	if s.ToolRepetitionThreshold > adaptiveThresholdMax {
		t.Errorf("threshold should not exceed max %d, got %d", adaptiveThresholdMax, s.ToolRepetitionThreshold)
	}
}

func TestSieve_AdaptiveThreshold_RelaxesAfterCleanTasks(t *testing.T) {
	s := NewSieve(100)
	// Tighten first.
	s.ToolRepetitionThreshold = 5

	// Simulate 10 consecutive clean task completions.
	for i := 0; i < adaptiveRelaxConsecutive; i++ {
		s.ClearHistory("clean-task-" + string(rune('A'+i)))
	}

	// Should have relaxed by 1.
	if s.ToolRepetitionThreshold != 4 {
		t.Errorf("expected threshold 4 after %d clean tasks, got %d", adaptiveRelaxConsecutive, s.ToolRepetitionThreshold)
	}
}

func TestSieve_AdaptiveThreshold_RespectsMin(t *testing.T) {
	s := NewSieve(100)
	s.ToolRepetitionThreshold = adaptiveThresholdMin

	// Simulate many clean tasks — should not go below min.
	for i := 0; i < adaptiveRelaxConsecutive*3; i++ {
		s.ClearHistory("clean-min-" + string(rune('A'+i%26)))
	}

	if s.ToolRepetitionThreshold < adaptiveThresholdMin {
		t.Errorf("threshold should not go below min %d, got %d", adaptiveThresholdMin, s.ToolRepetitionThreshold)
	}
}

// ─── #16: Cache Backpressure ───────────────────────────────────────────────

func TestSieve_CacheBackpressure_EvictsWhenFull(t *testing.T) {
	s := NewSieve(100)
	taskID := "task-cache-bp"

	// Fill cache to maxCachePerTask.
	for i := 0; i < maxCachePerTask; i++ {
		args := map[string]any{"index": i}
		s.CacheToolResult(taskID, "read_file", args, "result-"+string(rune('A'+i%26)))
	}

	// Add one more — should trigger eviction (clear + re-add).
	s.CacheToolResult(taskID, "read_file", map[string]any{"index": 999}, "new-result")

	// The new entry should be present.
	if _, found := s.GetCachedResult(taskID, "read_file", map[string]any{"index": 999}); !found {
		t.Error("new entry should be present after eviction")
	}

	// Old entries should have been cleared (cache was reset).
	if _, found := s.GetCachedResult(taskID, "read_file", map[string]any{"index": 0}); found {
		t.Error("old entry should have been evicted")
	}
}

func TestSieve_CacheBackpressure_BelowLimitNoEviction(t *testing.T) {
	s := NewSieve(100)
	taskID := "task-cache-normal"

	// Add entries below the limit.
	for i := 0; i < 10; i++ {
		args := map[string]any{"index": i}
		s.CacheToolResult(taskID, "read_file", args, "result-"+string(rune('A'+i%26)))
	}

	// All entries should be present.
	for i := 0; i < 10; i++ {
		args := map[string]any{"index": i}
		if _, found := s.GetCachedResult(taskID, "read_file", args); !found {
			t.Errorf("entry %d should be present (below limit)", i)
		}
	}
}
