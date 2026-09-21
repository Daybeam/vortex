package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

func TestContextArchive(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "context.jsonl")
	archive := NewContextArchive(archivePath)

	t.Run("Append and Load", func(t *testing.T) {
		item := MemoryItem{
			TaskID:    "task1",
			NodeID:    "node1",
			Intent:    "test intent",
			Summary:   "test summary",
			Timestamp: time.Now(),
		}
		if err := archive.Append(item); err != nil {
			t.Fatalf("failed to append: %v", err)
		}

		if err := archive.Load(); err != nil {
			t.Fatalf("failed to load: %v", err)
		}
		if len(archive.items) != 1 {
			t.Errorf("expected 1 item, got %d", len(archive.items))
		}
		if archive.items[0].NodeID != "node1" {
			t.Errorf("expected node1, got %s", archive.items[0].NodeID)
		}
	})

	t.Run("Search RRF", func(t *testing.T) {
		archive.Append(MemoryItem{
			TaskID:  "task1",
			NodeID:  "node2",
			Intent:  "another intent",
			Summary: "searchable content",
		})
		archive.BuildIndex()

		results := archive.Search("searchable", nil, 10, "", "")
		if len(results) == 0 {
			t.Errorf("expected search results for 'searchable'")
		}
		found := false
		for _, r := range results {
			if r.NodeID == "node2" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("node2 not found in search results")
		}
	})

	t.Run("Search TaskScope PreFilter", func(t *testing.T) {
		// two tasks, both contain "alpha"
		archive.Append(MemoryItem{
			TaskID:  "taskA",
			NodeID:  "A1",
			Intent:  "alpha in task A",
			Summary: "high value A",
		})
		archive.Append(MemoryItem{
			TaskID:  "taskB",
			NodeID:  "B1",
			Intent:  "alpha in task B",
			Summary: "high value B",
		})
		archive.BuildIndex()

		results := archive.Search("alpha", nil, 10, "taskA", "")
		if len(results) == 0 {
			t.Fatal("expected search results for taskA")
		}
		for _, r := range results {
			if r.TaskID != "taskA" {
				t.Errorf("taskScope pre-filter failed: got task %s, want taskA", r.TaskID)
			}
		}
		if len(results) != 1 {
			t.Errorf("expected 1 result from taskA, got %d", len(results))
		}
	})

	t.Run("PruneExpired", func(t *testing.T) {
		now := time.Now()
		expired := time.Now().Add(-24 * time.Hour)
		notExpired := time.Now().Add(24 * time.Hour)
		nilTTL := time.Time{} // marker

		archive.Load() // reset items

		// 3 expired items
		for i := 0; i < 3; i++ {
			archive.Append(MemoryItem{
				TaskID:    "task1",
				NodeID:    "exp_" + string(rune('0'+i)),
				Intent:    "expired",
				Timestamp: now,
				TTL:       &expired,
			})
		}
		// 1 not-expired
		archive.Append(MemoryItem{
			TaskID:    "task1",
			NodeID:    "keep1",
			Intent:    "active",
			Timestamp: now,
			TTL:       &notExpired,
		})
		// 1 nil TTL (never expires)
		archive.Append(MemoryItem{
			TaskID:    "task1",
			NodeID:    "keep2",
			Intent:    "permanent",
			Timestamp: now,
			TTL:       nil,
		})

		_ = nilTTL // silence unused
		pruned, err := archive.PruneExpired(now)
		if err != nil {
			t.Fatalf("PruneExpired error: %v", err)
		}
		if pruned != 3 {
			t.Errorf("expected 3 pruned, got %d", pruned)
		}
		// Reload and verify only non-expired survive
		archive.items = nil    // clear in-memory
		archive.lastOffset = 0 // force full reload
		if err := archive.Load(); err != nil {
			t.Fatalf("load after prune error: %v", err)
		}
		remaining := make(map[string]bool)
		for _, it := range archive.items {
			remaining[it.NodeID] = true
		}
		if _, ok := remaining["exp_0"]; ok {
			t.Error("expired item should be pruned")
		}
		if _, ok := remaining["keep1"]; !ok {
			t.Error("keep1 should survive")
		}
		if _, ok := remaining["keep2"]; !ok {
			t.Error("keep2 (nil TTL) should survive")
		}
	})

	t.Run("Empty Archive Search and Prune", func(t *testing.T) {
		archive.items = nil
		results := archive.Search("anything", nil, 10, "", "")
		if len(results) != 0 {
			t.Errorf("expected empty results, got %d", len(results))
		}
		pruned, err := archive.PruneExpired(time.Now())
		if err != nil {
			t.Fatalf("prune empty archive error: %v", err)
		}
		if pruned != 0 {
			t.Errorf("expected 0 pruned on empty archive, got %d", pruned)
		}
	})

	t.Run("Incremental Load and Index Logic", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "inc.jsonl")
		archive := NewContextArchive(path)

		// 1. Initial append and load
		archive.Append(MemoryItem{NodeID: "n1", Intent: "one"})
		if err := archive.Load(); err != nil {
			t.Fatal(err)
		}
		if len(archive.items) != 1 {
			t.Errorf("expected 1 item, got %d", len(archive.items))
		}
		offset1 := archive.lastOffset
		if offset1 == 0 {
			t.Error("offset should be non-zero after load")
		}

		// 2. Second append and incremental load
		archive.Append(MemoryItem{NodeID: "n2", Intent: "two"})
		if err := archive.Load(); err != nil {
			t.Fatal(err)
		}
		if len(archive.items) != 2 {
			t.Errorf("expected 2 items, got %d", len(archive.items))
		}
		if archive.lastOffset <= offset1 {
			t.Errorf("offset should have increased, got %d -> %d", offset1, archive.lastOffset)
		}

		// 3. Verify index dirty flag logic
		archive.BuildIndex()
		if archive.indexDirty {
			t.Error("index should not be dirty after build")
		}
		archive.Append(MemoryItem{NodeID: "n3", Intent: "three"})
		if !archive.indexDirty {
			t.Error("index should be dirty after append")
		}
	})
}

