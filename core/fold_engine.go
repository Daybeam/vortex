package core

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// ─── Segment-Based Incremental Fold ───────────────────────────────────────
//
// Design ref: docs/plan/SEGMENT_BASED_INCREMENTAL_FOLD_DESIGN.md
//
// FoldEngine performs incremental, reversible compression of chat context by
// folding sealed SegmentManager segments into summaries. Unlike
// PromptBudgetEnforcer.hardTruncate (which is irreversible and token-count-
// based), the fold engine:
//
//   - Compresses at semantic boundaries (sealed segments, not arbitrary offsets)
//   - Is incremental (tracks foldBaseline, only folds new sealed segments)
//   - Is reversible (decompress recovers original content from ContextArchive)
//   - Is harness-triggered (runs before EnforceBudget, not model-initiated)
//
// FoldEngine is per-task-scoped, matching SegmentManager's scoping model.
// It is separate from PromptBudgetEnforcer to preserve the enforcer's
// stateless design philosophy.

const (
	// tier2Trigger is the threshold at which tier-1 fold blocks are merged
	// into a tier-2 block (LSM-style compaction).
	tier2Trigger = 10

	// foldSummaryMaxChars limits each fold block's summary text length.
	foldSummaryMaxChars = 500
)

// FoldBlock represents a folded sealed segment.
// Original content lives in ContextArchive; FoldBlock only records which
// ItemIDs (NodeIDs) were folded, so decompress can recover them.
type FoldBlock struct {
	BlockID    string    `json:"block_id"`    // "fold_b1", "fold_b2", ...
	SegmentID  string    `json:"segment_id"`  // source segment ID (empty for tier-2)
	TopicTitle string    `json:"topic_title"` // segment topic title
	Summary    string    `json:"summary"`     // segment summary (LLM or fallback)
	ItemIDs    []string  `json:"item_ids"`    // folded NodeID list
	ItemCount  int       `json:"item_count"`
	Tier       int       `json:"tier"` // 1 = original fold, 2 = merged fold
	CreatedAt  time.Time `json:"created_at"`
}

// FoldState tracks the fold state for one taskScope.
type FoldState struct {
	Blocks       []*FoldBlock `json:"blocks"`
	FoldBaseline int          `json:"fold_baseline"` // sealed segment count at last fold
}

// FoldEngine manages incremental fold compression for chat context.
// It is thread-safe and per-task-scoped.
type FoldEngine struct {
	mu         sync.Mutex
	states     map[string]*FoldState // taskScope -> fold state
	segMgr     *SegmentManager
	archive    *ContextArchive
	summarizer func(*MemorySegment) string
	logger     *Logger
}

// NewFoldEngine creates a FoldEngine. segMgr and archive may be nil —
// the engine will be a no-op in that case.
func NewFoldEngine(segMgr *SegmentManager, archive *ContextArchive, logger *Logger) *FoldEngine {
	return &FoldEngine{
		states:  make(map[string]*FoldState),
		segMgr:  segMgr,
		archive: archive,
		logger:  logger,
	}
}

// SetSummarizer sets a custom LLM-based summarizer for fold blocks.
// If not set, a fallback summary (topic + item count) is used.
func (f *FoldEngine) SetSummarizer(fn func(*MemorySegment) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summarizer = fn
}

// FoldSealedSegments folds new sealed segments into summaries and inserts
// a fold summary block into the prompt blocks. It should be called BEFORE
// PromptBudgetEnforcer.EnforceBudget.
//
// If the total estimated tokens are within budget, blocks are returned
// unchanged (no folding needed). Otherwise, sealed segments since
// foldBaseline are folded into FoldBlocks and a summary block is inserted.
func (f *FoldEngine) FoldSealedSegments(
	blocks []schemas.ContentBlock,
	taskScope string,
	budgetTokens int,
) []schemas.ContentBlock {
	if f == nil || f.segMgr == nil {
		return blocks
	}

	f.mu.Lock()
	state := f.states[taskScope]
	if state == nil {
		state = &FoldState{}
		f.states[taskScope] = state
	}
	f.mu.Unlock()

	sealedCount := f.segMgr.SealedCount()
	if sealedCount <= state.FoldBaseline {
		return blocks // no new sealed segments to fold
	}

	// Estimate current total tokens.
	totalTokens := estimateFoldTokens(blocks)
	if totalTokens <= budgetTokens {
		return blocks // within budget, no fold needed
	}

	// Get sealed segments since baseline.
	newSealed := f.segMgr.GetSealedSince(state.FoldBaseline)
	if len(newSealed) == 0 {
		return blocks
	}

	// Fold from oldest new sealed segment until within budget.
	for i := range newSealed {
		seg := &newSealed[i]
		summary := f.generateSummary(seg)

		block := &FoldBlock{
			BlockID:    fmt.Sprintf("fold_b%d", len(state.Blocks)+1),
			SegmentID:  seg.SegmentID,
			TopicTitle: seg.TopicTitle,
			Summary:    truncateStr(summary, foldSummaryMaxChars),
			ItemIDs:    seg.ItemIDs,
			ItemCount:  seg.ItemCount,
			Tier:       1,
			CreatedAt:  time.Now(),
		}
		state.Blocks = append(state.Blocks, block)

		// Check if we're now within budget after folding this segment.
		// The fold summary is much smaller than the original messages,
		// so each fold reduces the total.
		foldText := f.buildFoldSummaryBlock(state.Blocks, taskScope)
		if len(foldText.Text)/charsPerToken+estimateFoldTokens(blocks) <= budgetTokens {
			break
		}
	}

	state.FoldBaseline = sealedCount

	// Tier-2 merge if too many tier-1 blocks.
	if len(state.Blocks) > tier2Trigger {
		f.foldTier2Merge(state, taskScope)
	}

	// Insert fold summary block into the prompt blocks.
	foldBlock := f.buildFoldSummaryBlock(state.Blocks, taskScope)
	if foldBlock.Text != "" {
		blocks = insertFoldBlock(blocks, foldBlock)
	}

	if f.logger != nil {
		f.logger.Log(EventCacheInefficiency, "", "", map[string]any{
			"event":         "fold_segments",
			"task_scope":    taskScope,
			"folded_blocks": len(state.Blocks),
			"baseline":      state.FoldBaseline,
		})
	}

	return blocks
}

