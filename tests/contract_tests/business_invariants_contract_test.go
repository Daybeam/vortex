package contract_tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/store"
)

// =============================================================================
// Feature: Staged Workspace & Snapshot Rollback
// Invariant: Before step execution, a file snapshot must be taken in
// .staging/snapshots. On step failure, the disk is automatically rolled back
// to keep the host workspace clean.
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Staged Workspace
// =============================================================================

// TestContract_StagedWorkspace_RollbackOnFailure
//
// Scenario: Automatic disk snapshot rollback on step execution failure
//   Given: Before step Step_Edit starts, StagedWorkspace has taken a file
//          snapshot at .staging/snapshots/step_101
//   When:  The step corrupts core/scheduler.go mid-edit (panic or validation
//          failure)
//   Then:  The asset manager must automatically trigger SnapshotRollback
//   And:   core/scheduler.go content must be restored to its exact pre-step
//          state
//   And:   No uncommitted intermediate corrupt files may remain
func TestContract_StagedWorkspace_RollbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	sw := core.NewStagedWorkspace(dir)

	// Given: workspace has original files before step execution.
	originalContent := "package scheduler\n// original code\n"
	if err := os.WriteFile(filepath.Join(dir, "scheduler.go"), []byte(originalContent), 0644); err != nil {
		t.Fatalf("setup: write scheduler.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":"1.0"}`), 0644); err != nil {
		t.Fatalf("setup: write config.json: %v", err)
	}

	// Given: StagedWorkspace takes a pre-step snapshot.
	pre, err := sw.TakePreStepSnapshot("step_101")
	if err != nil {
		t.Fatalf("TakePreStepSnapshot: %v", err)
	}
	if pre == nil {
		t.Fatal("expected non-nil pre-step snapshot")
	}

	// When: step execution fails — files are corrupted, deleted, and new files created.
	// Simulate a panic mid-edit: scheduler.go is left in a broken state.
	if err := os.WriteFile(filepath.Join(dir, "scheduler.go"), []byte("BROKEN // panic mid-edit"), 0644); err != nil {
		t.Fatalf("corrupt scheduler.go: %v", err)
	}
	// config.json is deleted.
	if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("remove config.json: %v", err)
	}
	// A new intermediate file is created that should not survive rollback.
	if err := os.WriteFile(filepath.Join(dir, "tmp_intermediate.go"), []byte("should not survive"), 0644); err != nil {
		t.Fatalf("create tmp_intermediate.go: %v", err)
	}

	// Then: Rollback is triggered.
	if err := sw.Rollback(pre); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// And: scheduler.go content must be restored to exact original state.
	data, err := os.ReadFile(filepath.Join(dir, "scheduler.go"))
	if err != nil {
		t.Fatalf("scheduler.go should exist after rollback: %v", err)
	}
	if string(data) != originalContent {
		t.Errorf("scheduler.go not restored to original:\n  got:  %q\n  want: %q", string(data), originalContent)
	}

	// And: config.json must be restored (was deleted during failed step).
	data, err = os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config.json should be restored after rollback: %v", err)
	}
	if string(data) != `{"version":"1.0"}` {
		t.Errorf("config.json not restored correctly: %q", string(data))
	}

	// And: no intermediate files should remain (workspace is clean).
	if _, err := os.Stat(filepath.Join(dir, "tmp_intermediate.go")); !os.IsNotExist(err) {
		t.Error("tmp_intermediate.go should have been removed by rollback")
	}
}

// TestContract_StagedWorkspace_RollbackNilPreIsError
//
// Scenario: Rollback requires a pre-step snapshot
//   Given: No pre-step snapshot was taken (pre = nil)
//   When:  Rollback(nil) is called
//   Then:  Must return an error to prevent ungrounded rollback
func TestContract_StagedWorkspace_RollbackNilPreIsError(t *testing.T) {
	dir := t.TempDir()
	sw := core.NewStagedWorkspace(dir)
	if err := sw.Rollback(nil); err == nil {
		t.Error("expected error when rolling back with nil pre-snapshot")
	}
}

// =============================================================================
// Feature: Sieve Guardian
// Invariant: Automatically intercepts model loop hallucinations
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Sieve Guardian
// =============================================================================

// TestContract_SieveGuardian_LoopDetection
//
// Scenario: Automatically intercept model loop hallucination
//   Given: Sieve guardian repetition threshold is 3
//   When:  The model outputs 4 identical tool calls run_command(args=["ls -R"])
//   Then:  The sieve guardian must forcibly intercept before the 4th call
//   And:   Task status immediately becomes FAILED with error code
//          InfiniteLoopDetected
func TestContract_SieveGuardian_LoopDetection(t *testing.T) {
	sieve := core.NewSieve(10) // maxKeep=10, threshold=3
	taskID := "task-sieve-contract"

	// First 3 calls: should pass (threshold=3, check counts BEFORE appending).
	for i := 1; i <= 3; i++ {
		ok, _ := sieve.InspectToolCalls(taskID, "", []store.ToolInteraction{
			{ToolName: "run_command", Arguments: map[string]any{"args": []string{"ls", "-R"}}},
		})
		if !ok {
			t.Fatalf("call %d: should pass (below threshold)", i)
		}
	}

	// 4th call: consecutiveCount=3 >= threshold=3 → intercepted.
	ok, reason := sieve.InspectToolCalls(taskID, "", []store.ToolInteraction{
		{ToolName: "run_command", Arguments: map[string]any{"args": []string{"ls", "-R"}}},
	})
	if ok {
		t.Fatal("4th identical call should be intercepted")
	}
	if reason != "InfiniteLoopDetected" {
		t.Errorf("expected reason 'InfiniteLoopDetected', got %q", reason)
	}
}