func TestArchiveStepIngestion(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "context.jsonl")
	archive := NewContextArchive(archivePath)

	s := &DirectedEngine{
		Archive: archive,
		logger:  &Logger{},
	}

	graph := &schemas.TaskGraph{
		TaskID: "task-ingest-1",
	}
	step := &schemas.Step{
		ID:   "step-1",
		Task: "Analyze the data",
	}
	output := schemas.SubagentOutput{
		Result: map[string]any{
			"summary": "Analysis complete with 3 findings",
		},
	}

	s.archiveStep(graph, step, output)

	if err := archive.Load(); err != nil {
		t.Fatalf("load error: %v", err)
	}
	if len(archive.items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(archive.items))
	}
	item := archive.items[0]
	if item.TaskID != "task-ingest-1" {
		t.Errorf("TaskID = %s, want task-ingest-1", item.TaskID)
	}
	if item.NodeID != "step-1" {
		t.Errorf("NodeID = %s, want step-1", item.NodeID)
	}
	if item.Intent != "Analyze the data" {
		t.Errorf("Intent = %s, want 'Analyze the data'", item.Intent)
	}
	if item.Summary != "Analysis complete with 3 findings" {
		t.Errorf("Summary = %s, want 'Analysis complete with 3 findings'", item.Summary)
	}
}

func TestSingleTaskScopeEnforcement(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "context.jsonl")
	archive := NewContextArchive(archivePath)

	archive.Append(MemoryItem{TaskID: "taskA", NodeID: "A1", Intent: "alpha", Summary: "A content"})
	archive.Append(MemoryItem{TaskID: "taskB", NodeID: "B1", Intent: "alpha", Summary: "B content"})
	archive.BuildIndex()

	resultsA := archive.Search("alpha", nil, 10, "taskA", "taskA")
	if len(resultsA) != 1 || resultsA[0].TaskID != "taskA" {
		t.Errorf("taskA scope: expected 1 result from taskA, got %v", resultsA)
	}

	resultsB := archive.Search("alpha", nil, 10, "taskB", "taskB")
	if len(resultsB) != 1 || resultsB[0].TaskID != "taskB" {
		t.Errorf("taskB scope: expected 1 result from taskB, got %v", resultsB)
	}

	resultsAll := archive.Search("alpha", nil, 10, "", "")
	if len(resultsAll) != 2 {
		t.Errorf("empty scope: expected 2 results (all tasks), got %d — handler must reject empty task_scope", len(resultsAll))
	}
}
