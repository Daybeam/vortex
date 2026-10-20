package core

import (
	"path/filepath"
	"testing"
	"time"
)

// --- Phase 2: Temporal Filtering ---

func TestFilterByTimeRange_NoFilter(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "a", Timestamp: time.Now().Add(-2 * time.Hour)},
		{NodeID: "b", Timestamp: time.Now().Add(-1 * time.Hour)},
		{NodeID: "c", Timestamp: time.Now()},
	}
	result := filterByTimeRange(items, QueryOptions{})
	if len(result) != 3 {
		t.Fatalf("no filter: expected 3 items, got %d", len(result))
	}
}

func TestFilterByTimeRange_CreatedAfter(t *testing.T) {
	now := time.Now()
	items := []MemoryItem{
		{NodeID: "old", Timestamp: now.Add(-2 * time.Hour)},
		{NodeID: "mid", Timestamp: now.Add(-30 * time.Minute)},
		{NodeID: "new", Timestamp: now},
	}
	result := filterByTimeRange(items, QueryOptions{CreatedAfter: now.Add(-1 * time.Hour)})
	if len(result) != 2 {
		t.Fatalf("CreatedAfter: expected 2 items, got %d", len(result))
	}
	if result[0].NodeID != "mid" || result[1].NodeID != "new" {
		t.Fatalf("CreatedAfter: expected [mid, new], got [%s, %s]", result[0].NodeID, result[1].NodeID)
	}
}

func TestFilterByTimeRange_CreatedBefore(t *testing.T) {
	now := time.Now()
	items := []MemoryItem{
		{NodeID: "old", Timestamp: now.Add(-2 * time.Hour)},
		{NodeID: "mid", Timestamp: now.Add(-30 * time.Minute)},
		{NodeID: "new", Timestamp: now},
	}
	result := filterByTimeRange(items, QueryOptions{CreatedBefore: now.Add(-1 * time.Hour)})
	if len(result) != 1 {
		t.Fatalf("CreatedBefore: expected 1 item, got %d", len(result))
	}
	if result[0].NodeID != "old" {
		t.Fatalf("CreatedBefore: expected [old], got [%s]", result[0].NodeID)
	}
}

func TestFilterByTimeRange_Window(t *testing.T) {
	now := time.Now()
	items := []MemoryItem{
		{NodeID: "old", Timestamp: now.Add(-3 * time.Hour)},
		{NodeID: "in-window", Timestamp: now.Add(-90 * time.Minute)},
		{NodeID: "new", Timestamp: now},
	}
	result := filterByTimeRange(items, QueryOptions{
		CreatedAfter:  now.Add(-2 * time.Hour),
		CreatedBefore: now.Add(-1 * time.Hour),
	})
	if len(result) != 1 {
		t.Fatalf("window: expected 1 item, got %d", len(result))
	}
	if result[0].NodeID != "in-window" {
		t.Fatalf("window: expected [in-window], got [%s]", result[0].NodeID)
	}
}

func TestSearchWithOptions_TemporalFilter(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	now := time.Now()
	// Append items with different timestamps.
	items := []MemoryItem{
		{NodeID: "old", TaskID: "t1", Intent: "old intent", Summary: "old summary", Timestamp: now.Add(-3 * time.Hour)},
		{NodeID: "recent", TaskID: "t1", Intent: "recent intent", Summary: "recent summary", Timestamp: now.Add(-30 * time.Minute)},
		{NodeID: "newest", TaskID: "t1", Intent: "newest intent", Summary: "newest summary", Timestamp: now},
	}
	for _, item := range items {
		if err := archive.Append(item); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	archive.BuildIndex()

	// Search with CreatedAfter filter — should only return "recent" and "newest".
	results := archive.SearchWithOptions("intent", nil, 10, "", "", QueryOptions{
		CreatedAfter: now.Add(-1 * time.Hour),
	})
	nodeIDs := make(map[string]bool)
	for _, r := range results {
		nodeIDs[r.NodeID] = true
	}
	if nodeIDs["old"] {
		t.Error("old item should be filtered out by CreatedAfter")
	}
	if !nodeIDs["recent"] {
		t.Error("recent item should be present")
	}
	if !nodeIDs["newest"] {
		t.Error("newest item should be present")
	}
}

// --- Phase 2b: Status Filtering (Self-Healing §1) ---

func TestFilterActiveItems_KeepsAllWhenNoStatus(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "a", Timestamp: time.Now()},
		{NodeID: "b", Timestamp: time.Now()},
		{NodeID: "c", Timestamp: time.Now()},
	}
	result := filterActiveItems(items)
	if len(result) != 3 {
		t.Fatalf("no status: expected 3 items, got %d", len(result))
	}
}

