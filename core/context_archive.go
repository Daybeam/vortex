package core

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Fact-level graph edges for 1-hop semantic traversal during recall.
	// See docs/plan/search_adaption_design.md §2.3.
	CausalLinks []string `json:"causal_links,omitempty"` // node IDs of causally related items
	EntityLinks []string `json:"entity_links,omitempty"` // node IDs of entity-co-occurrence items
	// Self-healing §1: SupersededBy state chain. When a new fact overturns an
	// older conclusion, the older item's Status is set to "superseded" and
	// SupersededBy points to the newer item's NodeID. SearchWithOptions filters
	// out superseded/stale items so contradictory facts aren't recalled together.
	Status       string `json:"status,omitempty"`        // "", "active", "superseded", "stale"
	SupersededBy string `json:"superseded_by,omitempty"` // NodeID of the replacing item
}

const (
	maxInMemoryItems = 10000

	// defaultRRFMaxPerSource caps each search arm before RRF fusion,
	// preventing a noisy BM25 arm from flooding the fusion pool.
	// 0 = no cap. See docs/plan/search_adaption_design.md §2.2.
	defaultRRFMaxPerSource = 50
)

// QueryOptions extends Search with optional temporal filtering.
// Zero values mean "no filter" (backward compatible).
// See docs/plan/search_adaption_design.md §2.1.
type QueryOptions struct {
	CreatedAfter  time.Time // only items with Timestamp >= CreatedAfter
	CreatedBefore time.Time // only items with Timestamp <= CreatedBefore
}

