package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaSKG_TransitionRecording(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "caskg.json")

	manager := NewCaSKGManager(path)
	bus := NewEventBus()
	manager.HookEventBus(bus)

	// Simulate sequence: SkillA -> SkillB -> Success
	bus.Publish(NewAgentEvent("task-success", "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
	bus.Publish(NewAgentEvent("task-success", "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
	bus.Publish(NewAgentEvent("task-success", "", string(EventTaskCompleted), nil))

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	boost := manager.GetCausalBoost("SkillA", "SkillB")
	// Since TotalCount < 3, should be 1.0 (cold start)
	if boost != 1.0 {
		t.Errorf("expected 1.0 for cold start, got %f", boost)
	}

	// Add more successful transitions to cross threshold
	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("t-%d", i)
		bus.Publish(NewAgentEvent(tid, "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
		bus.Publish(NewAgentEvent(tid, "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
		bus.Publish(NewAgentEvent(tid, "", string(EventTaskCompleted), nil))
	}
	time.Sleep(200 * time.Millisecond)

	boost = manager.GetCausalBoost("SkillA", "SkillB")
	if boost <= 1.0 {
		t.Errorf("expected boost > 1.0 for successful sequence, got %f", boost)
	}

	// Simulate sequence: SkillA -> SkillC -> Failure
	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("tf-%d", i)
		bus.Publish(NewAgentEvent(tid, "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
		bus.Publish(NewAgentEvent(tid, "s2", string(EventStepCompleted), map[string]any{"skill": "SkillC"}))
		bus.Publish(NewAgentEvent(tid, "", string(EventTaskFailed), nil))
	}
	time.Sleep(200 * time.Millisecond)

	boost = manager.GetCausalBoost("SkillA", "SkillC")
	if boost >= 1.0 {
		t.Errorf("expected penalty < 1.0 for failing sequence, got %f", boost)
	}
}

// TestCaSKG_Save_AutoCreatesDir is a regression test for the bug where
// Save() failed with "no such file or directory" when the parent directory
// (e.g. data/) did not exist, causing causal learning to be silently lost
// across restarts. See: CaSKGManager Save() now calls os.MkdirAll.
func TestCaSKG_Save_AutoCreatesDir(t *testing.T) {
	// Use a deeply nested path that definitely doesn't exist.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "nested", "deep", "causal_skill_graph.json")

	// Sanity: the parent directory does not exist yet.
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: parent dir should not exist")
	}

	manager := NewCaSKGManager(path)

	// Record a transition so the graph has data to save.
	bus := NewEventBus()
	manager.HookEventBus(bus)
	bus.Publish(NewAgentEvent("t1", "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
	bus.Publish(NewAgentEvent("t1", "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
	bus.Publish(NewAgentEvent("t1", "", string(EventTaskCompleted), nil))
	time.Sleep(100 * time.Millisecond)

	// Save() should auto-create the directory and succeed.
	if err := manager.Save(); err != nil {
		t.Fatalf("Save() should auto-create parent dir, got error: %v", err)
	}

	// Verify the file was actually written.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("file should exist after Save(), but does not: %s", path)
	}

	// Verify we can reload the persisted graph.
	reloaded := NewCaSKGManager(path)
	if len(reloaded.Graph.Nodes) == 0 {
		t.Fatalf("reloaded graph should have nodes, but is empty")
	}
}

// =============================================================================
// Feature: CaSKG Metabolic Pruning & Anti-Stale
// Invariant: Stale and low-performing edges are evicted to prevent
// Retention-Adaptation Conflict (Harness-Induced Forgetting).
// Source: docs/completed/2026-10-04/CASKG_METABOLIC_PRUNING_DESIGN.md
// =============================================================================

// TestCaSKG_PruneStaleEdges_FreshHighPerformingRetained
//
// Scenario: Fresh, high-performing edges are retained after pruning
//   Given: Edges with recent LastUpdated and success rate >= minSuccessRate
//   When:  PruneStaleEdges is called
//   Then:  All fresh, high-performing edges remain in the graph
func TestCaSKG_PruneStaleEdges_FreshHighPerformingRetained(t *testing.T) {
	m := NewCaSKGManager("")

	// Create a fresh, high-performing edge: 8 successes / 10 total = 80%.
	m.mu.Lock()
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  8,
			TotalCount:    10,
			CausalLift:    0.8,
			LastUpdated:   time.Now(),
		},
	}
	m.mu.Unlock()

	m.PruneStaleEdges(7*24*time.Hour, 0.3)

	m.mu.RLock()
	edge, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if !ok {
		t.Fatal("fresh high-performing edge should be retained")
	}
	if edge.TotalCount != 10 {
		t.Errorf("edge data should be unchanged: TotalCount=%d, want 10", edge.TotalCount)
	}
}

// TestCaSKG_PruneStaleEdges_StaleEvicted
//
// Scenario: Edges unused beyond maxAge are evicted
//   Given: An edge with LastUpdated 10 days ago (maxAge = 7 days)
//   When:  PruneStaleEdges is called
//   Then:  The stale edge is removed from the graph
func TestCaSKG_PruneStaleEdges_StaleEvicted(t *testing.T) {
	m := NewCaSKGManager("")

	m.mu.Lock()
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  8,
			TotalCount:    10,
			CausalLift:    0.8,
			LastUpdated:   time.Now().Add(-10 * 24 * time.Hour), // 10 days ago
		},
	}
	m.mu.Unlock()

	m.PruneStaleEdges(7*24*time.Hour, 0.3)

	m.mu.RLock()
	_, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if ok {
		t.Error("stale edge (10 days old, maxAge=7d) should be evicted")
	}
}

// TestCaSKG_PruneStaleEdges_UnderperformingEvicted
//
// Scenario: Edges with TotalCount >= 5 and success rate < minSuccessRate are evicted
//   Given: An edge with 1 success / 10 total = 10% (below 30% threshold)
//   When:  PruneStaleEdges is called
//   Then:  The underperforming edge is removed
func TestCaSKG_PruneStaleEdges_UnderperformingEvicted(t *testing.T) {
	m := NewCaSKGManager("")

	m.mu.Lock()
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  1,
			TotalCount:    10, // 10% success rate, well below 30% threshold
			CausalLift:    0.1,
			LastUpdated:   time.Now(),
		},
	}
	m.mu.Unlock()

	m.PruneStaleEdges(7*24*time.Hour, 0.3)

	m.mu.RLock()
	_, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if ok {
		t.Error("underperforming edge (10% success, threshold=30%) should be evicted")
	}
}

// TestCaSKG_PruneStaleEdges_LowTotalCountSpared
//
// Scenario: Edges with TotalCount < 5 are spared from performance pruning
//   Given: An edge with 1 success / 3 total = 33% (below threshold but < 5 total)
//   When:  PruneStaleEdges is called
//   Then:  The edge is retained (not enough data to prune)
func TestCaSKG_PruneStaleEdges_LowTotalCountSpared(t *testing.T) {
	m := NewCaSKGManager("")

	m.mu.Lock()
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  1,
			TotalCount:    3, // 33% success but only 3 observations — too few to prune
			CausalLift:    0.33,
			LastUpdated:   time.Now(),
		},
	}
	m.mu.Unlock()

	m.PruneStaleEdges(7*24*time.Hour, 0.3)

	m.mu.RLock()
	_, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if !ok {
		t.Error("edge with TotalCount < 5 should be spared (insufficient data)")
	}
}

// TestCaSKG_PruneStaleEdges_LegacyZeroTimeSpared
//
// Scenario: Legacy edges with zero LastUpdated are spared from time-decay pruning
//   Given: An edge loaded from old JSON (LastUpdated is zero)
//   When:  PruneStaleEdges is called
//   Then:  The edge is NOT evicted by Rule 1 (time-decay)
//   And:   It can still be evicted by Rule 2 (performance) if applicable
func TestCaSKG_PruneStaleEdges_LegacyZeroTimeSpared(t *testing.T) {
	m := NewCaSKGManager("")

	m.mu.Lock()
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  8,
			TotalCount:    10,
			CausalLift:    0.8,
			LastUpdated:   time.Time{}, // zero = legacy edge from old JSON
		},
	}
	m.mu.Unlock()

	m.PruneStaleEdges(1*time.Nanosecond, 0.0) // maxAge tiny, minSuccessRate=0 (spare all)

	m.mu.RLock()
	_, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if !ok {
		t.Error("legacy edge with zero LastUpdated should be spared from time-decay pruning")
	}
}