func TestFilterActiveItems_FiltersSuperseded(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "active1", Timestamp: time.Now()},
		{NodeID: "superseded1", Timestamp: time.Now(), Status: "superseded", SupersededBy: "active1"},
		{NodeID: "active2", Timestamp: time.Now(), Status: "active"},
	}
	result := filterActiveItems(items)
	if len(result) != 2 {
		t.Fatalf("superseded: expected 2 items, got %d", len(result))
	}
	got := make(map[string]bool)
	for _, r := range result {
		got[r.NodeID] = true
	}
	if !got["active1"] || !got["active2"] {
		t.Fatalf("expected [active1, active2], got %v", got)
	}
	if got["superseded1"] {
		t.Fatal("superseded1 should be filtered out")
	}
}

func TestFilterActiveItems_FiltersStale(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "fresh", Timestamp: time.Now()},
		{NodeID: "stale1", Timestamp: time.Now(), Status: "stale"},
	}
	result := filterActiveItems(items)
	if len(result) != 1 {
		t.Fatalf("stale: expected 1 item, got %d", len(result))
	}
	if result[0].NodeID != "fresh" {
		t.Fatalf("expected [fresh], got [%s]", result[0].NodeID)
	}
}

func TestFilterActiveItems_KeepsActive(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "a", Timestamp: time.Now(), Status: "active"},
		{NodeID: "b", Timestamp: time.Now(), Status: "active"},
	}
	result := filterActiveItems(items)
	if len(result) != 2 {
		t.Fatalf("active: expected 2 items, got %d", len(result))
	}
}