// foldTier2Merge merges the oldest tier-1 blocks into a single tier-2 block.
// This is analogous to LSM-tree compaction: when tier-1 blocks accumulate
// beyond tier2Trigger, the oldest ones are merged to reduce block count.
func (f *FoldEngine) foldTier2Merge(state *FoldState, taskScope string) {
	var tier1, tier2 []*FoldBlock
	for _, b := range state.Blocks {
		if b.Tier == 1 {
			tier1 = append(tier1, b)
		} else {
			tier2 = append(tier2, b)
		}
	}

	if len(tier1) < tier2Trigger {
		return
	}

	// Sort tier-1 by CreatedAt (oldest first).
	sort.Slice(tier1, func(i, j int) bool {
		return tier1[i].CreatedAt.Before(tier1[j].CreatedAt)
	})

	// Merge oldest tier-1 blocks (up to tier2Trigger).
	toMerge := tier1[:tier2Trigger]
	remaining := tier1[tier2Trigger:]

	var titles, summaries []string
	var allItemIDs []string
	seen := make(map[string]bool)

	for _, b := range toMerge {
		titles = append(titles, b.TopicTitle)
		summaries = append(summaries, b.Summary)
		for _, id := range b.ItemIDs {
			if !seen[id] {
				allItemIDs = append(allItemIDs, id)
				seen[id] = true
			}
		}
	}

	merged := &FoldBlock{
		BlockID:    fmt.Sprintf("fold_t2_%d", time.Now().UnixNano()),
		SegmentID:  "", // tier-2 spans multiple segments
		TopicTitle: fmt.Sprintf("跨段摘要: %s", strings.Join(titles, "; ")),
		Summary:    truncateStr(strings.Join(summaries, "\n---\n"), foldSummaryMaxChars),
		ItemIDs:    allItemIDs,
		ItemCount:  len(allItemIDs),
		Tier:       2,
		CreatedAt:  time.Now(),
	}

	// Rebuild: tier-2 blocks + merged block + remaining tier-1.
	state.Blocks = append(tier2, merged)
	state.Blocks = append(state.Blocks, remaining...)

	if f.logger != nil {
		f.logger.Log(EventCacheInefficiency, "", "", map[string]any{
			"event":           "fold_tier2_merge",
			"task_scope":      taskScope,
			"merged_count":    len(toMerge),
			"remaining_tier1": len(remaining),
		})
	}
}

// Decompress recovers original content for a folded segment.
// It looks up the FoldBlock by segmentID, then fetches the original
// MemoryItems from ContextArchive by their NodeIDs.
//
// Returns a formatted string containing the original user intents and
// assistant summaries, plus cross-segment links if available.
func (f *FoldEngine) Decompress(segmentID, taskScope string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("fold engine is nil")
	}
	if f.archive == nil {
		return "", fmt.Errorf("archive not available")
	}

	f.mu.Lock()
	state := f.states[taskScope]
	f.mu.Unlock()

	if state == nil {
		return "", fmt.Errorf("no fold state for task scope %q", taskScope)
	}

	// Find the FoldBlock for this segment.
	var block *FoldBlock
	for _, b := range state.Blocks {
		if b.SegmentID == segmentID {
			block = b
			break
		}
	}
	if block == nil {
		return "", fmt.Errorf("segment %q not found in fold state", segmentID)
	}

	// Recover original items from archive.
	items := f.archive.GetByNodeIDs(block.ItemIDs)

	var result strings.Builder
	result.WriteString(fmt.Sprintf("# Segment: %s\n\n", block.TopicTitle))

	for _, item := range items {
		result.WriteString(fmt.Sprintf("## User\n%s\n\n## Assistant\n%s\n\n---\n\n",
			item.Intent, item.Summary))
	}

	// Append cross-segment trace (Trace Weaver) if available.
	if f.segMgr != nil {
		trace := f.segMgr.FollowTrace(segmentID, 0) // 0 = default depth
		if len(trace.Segments) > 1 {
			woven := f.segMgr.WeaveTrace(trace)
			if woven != "" {
				result.WriteString("\n**Topic trace:**\n" + woven)
			}
		}
	}

	return result.String(), nil
}

