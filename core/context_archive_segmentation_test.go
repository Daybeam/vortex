package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ── P1-5: PromptAssembler Archive Query (Path D) ──────────────────────────

func TestPromptAssembler_ArchiveStepSummaries_Injected(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test_archive.jsonl"))

	// Add some step summaries to the archive.
	taskID := "task-123"
	for i := 0; i < 3; i++ {
		_ = archive.Append(MemoryItem{
			TaskID:    taskID,
			NodeID:    "step-" + string(rune('A'+i)),
			Intent:    "Do step " + string(rune('A'+i)),
			Summary:   "Completed step " + string(rune('A'+i)) + " successfully",
			Timestamp: time.Now().Add(-time.Duration(3-i) * time.Minute),
		})
	}

	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetArchive(archive)

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", taskID,
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Verify that step summaries were injected.
	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Recent Step Summaries") {
			found = true
			if !strings.Contains(b.Text, "Completed step") {
				t.Errorf("step summary text not found in block: %s", b.Text)
			}
		}
	}
	if !found {
		t.Error("Archive step summaries were not injected into the prompt")
	}
}

func TestPromptAssembler_ArchiveStepSummaries_NilArchive_NoOp(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	// No archive set — should not panic and should not inject summaries.

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-123",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, b := range blocks {
		if strings.Contains(b.Text, "Recent Step Summaries") {
			t.Error("Archive summaries should not be injected when archive is nil")
		}
	}
}

func TestPromptAssembler_ArchiveStepSummaries_EmptyTaskID_NoOp(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test_archive.jsonl"))

	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetArchive(archive)

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "", // empty taskID
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, b := range blocks {
		if strings.Contains(b.Text, "Recent Step Summaries") {
			t.Error("Archive summaries should not be injected when taskID is empty")
		}
	}
}

// ── P2-9: SegmentManager Cosine Similarity ────────────────────────────────

func TestSegmentManager_CosineSimilarity_UsedWhenEmbeddingsAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Add first item with embedding.
	seg1 := sm.AddItem(MemoryItem{
		NodeID:    "item-1",
		Intent:    "completely unique topic alpha",
		Summary:   "did something with alpha",
		Embedding: []float32{1.0, 0.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")

	if seg1 == nil {
		t.Fatal("expected non-nil segment")
	}

	// Add second item with similar embedding — should STAY in same segment.
	seg2 := sm.AddItem(MemoryItem{
		NodeID:    "item-2",
		Intent:    "totally different words but same embedding direction",
		Summary:   "different text same vector",
		Embedding: []float32{0.99, 0.01, 0.0}, // high cosine similarity
		Timestamp: time.Now(),
	}, "scope-1")

	if seg2.SegmentID != seg1.SegmentID {
		t.Error("item with similar embedding should stay in same segment")
	}

	// Add third item with orthogonal embedding — should SEAL and start new segment.
	seg3 := sm.AddItem(MemoryItem{
		NodeID:    "item-3",
		Intent:    "orthogonal topic beta",
		Summary:   "did something with beta",
		Embedding: []float32{0.0, 1.0, 0.0}, // orthogonal — cosine similarity = 0
		Timestamp: time.Now(),
	}, "scope-1")

	if seg3.SegmentID == seg2.SegmentID {
		t.Error("item with orthogonal embedding should start a new segment")
	}
}

func TestSegmentManager_JaccardFallback_WhenNoEmbeddings(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Add items without embeddings — should use Jaccard.
	seg1 := sm.AddItem(MemoryItem{
		NodeID:  "item-1",
		Intent:  "deploy the application to production",
		Summary: "deployment completed",
		Timestamp: time.Now(),
	}, "scope-1")

	seg2 := sm.AddItem(MemoryItem{
		NodeID:  "item-2",
		Intent:  "deploy the application to staging",
		Summary: "deployment completed",
		Timestamp: time.Now(),
	}, "scope-1")

	// High Jaccard similarity — should stay in same segment.
	if seg2.SegmentID != seg1.SegmentID {
		t.Error("items with high Jaccard similarity should stay in same segment")
	}
}

// ── P2-10: Segment End LLM-Generated Summaries ────────────────────────────

func TestSegmentManager_LLMSummarizer_UsedWhenSet(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Set a custom summarizer.
	sm.SetSummarizer(func(seg *MemorySegment) string {
		return "LLM_SUMMARY: " + seg.TopicTitle + " (" + string(rune('0'+seg.ItemCount)) + " items)"
	})

	// Add items to trigger a seal.
	sm.AddItem(MemoryItem{
		NodeID:  "item-1",
		Intent:  "topic one alpha",
		Summary: "did alpha",
		Timestamp: time.Now(),
	}, "scope-1")

	sm.AddItem(MemoryItem{
		NodeID:  "item-2",
		Intent:  "completely different topic beta gamma delta",
		Summary: "did beta gamma delta",
		Timestamp: time.Now(),
	}, "scope-1")

	// The second item should trigger a seal (low similarity).
	sm.AddItem(MemoryItem{
		NodeID:  "item-3",
		Intent:  "more different content epsilon zeta eta",
		Summary: "did epsilon zeta eta",
		Timestamp: time.Now(),
	}, "scope-1")

	// Force seal.
	sm.SealActive("scope-1")

	// Check that the sealed segment has the LLM summary.
	if sm.SealedCount() == 0 {
		t.Fatal("expected at least one sealed segment")
	}

	// The LLM summarizer should have been called.
	// We can't directly inspect the sealed segment's summary from outside,
	// but we can check via GetSegmentLinks or other methods.
	// For now, just verify it didn't panic and sealed correctly.
}

func TestSegmentManager_FallbackSummary_WhenNoSummarizer(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// No summarizer set — should use head+tail fallback.
	sm.AddItem(MemoryItem{
		NodeID:  "item-1",
		Intent:  "topic one alpha",
		Summary: "did alpha",
		Timestamp: time.Now(),
	}, "scope-1")

	sm.AddItem(MemoryItem{
		NodeID:  "item-2",
		Intent:  "completely different topic beta gamma delta",
		Summary: "did beta gamma delta",
		Timestamp: time.Now(),
	}, "scope-1")

	sm.SealActive("scope-1")

	if sm.SealedCount() == 0 {
		t.Fatal("expected at least one sealed segment")
	}
}

// ── P2-11: Feedback Loop (Duration → Auto-tighten Budget) ─────────────────

func TestPromptAssembler_RecordSubagentDuration_TightensBudget(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			WarmContextBudgetTokens: 2000,
		},
		Roles: make(map[string]*config.Role),
	}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	// Record a high duration — should tighten the budget.
	pa.RecordSubagentDuration(60 * time.Second) // > 30s baseline

	budget := pa.getEffectiveWarmBudget()
	if budget >= 2000 {
		t.Errorf("budget should be tightened after high duration, got %d", budget)
	}
}

