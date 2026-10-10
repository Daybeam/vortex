package core

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/pkg/search"
)

// MemoryItem is the minimal hot-data unit persisted to disk.
// One item = one ContextNode + its GlobalWorkspace sources + embedding.
type MemoryItem struct {
	TaskID         string     `json:"task_id"`
	NodeID         string     `json:"node_id"`
	ParentID       string     `json:"parent_id,omitempty"`
	Intent         string     `json:"intent"`
	Goal           string     `json:"goal,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	StepIDs        []string   `json:"step_ids"`
	SourceKeys     []string   `json:"source_keys,omitempty"` // GlobalWorkspace keys that fed this node
	Embedding      []float32  `json:"embedding,omitempty"`
	EmbeddingModel string     `json:"embedding_model,omitempty"`
	Timestamp      time.Time  `json:"timestamp"`
	TTL            *time.Time `json:"ttl,omitempty"` // auto-expiry for staged data
	Checksum       string     `json:"checksum,omitempty"`
}

const (
	maxInMemoryItems = 10000
)

// ContextArchive handles appending and reading MemoryItems.
// Append is sync.WriteFile append; read is incremental.
// Zero-CGO, no SQLite dependency.
type ContextArchive struct {
	mu         sync.RWMutex
	path       string // .jsonl file
	items      []MemoryItem
	index      *search.BM25Corpus // lazy-built
	indexMu    sync.RWMutex
	lastOffset int64 // Last read position in file
	indexDirty bool  // True if new items appended but index not rebuilt
	ttlPruned  int   // count of items pruned by TTL since start
}

func NewContextArchive(path string) *ContextArchive {
	return &ContextArchive{path: path}
}

// Append writes one MemoryItem to disk. Called from scheduler.go after step success.
func (a *ContextArchive) Append(item MemoryItem) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if item.Timestamp.IsZero() {
		item.Timestamp = time.Now()
	}

	// JSON Lines: each item = one line
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}

	a.items = append(a.items, item)
	if len(a.items) > maxInMemoryItems {
		a.items = a.items[len(a.items)-maxInMemoryItems:]
	}
	a.indexDirty = true

	// Create directory if not exists
	dir := strings.ReplaceAll(a.path, "\\", "/")
	lastSlash := strings.LastIndex(dir, "/")
	if lastSlash != -1 {
		os.MkdirAll(dir[:lastSlash], 0755)
	}

	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		if stat, statErr := f.Stat(); statErr == nil {
			a.lastOffset = stat.Size()
		}
	}
	return err
}

// Load reads *new* items from disk since last read.
func (a *ContextArchive) Load() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	info, err := os.Stat(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			a.lastOffset = 0
			a.items = nil
			return nil
		}
		return err
	}

	// If file was truncated or replaced, reset offset
	if info.Size() < a.lastOffset {
		a.lastOffset = 0
		a.items = nil
	}

	if info.Size() == a.lastOffset {
		return nil // No new data
	}

	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(a.lastOffset, 0); err != nil {
		return err
	}

	// Read new lines
	decoder := json.NewDecoder(f)
	newCount := 0
	for decoder.More() {
		var item MemoryItem
		if err := decoder.Decode(&item); err != nil {
			break // skip malformed line
		}
		a.items = append(a.items, item)
		newCount++
	}

	// Enforce in-memory limit
	if len(a.items) > maxInMemoryItems {
		a.items = a.items[len(a.items)-maxInMemoryItems:]
	}

	if newCount > 0 {
		a.indexDirty = true
	}
	a.lastOffset, _ = f.Seek(0, 1) // current position

	return nil
}

// BuildIndex lazily constructs the BM25 corpus if dirty.
func (a *ContextArchive) BuildIndex() {
	a.mu.RLock()
	if !a.indexDirty && a.index != nil {
		a.mu.RUnlock()
		return
	}
	items := make([]MemoryItem, len(a.items))
	copy(items, a.items)
	a.mu.RUnlock()

	a.indexMu.Lock()
	defer a.indexMu.Unlock()

	docs := make(map[string]string)
	for _, item := range items {
		// M-7 (2026-10-04): Use strings.Join instead of repeated += allocation.
		parts := make([]string, 0, 3+len(item.SourceKeys))
		parts = append(parts, item.Intent, item.Goal, item.Summary)
		parts = append(parts, item.SourceKeys...)
		docs[item.NodeID] = strings.Join(parts, " ")
	}
	a.index = search.NewBM25Corpus(1.2, 0.75, docs)

	a.mu.Lock()
	a.indexDirty = false
	a.mu.Unlock()
}

// filterItemsByTask returns only items matching the given taskID.
func filterItemsByTask(items []MemoryItem, taskScope string) []MemoryItem {
	out := make([]MemoryItem, 0, len(items))
	for _, it := range items {
		if it.TaskID == taskScope {
			out = append(out, it)
		}
	}
	return out
}

// Sanitize redacts sensitive fields if the item doesn't belong to currentTaskID.
func (it *MemoryItem) Sanitize(currentTaskID string) {
	if it.TaskID != currentTaskID {
		// Clear raw data pointers for other tasks (Experience-only mode)
		it.SourceKeys = nil
		it.StepIDs = nil
		it.Checksum = "[HIDDEN]"
		// Summary can remain as it's typically semantic, but we ensure it's scrubbed
		it.Summary = ScrubSecrets(it.Summary)
		it.Intent = ScrubSecrets(it.Intent)
		it.Goal = ScrubSecrets(it.Goal)
	}
}

// Search runs RRF hybrid search against the archive.
func (a *ContextArchive) Search(query string, embedClient EmbeddingClient, k int, taskScope, currentTaskID string) []MemoryItem {
	// Ensure loaded and indexed (Optimized Lazy Path)
	a.Load()
	a.BuildIndex()

	a.mu.RLock()
	items := make([]MemoryItem, len(a.items))
	copy(items, a.items)
	a.mu.RUnlock()

	// Pre-filter by task scope BEFORE ranking
	if taskScope != "" {
		items = filterItemsByTask(items, taskScope)
	}

	a.indexMu.RLock()
	bm25Rank := map[string]float64{}
	if a.index != nil {
		bm25Rank = a.index.Score(query)
	}
	a.indexMu.RUnlock()

	// Dense rank from cached embeddings — over the (already filtered) item set
	embeds := make(map[string][]float32)
	itemMap := make(map[string]MemoryItem)
	for _, item := range items {
		itemMap[item.NodeID] = item
		if len(item.Embedding) > 0 {
			embeds[item.NodeID] = item.Embedding
		}
	}

	denseRank := map[string]float64{}
	if embedClient != nil && len(embeds) > 0 {
		// audit L-NEW-11: add timeout to prevent blocking forever if embedding service hangs.
		embedCtx, embedCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if qVec, err := embedClient.Embed(embedCtx, query); err == nil {
			denseRank = search.CosineRank(qVec, embeds)
		}
		embedCancel()
	}

	merged := search.RRFMerge(bm25Rank, denseRank)

	type scoredItem struct {
		id    string
		score float64
	}
	var sorted []scoredItem
	for id, score := range merged {
		sorted = append(sorted, scoredItem{id, score})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].score > sorted[j].score })

	results := make([]MemoryItem, 0, k)
	for i := 0; i < k && i < len(sorted); i++ {
		if item, ok := itemMap[sorted[i].id]; ok {
			// Apply Privacy Sanitize (ADDED 2026-09-01)
			item.Sanitize(currentTaskID)
			results = append(results, item)
		}
	}
	return results
}

// --- Prune (TTL 清理) ---

// PruneExpired removes items past their TTL and rewrites the archive file.
// Items with TTL == nil are never expired. Returns number of items pruned and any error.
func (a *ContextArchive) PruneExpired(now time.Time) (pruned int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.items) == 0 {
		return 0, nil
	}

	keep := make([]MemoryItem, 0, len(a.items))
	for _, item := range a.items {
		if item.TTL == nil || now.Before(*item.TTL) {
			keep = append(keep, item)
		}
	}

	pruned = len(a.items) - len(keep)
	if pruned == 0 {
		return 0, nil
	}

	a.items = keep
	if err := a.writeFileLocked(); err != nil {
		return pruned, err
	}
	a.ttlPruned += pruned
	a.indexDirty = true
	return pruned, nil
}

// writeFileLocked rewrites the .jsonl file from a.items and updates lastOffset.
// Caller MUST hold a.mu.
func (a *ContextArchive) writeFileLocked() error {
	var buf strings.Builder
	for _, item := range a.items {
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	content := []byte(buf.String())
	err := os.WriteFile(a.path, content, 0644)
	if err == nil {
		a.lastOffset = int64(len(content))
	}
	return err
}

// --- Forest Search (Kruskal 下沉核心层) ---

// ForestResult bundles the selected MemoryItems with the edges of the Kruskal forest.
type ForestResult struct {
	Nodes []MemoryItem   `json:"nodes"`
	Edges []WeightedEdge `json:"edges"`
}

// SearchForest runs RRF hybrid search then optimizes the top-L seeds into
// a non-redundant forest via BuildSemanticEdges + MaxWeightForest.
// When no embeddings are available, degrades to RRF-only (returns empty forest edges).
func (a *ContextArchive) SearchForest(
	query string,
	embedClient EmbeddingClient,
	k int,
	redundancyThreshold float64,
	taskScope, currentTaskID string,
) (ForestResult, error) {
	if len(query) == 0 {
		return ForestResult{}, nil
	}

	rrf := a.Search(query, embedClient, k*3, taskScope, currentTaskID) // top-L seeds (L ≈ 3K)
	if len(rrf) == 0 {
		return ForestResult{}, nil
	}

	seedIDs := make([]string, 0, len(rrf))
	embeddings := make(map[string][]float32)
	itemMap := make(map[string]MemoryItem)

	for _, item := range rrf {
		seedIDs = append(seedIDs, item.NodeID)
		itemMap[item.NodeID] = item
		if len(item.Embedding) > 0 {
			embeddings[item.NodeID] = item.Embedding
		}
	}

	schemaEdges := make([]WeightedEdge, 0)
	for _, item := range rrf {
		if item.ParentID != "" {
			schemaEdges = append(schemaEdges, WeightedEdge{
				From:     item.ParentID,
				To:       item.NodeID,
				Weight:   1.0,
				EdgeType: "schema",
			})
		}
	}

	semanticEdges := BuildSemanticEdges(embeddings, 8)
	selectedIDs, forestEdges := MaxWeightForest(seedIDs, schemaEdges, semanticEdges, redundancyThreshold, k)

	selected := make([]MemoryItem, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		if item, ok := itemMap[id]; ok {
			selected = append(selected, item)
		}
	}

	return ForestResult{
		Nodes: selected,
		Edges: forestEdges,
	}, nil
}

// ─── ArchiveQuery Methods (P0-3, ADDED 2026-10-10) ────────────────────────
//
// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §4.
// Three query methods at different granularities for the four consumption
// paths (B: Planner Role, C: Step Plan Mode, D: Regular Step Warm layer).

// SearchRecentStepSummaries returns the most recent N step summaries for
// the given task. Used by Path C (Step Plan Mode) — only needs upstream
// dependencies, not full search.
//
// Items are sorted by Timestamp descending (most recent first) and limited
// to N. Only items with non-empty Summary are returned.
func (a *ContextArchive) SearchRecentStepSummaries(taskID string, n int) []MemoryItem {
	if a == nil || taskID == "" || n <= 0 {
		return nil
	}
	a.Load()

	a.mu.RLock()
	items := make([]MemoryItem, len(a.items))
	copy(items, a.items)
	a.mu.RUnlock()

	items = filterItemsByTask(items, taskID)

	// Filter to only items with summaries.
	filtered := make([]MemoryItem, 0, len(items))
	for _, it := range items {
		if it.Summary != "" {
			filtered = append(filtered, it)
		}
	}

	// Sort by timestamp descending (most recent first).
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Timestamp.After(filtered[j].Timestamp)
	})

	if len(filtered) > n {
		filtered = filtered[:n]
	}
	return filtered
}

// SearchAllStepSummaries returns all step summaries for the given task,
// ranked by BM25 relevance to the query. Used by Path B (Planner Role) —
// needs full task history for DAG planning.
func (a *ContextArchive) SearchAllStepSummaries(taskID, query string, n int) []MemoryItem {
	if a == nil || taskID == "" || n <= 0 {
		return nil
	}
	// Delegate to existing Search with task scope.
	return a.Search(query, nil, n, taskID, taskID)
}

// SearchSegments returns sealed segment summaries for the given task scope.
// Used by Path D (Regular Step Warm layer) — provides segment-level overview
// without full text, reducing token cost.
//
// This is a lightweight wrapper that filters the archive by task scope and
// returns items with their Summary field as the segment representation.
// A full SegmentManager integration (Path A) would provide richer segment
// structures; this method provides the archive-based fallback.
func (a *ContextArchive) SearchSegments(taskScope string, n int) []MemoryItem {
	if a == nil || taskScope == "" || n <= 0 {
		return nil
	}
	a.Load()

	a.mu.RLock()
	items := make([]MemoryItem, len(a.items))
	copy(items, a.items)
	a.mu.RUnlock()

	items = filterItemsByTask(items, taskScope)

	// Sort by timestamp descending.
	sort.Slice(items, func(i, j int) bool {
		return items[i].Timestamp.After(items[j].Timestamp)
	})

	if len(items) > n {
		items = items[:n]
	}
	return items
}