// GetFoldBlocks returns all fold blocks for a task scope.
// Used by PromptAssembler to build the fold summary section.
func (f *FoldEngine) GetFoldBlocks(taskScope string) []*FoldBlock {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.states[taskScope]
	if state == nil {
		return nil
	}
	return state.Blocks
}

// GetFoldBaseline returns the fold baseline for a task scope.
// Used by tests and diagnostics.
func (f *FoldEngine) GetFoldBaseline(taskScope string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.states[taskScope]
	if state == nil {
		return 0
	}
	return state.FoldBaseline
}

// ─── Helpers ─────────────────────────────────────────────────────────────

// generateSummary produces a summary for a segment using the injected
// summarizer or a fallback (topic title + item count).
func (f *FoldEngine) generateSummary(seg *MemorySegment) string {
	if f.summarizer != nil {
		return f.summarizer(seg)
	}
	return fmt.Sprintf("[%s] %d items folded. Topic: %s",
		seg.SegmentID, seg.ItemCount, seg.TopicTitle)
}

// buildFoldSummaryBlock creates a ContentBlock containing all fold block
// summaries. This block is inserted into the system prompt to give the
// model context about folded conversation segments.
func (f *FoldEngine) buildFoldSummaryBlock(blocks []*FoldBlock, taskScope string) schemas.ContentBlock {
	if len(blocks) == 0 {
		return schemas.ContentBlock{}
	}

	var buf strings.Builder
	buf.WriteString("# Folded Conversation Segments\n\n")
	for _, b := range blocks {
		tierLabel := "tier-1"
		if b.Tier == 2 {
			tierLabel = "tier-2"
		}
		buf.WriteString(fmt.Sprintf("## [%s] (%s) %s\n%s\n\n",
			b.BlockID, tierLabel, b.TopicTitle, b.Summary))
		// Per-block recovery hints: decompress for precise recovery,
		// search_archive for semantic search of related history.
		buf.WriteString("To recover full details:\n")
		if b.SegmentID != "" {
			buf.WriteString(fmt.Sprintf("- decompress(segment_id=\"%s\") — recover this segment's full conversation\n", b.SegmentID))
		}
		buf.WriteString(fmt.Sprintf("- search_archive(query=\"%s\") — semantic search of related history\n\n", buildSearchQuery(b)))
	}

	return schemas.ContentBlock{
		Text:         buf.String(),
		CacheControl: "ephemeral",
		Metadata: map[string]any{
			"fold_scope": taskScope,
			"fold_count": len(blocks),
		},
	}
}

// buildSearchQuery generates a search query from a FoldBlock's TopicTitle + Summary.
// Used to provide a search_archive hint in fold summary blocks.
func buildSearchQuery(block *FoldBlock) string {
	q := block.TopicTitle
	if len(block.Summary) > 0 {
		summaryPart := block.Summary
		if len(summaryPart) > 80 {
			summaryPart = summaryPart[:80]
		}
		q = q + " " + summaryPart
	}
	return q
}

// insertFoldBlock inserts a fold summary block into the blocks slice.
// It is placed after the first block (Role Instruction) to maintain
// prompt structure: [Role] → [Fold Summaries] → [Precedents] → ...
func insertFoldBlock(blocks []schemas.ContentBlock, foldBlock schemas.ContentBlock) []schemas.ContentBlock {
	if len(blocks) == 0 {
		return []schemas.ContentBlock{foldBlock}
	}
	// Insert after the first block (position 1).
	result := make([]schemas.ContentBlock, 0, len(blocks)+1)
	result = append(result, blocks[0])
	result = append(result, foldBlock)
	result = append(result, blocks[1:]...)
	return result
}

// estimateFoldTokens estimates total tokens for a slice of content blocks.
// Uses the same ~4 chars/token heuristic as PromptBudgetEnforcer.
func estimateFoldTokens(blocks []schemas.ContentBlock) int {
	total := 0
	for _, b := range blocks {
		total += len(b.Text) / charsPerToken
	}
	return total
}
