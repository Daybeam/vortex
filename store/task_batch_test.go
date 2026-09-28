package store

import (
	"context"
	"testing"
)

// TestGetBatch_ReturnsAllResults is a regression test for audit P-H1-H4:
// GetBatch must return all step results that exist in the store, matching
// what individual Get calls would return — but in a single call instead of N.
//
// Before the fix, 4 call sites (scheduler_dag InputMapping, ContextRefs,
// reflection trace fetch, foldNode) looped over Get, causing N+1 queries.
// After the fix, all 4 use GetBatch which does a single query (SQLite) or
// a single locked loop (File backend).
//
// Reproduction: Set N step results, then GetBatch them and verify all are
// returned correctly.
func TestGetBatch_ReturnsAllResults(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileTaskBackend(dir)
	ts := NewTaskStore(backend)
	ctx := context.Background()

	// Set 5 step results.
	stepIDs := []string{"s1", "s2", "s3", "s4", "s5"}
	for _, sid := range stepIDs {
		err := ts.Set(ctx, "task-batch", sid, &StepResult{
			Data:       "result-" + sid,
			Confidence: 0.9,
		})
		if err != nil {
			t.Fatalf("Set(%s) failed: %v", sid, err)
		}
	}

	// Batch-fetch all 5.
	results, err := ts.GetBatch(ctx, "task-batch", stepIDs)
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}

	if len(results) != len(stepIDs) {
		t.Fatalf("expected %d results, got %d", len(stepIDs), len(results))
	}

	// Verify each result matches what Get would return.
	for _, sid := range stepIDs {
		batchRes, ok := results[sid]
		if !ok {
			t.Errorf("step %s missing from batch results", sid)
			continue
		}
		singleRes, err := ts.Get(ctx, "task-batch", sid)
		if err != nil {
			t.Errorf("Get(%s) failed: %v", sid, err)
			continue
		}
		if batchRes.Data != singleRes.Data {
			t.Errorf("step %s: batch Data=%v != single Data=%v", sid, batchRes.Data, singleRes.Data)
		}
		if batchRes.Confidence != singleRes.Confidence {
			t.Errorf("step %s: batch Confidence=%v != single Confidence=%v", sid, batchRes.Confidence, singleRes.Confidence)
		}
	}
}

// TestGetBatch_MissingStepsOmitted verifies that steps not in the store
// are simply absent from the result map (not an error).
func TestGetBatch_MissingStepsOmitted(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileTaskBackend(dir)
	ts := NewTaskStore(backend)
	ctx := context.Background()

	// Set only s1 and s3.
	ts.Set(ctx, "task-missing", "s1", &StepResult{Data: "ok"})
	ts.Set(ctx, "task-missing", "s3", &StepResult{Data: "ok"})

	// Request s1, s2 (missing), s3, s4 (missing).
	results, err := ts.GetBatch(ctx, "task-missing", []string{"s1", "s2", "s3", "s4"})
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results (s1, s3), got %d: %v", len(results), results)
	}
	if results["s1"] == nil {
		t.Error("s1 should be present")
	}
	if results["s3"] == nil {
		t.Error("s3 should be present")
	}
	if results["s2"] != nil {
		t.Error("s2 should be absent (missing)")
	}
	if results["s4"] != nil {
		t.Error("s4 should be absent (missing)")
	}
}

// TestGetBatch_EmptyInput returns empty map without error.
func TestGetBatch_EmptyInput(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileTaskBackend(dir)
	ts := NewTaskStore(backend)

	results, err := ts.GetBatch(context.Background(), "task-empty", nil)
	if err != nil {
		t.Fatalf("GetBatch with nil input failed: %v", err)
	}
	if results == nil {
		t.Fatal("expected non-nil map")
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// TestGetBatch_SingleCallReplacesN verifies the core P-H1-H4 property:
// when the backend implements batchLoader, GetBatch makes exactly 1 LoadBatch
// call (not N Load calls). This is the whole point of the fix — replacing
// N+1 queries with a single query.
func TestGetBatch_SingleCallReplacesN(t *testing.T) {
	ctx := context.Background()
	backend := &countingBackend{data: make(map[string][]byte)}

	// Pre-populate with 10 steps.
	for i := 0; i < 10; i++ {
		sid := "s" + string(rune('0'+i))
		backend.data[sid] = []byte(`{"data":"ok"}`)
	}

	ts := NewTaskStore(backend)

	// Request all 10 steps.
	stepIDs := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		stepIDs = append(stepIDs, "s"+string(rune('0'+i)))
	}

	_, err := ts.GetBatch(ctx, "task-count", stepIDs)
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}

	// countingBackend implements batchLoader, so GetBatch must use LoadBatch
	// exactly once and must NOT call Load at all.
	if backend.loadBatchCount != 1 {
		t.Errorf("expected exactly 1 LoadBatch call, got %d", backend.loadBatchCount)
	}
	if backend.loadCount != 0 {
		t.Errorf("expected 0 Load calls (batch path used), got %d", backend.loadCount)
	}
}

// TestGetBatch_FallbackLoopsWhenNoBatchLoader verifies the fallback path:
// when the backend does NOT implement batchLoader, GetBatch loops over Load.
// This ensures the fallback works correctly but is less efficient.
func TestGetBatch_FallbackLoopsWhenNoBatchLoader(t *testing.T) {
	ctx := context.Background()
	backend := &fallbackBackend{data: make(map[string][]byte)}

	for i := 0; i < 5; i++ {
		sid := "s" + string(rune('0'+i))
		backend.data[sid] = []byte(`{"data":"ok"}`)
	}

	ts := NewTaskStore(backend)

	stepIDs := []string{"s0", "s1", "s2", "s3", "s4"}
	results, err := ts.GetBatch(ctx, "task-fallback", stepIDs)
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	// Fallback path must call Load once per step.
	if backend.loadCount != 5 {
		t.Errorf("expected 5 Load calls (fallback path), got %d", backend.loadCount)
	}
}

// countingBackend implements ITaskBackend AND batchLoader, so GetBatch uses
// the single LoadBatch call path.
type countingBackend struct {
	data           map[string][]byte
	loadCount      int
	loadBatchCount int
}

func (b *countingBackend) Save(ctx context.Context, taskID, stepID string, data []byte) error {
	b.data[stepID] = data
	return nil
}

func (b *countingBackend) Load(ctx context.Context, taskID, stepID string) ([]byte, error) {
	b.loadCount++
	return b.data[stepID], nil
}

func (b *countingBackend) LoadBatch(ctx context.Context, taskID string, stepIDs []string) (map[string][]byte, error) {
	b.loadBatchCount++
	result := make(map[string][]byte)
	for _, sid := range stepIDs {
		if data, ok := b.data[sid]; ok {
			result[sid] = data
		}
	}
	return result, nil
}

func (b *countingBackend) Delete(ctx context.Context, taskID string) (int, error) { return 0, nil }
func (b *countingBackend) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}

// fallbackBackend implements ITaskBackend but NOT batchLoader, so GetBatch
// falls back to looping over Load.
type fallbackBackend struct {
	data      map[string][]byte
	loadCount int
}

func (b *fallbackBackend) Save(ctx context.Context, taskID, stepID string, data []byte) error {
	b.data[stepID] = data
	return nil
}

func (b *fallbackBackend) Load(ctx context.Context, taskID, stepID string) ([]byte, error) {
	b.loadCount++
	return b.data[stepID], nil
}

func (b *fallbackBackend) Delete(ctx context.Context, taskID string) (int, error) { return 0, nil }
func (b *fallbackBackend) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}