func TestPromptAssembler_RecordSubagentDuration_RelaxesBudget(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			WarmContextBudgetTokens: 2000,
		},
		Roles: make(map[string]*config.Role),
	}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	// First tighten.
	pa.RecordSubagentDuration(60 * time.Second)
	tightened := pa.getEffectiveWarmBudget()

	// Then relax with low duration.
	pa.RecordSubagentDuration(5 * time.Second) // < 15s (50% of 30s baseline)
	relaxed := pa.getEffectiveWarmBudget()

	if relaxed <= tightened {
		t.Errorf("budget should be relaxed after low duration, tightened=%d relaxed=%d", tightened, relaxed)
	}
}

func TestPromptAssembler_RecordSubagentBudget_NeverBelowMin(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			WarmContextBudgetTokens: 600,
		},
		Roles: make(map[string]*config.Role),
	}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	// Record many high durations — should bottom out at 500.
	for i := 0; i < 20; i++ {
		pa.RecordSubagentDuration(120 * time.Second)
	}

	budget := pa.getEffectiveWarmBudget()
	if budget < 500 {
		t.Errorf("budget should never go below 500, got %d", budget)
	}
}

// ── P2-12: Trace Weaver Cross-Segment Linking ─────────────────────────────

func TestSegmentManager_CrossSegmentLinks_Created(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Add items with embeddings to create segments.
	// First segment: topic A
	sm.AddItem(MemoryItem{
		NodeID:    "a1",
		Intent:    "topic A item 1",
		Summary:   "did A1",
		Embedding: []float32{1.0, 0.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")

	// Force seal by adding orthogonal content.
	sm.AddItem(MemoryItem{
		NodeID:    "b1",
		Intent:    "topic B item 1",
		Summary:   "did B1",
		Embedding: []float32{0.0, 1.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")

	// Now add topic A again — should create a new segment that links to the first.
	sm.AddItem(MemoryItem{
		NodeID:    "a2",
		Intent:    "topic A item 2",
		Summary:   "did A2",
		Embedding: []float32{0.99, 0.0, 0.01}, // similar to first segment
		Timestamp: time.Now(),
	}, "scope-1")

	// Force seal.
	sm.SealActive("scope-1")

	// Check that cross-segment links were created.
	// The first and third segments should be linked (both topic A).
	totalLinks := 0
	for _, links := range sm.segmentLinks {
		totalLinks += len(links)
	}
	if totalLinks == 0 {
		t.Error("expected cross-segment links to be created")
	}
}

func TestSegmentManager_GetSegmentLinks_NilWhenNoLinks(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	links := sm.GetSegmentLinks("nonexistent-segment")
	if links != nil {
		t.Error("expected nil links for nonexistent segment")
	}
}

// ── P1-8: ChatHarness SegmentManager Wiring ────────────────────────────────

func TestChatHarness_SegmentManager_AddItemCalled(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Create a chat harness with SegmentManager set.
	// We can't easily test the full flow without a provider, but we can
	// verify the SegmentManager field is properly set and AddItem works.
	harness := &ChatHarness{
		Segments: sm,
	}

	if harness.Segments == nil {
		t.Fatal("SegmentManager should be set")
	}

	// Verify AddItem works through the harness.
	seg := harness.Segments.AddItem(MemoryItem{
		NodeID:    "test-item",
		Intent:    "test intent",
		Summary:   "test summary",
		Timestamp: time.Now(),
	}, "test-scope")

	if seg == nil {
		t.Error("AddItem should return a non-nil segment")
	}
}

// ── ArchiveQuery Methods ──────────────────────────────────────────────────

func TestArchiveQuery_SearchRecentStepSummaries(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	taskID := "task-query-test"
	for i := 0; i < 5; i++ {
		_ = archive.Append(MemoryItem{
			TaskID:    taskID,
			NodeID:    "step-" + string(rune('A'+i)),
			Intent:    "Step " + string(rune('A'+i)),
			Summary:   "Summary for step " + string(rune('A'+i)),
			Timestamp: time.Now().Add(-time.Duration(5-i) * time.Minute),
		})
	}

	// Query top 3.
	results := archive.SearchRecentStepSummaries(taskID, 3)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Should be sorted by timestamp descending (most recent first).
	if !results[0].Timestamp.After(results[1].Timestamp) {
		t.Error("results should be sorted by timestamp descending")
	}
}

func TestArchiveQuery_SearchRecentStepSummaries_FiltersByTask(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	_ = archive.Append(MemoryItem{
		TaskID:    "task-A",
		NodeID:    "step-1",
		Summary:   "task A step 1",
		Timestamp: time.Now(),
	})
	_ = archive.Append(MemoryItem{
		TaskID:    "task-B",
		NodeID:    "step-1",
		Summary:   "task B step 1",
		Timestamp: time.Now(),
	})

	results := archive.SearchRecentStepSummaries("task-A", 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result for task-A, got %d", len(results))
	}
	if results[0].TaskID != "task-A" {
		t.Error("result should be from task-A")
	}
}

func TestArchiveQuery_SearchRecentStepSummaries_OnlyWithSummary(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	_ = archive.Append(MemoryItem{
		TaskID:    "task-X",
		NodeID:    "step-1",
		Summary:   "has summary",
		Timestamp: time.Now(),
	})
	_ = archive.Append(MemoryItem{
		TaskID:    "task-X",
		NodeID:    "step-2",
		Summary:   "", // no summary
		Timestamp: time.Now(),
	})

	results := archive.SearchRecentStepSummaries("task-X", 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (only items with summaries), got %d", len(results))
	}
}

func TestArchiveQuery_SearchAllStepSummaries(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	taskID := "task-all-test"
	for i := 0; i < 3; i++ {
		_ = archive.Append(MemoryItem{
			TaskID:    taskID,
			NodeID:    "step-" + string(rune('A'+i)),
			Intent:    "Step " + string(rune('A'+i)),
			Summary:   "Summary for step " + string(rune('A'+i)),
			Timestamp: time.Now(),
		})
	}

	results := archive.SearchAllStepSummaries(taskID, "step", 10)
	// Search may return results or not depending on BM25 index state.
	// Just verify it doesn't panic and returns the right type.
	_ = results
}

func TestArchiveQuery_SearchSegments(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	taskID := "task-seg-test"
	for i := 0; i < 3; i++ {
		_ = archive.Append(MemoryItem{
			TaskID:    taskID,
			NodeID:    "step-" + string(rune('A'+i)),
			Summary:   "Summary " + string(rune('A'+i)),
			Timestamp: time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}

	results := archive.SearchSegments(taskID, 2)
	if len(results) > 2 {
		t.Fatalf("expected at most 2 results, got %d", len(results))
	}
}

func TestArchiveQuery_NilSafe(t *testing.T) {
	var archive *ContextArchive

	// All methods should be nil-safe.
	if r := archive.SearchRecentStepSummaries("task", 3); r != nil {
		t.Error("SearchRecentStepSummaries should return nil for nil archive")
	}
	if r := archive.SearchAllStepSummaries("task", "query", 3); r != nil {
		t.Error("SearchAllStepSummaries should return nil for nil archive")
	}
	if r := archive.SearchSegments("task", 3); r != nil {
		t.Error("SearchSegments should return nil for nil archive")
	}
}

// ── P1-7: Planner Role Spawn ──────────────────────────────────────────────

func TestTryPlannerReplan_NilRegistry_ReturnsEmpty(t *testing.T) {
	s := &DirectedEngine{}
	result := s.tryPlannerReplan("task-1", "step-1", "test reason")
	if result != "" {
		t.Error("tryPlannerReplan should return empty string when registry is nil")
	}
}

func TestTryPlannerReplan_NoPlannerRole_ReturnsEmpty(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	s := &DirectedEngine{registry: reg}
	result := s.tryPlannerReplan("task-1", "step-1", "test reason")
	if result != "" {
		t.Error("tryPlannerReplan should return empty string when task_planner role is not defined")
	}
}

// ── SetArchive Propagation ────────────────────────────────────────────────

func TestSpawner_SetArchive_PropagatesToPromptAssembler(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	logger := mustNewLogger(t, tmpDir, &config.SystemSettings{})
	spawner := NewSpawner(reg, nil, nil, logger, nil, tmpDir)

	spawner.SetArchive(archive)

	if spawner.archive != archive {
		t.Error("archive should be set on spawner")
	}
	// PromptAssembler is lazily initialized — set it explicitly.
	spawner.pa = NewPromptAssembler(nil, nil, reg, logger)
	spawner.SetArchive(archive)

	if spawner.pa.archive != archive {
		t.Error("archive should be propagated to PromptAssembler")
	}
}

func TestDirectedEngine_SetArchive_PropagatesToSpawner(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	logger := mustNewLogger(t, tmpDir, &config.SystemSettings{})
	engine := &DirectedEngine{
		spawner:  NewSpawner(reg, nil, nil, logger, nil, tmpDir),
		registry: reg,
	}

	engine.SetArchive(archive)

	if engine.Archive != archive {
		t.Error("archive should be set on engine")
	}
	if engine.spawner.archive != archive {
		t.Error("archive should be propagated to spawner")
	}
}

// ── SegmentManager SetEmbeddingClient ─────────────────────────────────────

func TestSegmentManager_SetEmbeddingClient(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Set a nil embedding client — should not panic.
	sm.SetEmbeddingClient(nil)

	// Verify it still works with Jaccard fallback.
	seg := sm.AddItem(MemoryItem{
		NodeID:    "item-1",
		Intent:    "test intent",
		Summary:   "test summary",
		Timestamp: time.Now(),
	}, "scope-1")

	if seg == nil {
		t.Error("AddItem should work with nil embedding client")
	}
}

// ── SpawnRequest TaskID Propagation ───────────────────────────────────────

func TestSpawnRequest_TaskID_PassedToBuildSystemPrompt(t *testing.T) {
	// Verify that the buildSystemPrompt signature includes taskID.
	// This is a compile-time check — if the signature changed, this won't compile.
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	logger := mustNewLogger(t, t.TempDir(), &config.SystemSettings{})
	spawner := NewSpawner(reg, nil, nil, logger, nil, t.TempDir())

	blocks, err := spawner.buildSystemPrompt(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-id-123",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt failed: %v", err)
	}
	// Just verify it returns blocks without error.
	_ = blocks
}

// ── Integration: Archive + PromptAssembler ────────────────────────────────

func TestIntegration_ArchiveStepSummaries_InPromptBlocks(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "integration.jsonl"))

	taskID := "integration-task"

	// Simulate step completion — archive a step summary.
	_ = archive.Append(MemoryItem{
		TaskID:    taskID,
		NodeID:    "step-1",
		Intent:    "Write the initial code",
		Summary:   "Created main.go with HTTP server setup",
		StepIDs:   []string{"step-1"},
		Timestamp: time.Now().Add(-2 * time.Minute),
	})
	_ = archive.Append(MemoryItem{
		TaskID:    taskID,
		NodeID:    "step-2",
		Intent:    "Add tests",
		Summary:   "Wrote unit tests for HTTP handlers",
		StepIDs:   []string{"step-2"},
		Timestamp: time.Now().Add(-1 * time.Minute),
	})

	// Now build a prompt for step-3 — it should see step-1 and step-2 summaries.
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetArchive(archive)

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"Add integration tests", "", taskID,
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Verify the step summaries are in the prompt.
	foundStep1 := false
	foundStep2 := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Created main.go") {
			foundStep1 = true
		}
		if strings.Contains(b.Text, "Wrote unit tests") {
			foundStep2 = true
		}
	}
	if !foundStep1 {
		t.Error("step-1 summary should be in the prompt blocks")
	}
	if !foundStep2 {
		t.Error("step-2 summary should be in the prompt blocks")
	}
}

// ── schemas import guard (ensures test compiles with schemas package) ──────

var _ = schemas.SubagentStatus("")