func TestSearchWithOptions_StatusFilter(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	items := []MemoryItem{
		{NodeID: "current", TaskID: "t1", Intent: "api contract", Summary: "v2 endpoint", Timestamp: time.Now(), Status: "active"},
		{NodeID: "outdated", TaskID: "t1", Intent: "api contract", Summary: "v1 endpoint", Timestamp: time.Now(), Status: "superseded", SupersededBy: "current"},
	}
	for _, item := range items {
		if err := archive.Append(item); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	archive.BuildIndex()

	results := archive.SearchWithOptions("api contract", nil, 10, "", "", QueryOptions{})
	nodeIDs := make(map[string]bool)
	for _, r := range results {
		nodeIDs[r.NodeID] = true
	}
	if !nodeIDs["current"] {
		t.Error("current (active) item should be present")
	}
	if nodeIDs["outdated"] {
		t.Error("outdated (superseded) item should be filtered out")
	}
}

// --- Phase 3: Fact-Level Memory Graph ---

func TestAddItemEdge_CausalLink(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	archive.Append(MemoryItem{NodeID: "node-a", TaskID: "t1", Intent: "a", Timestamp: time.Now()})
	archive.Append(MemoryItem{NodeID: "node-b", TaskID: "t1", Intent: "b", Timestamp: time.Now()})
	archive.Load()

	archive.AddItemEdge("node-a", "node-b", "causal")

	archive.mu.RLock()
	links := archive.items[0].CausalLinks
	archive.mu.RUnlock()

	if len(links) != 1 || links[0] != "node-b" {
		t.Fatalf("expected CausalLinks=[node-b], got %v", links)
	}
}

func TestAddItemEdge_EntityLink(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	archive.Append(MemoryItem{NodeID: "node-a", TaskID: "t1", Intent: "a", Timestamp: time.Now()})
	archive.Append(MemoryItem{NodeID: "node-c", TaskID: "t1", Intent: "c", Timestamp: time.Now()})
	archive.Load()

	archive.AddItemEdge("node-a", "node-c", "entity")

	archive.mu.RLock()
	links := archive.items[0].EntityLinks
	archive.mu.RUnlock()

	if len(links) != 1 || links[0] != "node-c" {
		t.Fatalf("expected EntityLinks=[node-c], got %v", links)
	}
}

func TestAddItemEdge_NonexistentFromNode(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	archive.Append(MemoryItem{NodeID: "node-a", TaskID: "t1", Intent: "a", Timestamp: time.Now()})
	archive.Load()

	// Edge from nonexistent node → no-op, no panic.
	archive.AddItemEdge("nonexistent", "node-a", "causal")

	archive.mu.RLock()
	links := archive.items[0].CausalLinks
	archive.mu.RUnlock()

	if len(links) != 0 {
		t.Fatalf("expected no causal links, got %v", links)
	}
}

func TestExpandWithLinkedItems_NoLinks(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	items := []MemoryItem{
		{NodeID: "a", TaskID: "t1", Intent: "a", Timestamp: time.Now()},
		{NodeID: "b", TaskID: "t1", Intent: "b", Timestamp: time.Now()},
	}
	for _, item := range items {
		archive.Append(item)
	}
	archive.Load()

	// No links → should return same items.
	result := archive.expandWithLinkedItems(items)
	if len(result) != 2 {
		t.Fatalf("no links: expected 2 items, got %d", len(result))
	}
}

func TestExpandWithLinkedItems_Adds1HopNeighbors(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	// Create items: a → b (causal), a → c (entity)
	items := []MemoryItem{
		{NodeID: "a", TaskID: "t1", Intent: "a", Timestamp: time.Now(), CausalLinks: []string{"b"}},
		{NodeID: "b", TaskID: "t1", Intent: "b", Timestamp: time.Now()},
		{NodeID: "c", TaskID: "t1", Intent: "c", Timestamp: time.Now()},
	}
	for _, item := range items {
		archive.Append(item)
	}
	archive.Load()

	// Also add entity link from a to c.
	archive.AddItemEdge("a", "c", "entity")

	// Start with only item "a" — expansion should add "b" (causal) and "c" (entity).
	seed := []MemoryItem{items[0]}
	// Reload to get the updated links.
	archive.mu.RLock()
	seed[0] = archive.items[0]
	archive.mu.RUnlock()

	result := archive.expandWithLinkedItems(seed)
	if len(result) != 3 {
		t.Fatalf("expected 3 items (a + b + c), got %d", len(result))
	}

	nodeIDs := make(map[string]bool)
	for _, r := range result {
		nodeIDs[r.NodeID] = true
	}
	for _, expected := range []string{"a", "b", "c"} {
		if !nodeIDs[expected] {
			t.Errorf("expected %s in results", expected)
		}
	}
}

func TestExpandWithLinkedItems_Deduplicates(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	items := []MemoryItem{
		{NodeID: "a", TaskID: "t1", Intent: "a", Timestamp: time.Now(), CausalLinks: []string{"b"}},
		{NodeID: "b", TaskID: "t1", Intent: "b", Timestamp: time.Now()},
	}
	for _, item := range items {
		archive.Append(item)
	}
	archive.Load()

	// Start with both "a" and "b" — "a" links to "b" but "b" is already present.
	seed := []MemoryItem{items[0], items[1]}
	result := archive.expandWithLinkedItems(seed)
	if len(result) != 2 {
		t.Fatalf("dedup: expected 2 items (a + b, no duplicate), got %d", len(result))
	}
}

func TestExpandWithLinkedItems_EmptyInput(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "context.jsonl"))

	result := archive.expandWithLinkedItems(nil)
	if len(result) != 0 {
		t.Fatalf("empty input: expected 0 items, got %d", len(result))
	}
}