// filterByTimeRange removes items outside the [CreatedAfter, CreatedBefore] window.
// Zero-value times are treated as unbounded.
func filterByTimeRange(items []MemoryItem, opts QueryOptions) []MemoryItem {
	if opts.CreatedAfter.IsZero() && opts.CreatedBefore.IsZero() {
		return items
	}
	filtered := make([]MemoryItem, 0, len(items))
	for _, item := range items {
		if !opts.CreatedAfter.IsZero() && item.Timestamp.Before(opts.CreatedAfter) {
			continue
		}
		if !opts.CreatedBefore.IsZero() && item.Timestamp.After(opts.CreatedBefore) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// filterActiveItems removes items whose Status is "superseded" or "stale"
// from the result set (self-healing design §1: SupersededBy state chain).
// Items with empty Status or Status == "active" are kept. This prevents
// contradictory facts from being recalled together when a newer item has
// overturned an older conclusion.
func filterActiveItems(items []MemoryItem) []MemoryItem {
	filtered := make([]MemoryItem, 0, len(items))
	for _, item := range items {
		if item.Status != "superseded" && item.Status != "stale" {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// AddItemEdge records a fact-level edge between two MemoryItems.
// edgeType is "causal" or "entity". The edge is stored on the from-item's
// link slice and is in-memory only (persisted when the item is next Append-ed).
func (a *ContextArchive) AddItemEdge(fromNodeID, toNodeID, edgeType string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.items {
		if a.items[i].NodeID == fromNodeID {
			switch edgeType {
			case "causal":
				a.items[i].CausalLinks = append(a.items[i].CausalLinks, toNodeID)
			case "entity":
				a.items[i].EntityLinks = append(a.items[i].EntityLinks, toNodeID)
			}
			return
		}
	}
}

// expandWithLinkedItems adds 1-hop neighbors from CausalLinks and EntityLinks
// to the result set. Deduplicates by NodeID. Items not found in the archive
// are silently skipped.
func (a *ContextArchive) expandWithLinkedItems(items []MemoryItem) []MemoryItem {
	if len(items) == 0 {
		return items
	}
	// Collect existing node IDs for dedup.
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.NodeID] = true
	}
	// Collect linked node IDs.
	var linkedIDs []string
	for _, item := range items {
		linkedIDs = append(linkedIDs, item.CausalLinks...)
		linkedIDs = append(linkedIDs, item.EntityLinks...)
	}
	if len(linkedIDs) == 0 {
		return items
	}
	// Fetch linked items from archive.
	a.mu.RLock()
	expanded := make([]MemoryItem, 0, len(items)+len(linkedIDs))
	expanded = append(expanded, items...)
	for _, id := range linkedIDs {
		if seen[id] {
			continue
		}
		for _, item := range a.items {
			if item.NodeID == id {
				expanded = append(expanded, item)
				seen[id] = true
				break
			}
		}
	}
	a.mu.RUnlock()
	return expanded
}

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

// GetByNodeIDs returns items matching the given node IDs.
// Used by FoldEngine.Decompress to recover folded content from the archive.
// Returns items in the order they are found in the archive.
func (a *ContextArchive) GetByNodeIDs(nodeIDs []string) []MemoryItem {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if len(nodeIDs) == 0 {
		return nil
	}
	idSet := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		idSet[id] = true
	}
	var result []MemoryItem
	for _, item := range a.items {
		if idSet[item.NodeID] {
			result = append(result, item)
		}
	}
	return result
}

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
//
// GUARD (P3.2, 2026-10-22): task_scope is TASK-level isolation, NOT tenant
// isolation. It answers "which task may recall this memory", never "which
// tenant owns this data" — the codebase has no tenant_id anywhere (verified:
// 0 occurrences; store/ is one process-level SQLite; engine graphs are keyed
// by task only). Do NOT treat this filter as evidence of multi-tenancy. The
// SaaS boundary (per-tenant process isolation + gateway routing) is designed
// in docs/architecture/SAAS_BOUNDARY_DESIGN.md and deliberately NOT built.
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
	return a.SearchWithOptions(query, embedClient, k, taskScope, currentTaskID, QueryOptions{})
}

// SearchWithOptions extends Search with temporal filtering (QueryOptions).
func (a *ContextArchive) SearchWithOptions(query string, embedClient EmbeddingClient, k int, taskScope, currentTaskID string, opts QueryOptions) []MemoryItem {
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

	// Pre-filter by temporal window BEFORE ranking
	items = filterByTimeRange(items, opts)

	// Pre-filter by status: skip superseded/stale items (self-healing §1).
	items = filterActiveItems(items)

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

	merged := search.RRFMergeCapped(defaultRRFMaxPerSource, bm25Rank, denseRank)

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

// --- Prune (TTL cleanup) ---

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

// ArchiveScope specifies which archive entries to delete.
// At least one field must be non-empty. Empty fields are treated as
// "no filter on this dimension".
//
// See docs/plan/SESSION_MEMORY_DELETION_DESIGN.md.
type ArchiveScope struct {
	TaskID string // if non-empty, delete entries with this TaskID
}

// matchesScope returns true if the item matches at least one non-empty
// scope field (i.e. should be deleted).
func (s ArchiveScope) matchesScope(item MemoryItem) bool {
	if s.TaskID != "" && item.TaskID == s.TaskID {
		return true
	}
	return false
}

// DeleteByScope removes archive entries matching the scope filter, rewrites
// the .jsonl file, and marks the BM25 index for rebuild.
// Returns the count of items deleted.
//
// ContextArchive is JSONL-based (not SQLite), so deletion is implemented as
// filter-in-memory + rewrite-file — the same pattern as PruneExpired.
// See docs/plan/SESSION_MEMORY_DELETION_DESIGN.md §Storage Layer Changes.
func (a *ContextArchive) DeleteByScope(scope ArchiveScope) (deleted int, err error) {
	if scope.TaskID == "" {
		return 0, fmt.Errorf("DeleteByScope: at least one scope field must be non-empty")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.items) == 0 {
		return 0, nil
	}

	keep := make([]MemoryItem, 0, len(a.items))
	for _, item := range a.items {
		if scope.matchesScope(item) {
			deleted++
			continue
		}
		keep = append(keep, item)
	}

	if deleted == 0 {
		return 0, nil
	}

	a.items = keep
	if err := a.writeFileLocked(); err != nil {
		return deleted, err
	}
	a.indexDirty = true
	return deleted, nil
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

// MarkStaleByStep marks all MemoryItems matching the given taskID and any of
// stepIDs as "stale" (self-healing §3: CycleBreaker stale memory patch hook).
// Called when the CycleBreaker reaches its fuse limit — the failing step's
// contributing memory is presumed unreliable and should not be recalled in
// future searches. Returns the count of items marked stale.
func (a *ContextArchive) MarkStaleByStep(taskID string, stepIDs []string) int {
	if taskID == "" || len(stepIDs) == 0 {
		return 0
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	stepSet := make(map[string]bool, len(stepIDs))
	for _, sid := range stepIDs {
		stepSet[sid] = true
	}

	marked := 0
	for i := range a.items {
		if a.items[i].TaskID != taskID {
			continue
		}
		for _, sid := range a.items[i].StepIDs {
			if stepSet[sid] && a.items[i].Status != "stale" {
				a.items[i].Status = "stale"
				marked++
				break
			}
		}
	}

	if marked > 0 {
		_ = a.writeFileLocked()
		a.indexDirty = true
	}
	return marked
}

// --- Forest Search (Kruskal sink core layer) ---

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
	return a.SearchForestWithOptions(query, embedClient, k, redundancyThreshold, taskScope, currentTaskID, QueryOptions{})
}

// SearchForestWithOptions extends SearchForest with temporal filtering.
func (a *ContextArchive) SearchForestWithOptions(
	query string,
	embedClient EmbeddingClient,
	k int,
	redundancyThreshold float64,
	taskScope, currentTaskID string,
	opts QueryOptions,
) (ForestResult, error) {
	if len(query) == 0 {
		return ForestResult{}, nil
	}

	rrf := a.SearchWithOptions(query, embedClient, k*3, taskScope, currentTaskID, opts) // top-L seeds (L ≈ 3K)
	if len(rrf) == 0 {
		return ForestResult{}, nil
	}

	// Expand with 1-hop neighbors from fact-level graph edges (causal/entity).
	rrf = a.expandWithLinkedItems(rrf)

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
// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §4.
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