// TestCaSKG_PruneStaleEdges_EmptySourceRemoved
//
// Scenario: Source nodes with no remaining targets are cleaned up
//   Given: A source node whose only target edge is pruned
//   When:  PruneStaleEdges is called
//   Then:  The empty source entry is removed from the edges map
func TestCaSKG_PruneStaleEdges_EmptySourceRemoved(t *testing.T) {
	m := NewCaSKGManager("")

	m.mu.Lock()
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  1,
			TotalCount:    10,
			CausalLift:    0.1,
			LastUpdated:   time.Now(),
		},
	}
	m.mu.Unlock()

	m.PruneStaleEdges(7*24*time.Hour, 0.3)

	m.mu.RLock()
	_, ok := m.Graph.Edges["SkillA"]
	m.mu.RUnlock()
	if ok {
		t.Error("source node with no remaining targets should be removed")
	}
}

// TestCaSKG_JSONBackwardCompat_LegacyFileWithoutLastUpdated
//
// Scenario: JSON files from before LastUpdated was added can still be loaded
//   Given: A JSON file with edges that lack the "last_updated" field
//   When:  NewCaSKGManager loads the file
//   Then:  The graph is loaded successfully with LastUpdated as zero value
//   And:   The edge data (SuccessCount, TotalCount, CausalLift) is preserved
func TestCaSKG_JSONBackwardCompat_LegacyFileWithoutLastUpdated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy_caskg.json")

	// Legacy JSON without "last_updated" field (pre-metabolic-pruning format).
	legacyJSON := `{
  "nodes": {"SkillA": 10, "SkillB": 10},
  "edges": {
    "SkillA": {
      "SkillB": {
        "source_skill_id": "SkillA",
        "target_skill_id": "SkillB",
        "success_count": 8,
        "total_count": 10,
        "causal_lift": 0.8
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(legacyJSON), 0644); err != nil {
		t.Fatalf("setup: write legacy JSON: %v", err)
	}

	m := NewCaSKGManager(path)

	m.mu.RLock()
	edge, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if !ok {
		t.Fatal("legacy edge should be loaded from JSON")
	}
	if edge.SuccessCount != 8 || edge.TotalCount != 10 {
		t.Errorf("edge data mismatch: SuccessCount=%d TotalCount=%d", edge.SuccessCount, edge.TotalCount)
	}
	if !edge.LastUpdated.IsZero() {
		t.Errorf("legacy edge LastUpdated should be zero, got %v", edge.LastUpdated)
	}
}

// TestCaSKG_Save_TriggersPruning
//
// Scenario: Save() automatically prunes stale edges before persisting (throttled)
//   Given: A graph with a stale edge and a fresh edge, and prune throttle has elapsed
//   When:  Save() is called
//   Then:  The stale edge is evicted and only the fresh edge is persisted
func TestCaSKG_Save_TriggersPruning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "caskg.json")

	m := NewCaSKGManager(path)

	m.mu.Lock()
	// Force prune throttle to fire (lastPruneTime initialized to now in constructor).
	m.lastPruneTime = time.Now().Add(-2 * pruneInterval)
	// Fresh edge — should survive.
	m.Graph.Edges["SkillA"] = map[string]*SkillCausalEdge{
		"SkillB": {
			SourceSkillID: "SkillA",
			TargetSkillID: "SkillB",
			SuccessCount:  8,
			TotalCount:    10,
			CausalLift:    0.8,
			LastUpdated:   time.Now(),
		},
	}
	// Stale edge — should be pruned by Save().
	m.Graph.Edges["SkillC"] = map[string]*SkillCausalEdge{
		"SkillD": {
			SourceSkillID: "SkillC",
			TargetSkillID: "SkillD",
			SuccessCount:  8,
			TotalCount:    10,
			CausalLift:    0.8,
			LastUpdated:   time.Now().Add(-30 * 24 * time.Hour), // 30 days ago
		},
	}
	m.mu.Unlock()

	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify only the fresh edge survived.
	reloaded := NewCaSKGManager(path)
	reloaded.mu.RLock()
	_, freshOK := reloaded.Graph.Edges["SkillA"]["SkillB"]
	_, staleOK := reloaded.Graph.Edges["SkillC"]["SkillD"]
	reloaded.mu.RUnlock()

	if !freshOK {
		t.Error("fresh edge should survive Save() pruning")
	}
	if staleOK {
		t.Error("stale edge (30 days old) should be pruned by Save()")
	}
}

// TestCaSKG_RecordTransition_StampsLastUpdated
//
// Scenario: recordTransition stamps LastUpdated on every edge activation
//   Given: A new edge is created via recordTransition
//   When:  recordTransition is called
//   Then:  edge.LastUpdated is set to a non-zero timestamp close to now
func TestCaSKG_RecordTransition_StampsLastUpdated(t *testing.T) {
	m := NewCaSKGManager("")

	before := time.Now()
	m.mu.Lock()
	m.recordTransition("SkillA", "SkillB", true)
	m.mu.Unlock()

	m.mu.RLock()
	edge, ok := m.Graph.Edges["SkillA"]["SkillB"]
	m.mu.RUnlock()
	if !ok {
		t.Fatal("edge should exist after recordTransition")
	}
	if edge.LastUpdated.IsZero() {
		t.Fatal("LastUpdated should be stamped, got zero")
	}
	if edge.LastUpdated.Before(before) {
		t.Errorf("LastUpdated should be >= before: got %v, before %v", edge.LastUpdated, before)
	}
}
