package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// ─── GetSealedSince Tests ─────────────────────────────────────────────────

func TestGetSealedSince_ReturnsSegmentsSinceBaseline(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Create 3 sealed segments using orthogonal embeddings.
	for i := 0; i < 3; i++ {
		emb := make([]float32, 3)
		emb[i] = 1.0
		sm.AddItem(MemoryItem{
			NodeID:    "item-seal-" + string(rune('A'+i)),
			Intent:    "unique topic " + string(rune('A'+i)),
			Summary:   "summary " + string(rune('A'+i)),
			Embedding: emb,
			Timestamp: time.Now(),
		}, "scope-1")
	}

	if sm.SealedCount() != 2 { // 3 items with orthogonal embeddings → 2 seals (first opens, 2nd seals 1st, 3rd seals 2nd)
		// Actually: item1 opens segment, item2 seals segment1+opens segment2, item3 seals segment2+opens segment3
		// So sealed count = 2 (segments 1 and 2 sealed, segment 3 active)
		t.Fatalf("expected 2 sealed segments, got %d", sm.SealedCount())
	}

	// GetSealedSince(0) should return all sealed segments.
	all := sm.GetSealedSince(0)
	if len(all) != 2 {
		t.Fatalf("GetSealedSince(0): expected 2, got %d", len(all))
	}

	// GetSealedSince(1) should return 1 segment.
	since1 := sm.GetSealedSince(1)
	if len(since1) != 1 {
		t.Fatalf("GetSealedSince(1): expected 1, got %d", len(since1))
	}

	// GetSealedSince(2) should return nil (no segments beyond index 2).
	since2 := sm.GetSealedSince(2)
	if since2 != nil {
		t.Fatalf("GetSealedSince(2): expected nil, got %v", since2)
	}
}

func TestGetSealedSince_NegativeIndex_ReturnsNil(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	if result := sm.GetSealedSince(-1); result != nil {
		t.Error("expected nil for negative index")
	}
}

// ─── GetByNodeIDs Tests ───────────────────────────────────────────────────

func TestGetByNodeIDs_ReturnsMatchingItems(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))

	items := []MemoryItem{
		{NodeID: "node-1", Intent: "intent-1", Summary: "summary-1", Timestamp: time.Now()},
		{NodeID: "node-2", Intent: "intent-2", Summary: "summary-2", Timestamp: time.Now()},
		{NodeID: "node-3", Intent: "intent-3", Summary: "summary-3", Timestamp: time.Now()},
	}
	for _, item := range items {
		mustAppend(t, archive, item)
	}

	result := archive.GetByNodeIDs([]string{"node-1", "node-3"})
	if len(result) != 2 {
		t.Fatalf("expected 2 items, got %d", len(result))
	}

	// Verify the right items were returned.
	nodeIDs := map[string]bool{}
	for _, r := range result {
		nodeIDs[r.NodeID] = true
	}
	if !nodeIDs["node-1"] || !nodeIDs["node-3"] {
		t.Error("expected node-1 and node-3 in results")
	}
}

func TestGetByNodeIDs_EmptyInput_ReturnsNil(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))
	if result := archive.GetByNodeIDs(nil); result != nil {
		t.Error("expected nil for empty input")
	}
}

func TestGetByNodeIDs_NoMatch_ReturnsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "test.jsonl"))
	mustAppend(t, archive, MemoryItem{
		NodeID: "node-1", Intent: "intent-1", Summary: "summary-1", Timestamp: time.Now(),
	})

	result := archive.GetByNodeIDs([]string{"nonexistent"})
	if len(result) != 0 {
		t.Fatalf("expected 0 items, got %d", len(result))
	}
}

// ─── FoldSealedSegments Tests ─────────────────────────────────────────────

func TestFoldSealedSegments_NoFoldWithinBudget(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	// Create a sealed segment.
	sm.AddItem(MemoryItem{
		NodeID:    "item-1",
		Intent:    "topic alpha",
		Summary:   "did alpha",
		Embedding: []float32{1.0, 0.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")
	sm.AddItem(MemoryItem{
		NodeID:    "item-2",
		Intent:    "topic beta",
		Summary:   "did beta",
		Embedding: []float32{0.0, 1.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")

	blocks := []schemas.ContentBlock{
		{Text: "small prompt", Metadata: map[string]any{}},
	}

	// Budget is large — no fold needed.
	result := fe.FoldSealedSegments(blocks, "scope-1", 100000)
	if len(result) != 1 {
		t.Fatalf("expected 1 block (no fold), got %d", len(result))
	}
	if fe.GetFoldBaseline("scope-1") != 0 {
		t.Error("baseline should be 0 when no fold occurred")
	}
}

func TestFoldSealedSegments_FoldWhenOverBudget(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	// Create sealed segments with items in archive.
	for i := 0; i < 3; i++ {
		emb := make([]float32, 3)
		emb[i] = 1.0
		item := MemoryItem{
			NodeID:    "fold-item-" + string(rune('A'+i)),
			Intent:    "unique topic " + string(rune('A'+i)),
			Summary:   "summary " + string(rune('A'+i)),
			Embedding: emb,
			Timestamp: time.Now(),
		}
		sm.AddItem(item, "scope-1")
		mustAppend(t, archive, item)
	}

	// Large prompt that exceeds budget.
	blocks := []schemas.ContentBlock{
		{Text: strings.Repeat("x", 10000), Metadata: map[string]any{}},
	}

	// Small budget — should trigger fold.
	result := fe.FoldSealedSegments(blocks, "scope-1", 100)

	if len(result) < 2 {
		t.Fatalf("expected at least 2 blocks (original + fold summary), got %d", len(result))
	}

	// Fold summary block should contain "Folded Conversation Segments".
	foldFound := false
	for _, b := range result {
		if strings.Contains(b.Text, "Folded Conversation Segments") {
			foldFound = true
			break
		}
	}
	if !foldFound {
		t.Error("expected fold summary block in result")
	}

	// Baseline should be updated.
	if fe.GetFoldBaseline("scope-1") == 0 {
		t.Error("baseline should be > 0 after fold")
	}

	// Fold blocks should exist.
	blocks_list := fe.GetFoldBlocks("scope-1")
	if len(blocks_list) == 0 {
		t.Error("expected fold blocks to exist")
	}
}

func TestFoldSealedSegments_NoNewSealed_NoOp(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	// Create one sealed segment.
	sm.AddItem(MemoryItem{
		NodeID:    "item-1",
		Intent:    "topic alpha",
		Summary:   "did alpha",
		Embedding: []float32{1.0, 0.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")
	sm.AddItem(MemoryItem{
		NodeID:    "item-2",
		Intent:    "topic beta",
		Summary:   "did beta",
		Embedding: []float32{0.0, 1.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-1")

	blocks := []schemas.ContentBlock{
		{Text: strings.Repeat("x", 10000), Metadata: map[string]any{}},
	}

	// First call folds.
	fe.FoldSealedSegments(blocks, "scope-1", 100)
	baseline := fe.GetFoldBaseline("scope-1")
	if baseline == 0 {
		t.Fatal("expected baseline > 0 after first fold")
	}

	// Second call with no new sealed segments — should be no-op.
	result := fe.FoldSealedSegments(blocks, "scope-1", 100)
	if len(result) != 1 {
		t.Fatalf("expected 1 block (no-op), got %d", len(result))
	}
}

func TestFoldSealedSegments_NilSegMgr_NoOp(t *testing.T) {
	fe := NewFoldEngine(nil, nil, nil)
	blocks := []schemas.ContentBlock{{Text: "test"}}
	result := fe.FoldSealedSegments(blocks, "scope-1", 100)
	if len(result) != 1 || result[0].Text != "test" {
		t.Error("nil SegmentManager should be no-op")
	}
}

func TestFoldSealedSegments_NilEngine_NoOp(t *testing.T) {
	var fe *FoldEngine
	blocks := []schemas.ContentBlock{{Text: "test"}}
	result := fe.FoldSealedSegments(blocks, "scope-1", 100)
	if len(result) != 1 || result[0].Text != "test" {
		t.Error("nil FoldEngine should be no-op")
	}
}

// ─── Tier-2 Merge Tests ───────────────────────────────────────────────────

func TestFoldTier2Merge_MergesWhenThresholdExceeded(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	// Create more than tier2Trigger (10) sealed segments.
	for i := 0; i < 12; i++ {
		emb := make([]float32, 12)
		emb[i] = 1.0
		item := MemoryItem{
			NodeID:    "merge-item-" + string(rune('a'+i)),
			Intent:    "unique topic " + string(rune('a'+i)),
			Summary:   "summary " + string(rune('a'+i)),
			Embedding: emb,
			Timestamp: time.Now(),
		}
		sm.AddItem(item, "scope-merge")
		mustAppend(t, archive, item)
	}

	// Trigger fold with small budget.
	blocks := []schemas.ContentBlock{
		{Text: strings.Repeat("x", 100000), Metadata: map[string]any{}},
	}
	fe.FoldSealedSegments(blocks, "scope-merge", 100)

	foldBlocks := fe.GetFoldBlocks("scope-merge")
	if len(foldBlocks) == 0 {
		t.Fatal("expected fold blocks to exist")
	}

	// Check if any tier-2 block was created.
	hasTier2 := false
	tier1Count := 0
	for _, b := range foldBlocks {
		if b.Tier == 2 {
			hasTier2 = true
		} else if b.Tier == 1 {
			tier1Count++
		}
	}

	if !hasTier2 {
		t.Errorf("expected at least one tier-2 block after merge, got %d tier-1 blocks",
			tier1Count)
	}
}

// ─── Decompress Tests ─────────────────────────────────────────────────────

func TestDecompress_RecoversOriginalContent(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	// Create a sealed segment with items in archive.
	item1 := MemoryItem{
		NodeID:    "dec-item-1",
		Intent:    "user question about Go",
		Summary:   "assistant answer about Go",
		TaskID:    "task-1",
		Timestamp: time.Now(),
	}
	item2 := MemoryItem{
		NodeID:    "dec-item-2",
		Intent:    "follow-up about testing",
		Summary:   "answer about testing",
		TaskID:    "task-1",
		Timestamp: time.Now(),
	}

	sm.AddItem(item1, "scope-dec")
	mustAppend(t, archive, item1)
	// Add orthogonal item to seal the first segment.
	sm.AddItem(MemoryItem{
		NodeID:    "dec-item-2",
		Intent:    "completely different topic",
		Summary:   "different answer",
		Embedding: []float32{0.0, 1.0, 0.0},
		Timestamp: time.Now(),
	}, "scope-dec")
	mustAppend(t, archive, item2)

	// Trigger fold.
	blocks := []schemas.ContentBlock{
		{Text: strings.Repeat("x", 10000), Metadata: map[string]any{}},
	}
	fe.FoldSealedSegments(blocks, "scope-dec", 100)

	// Get fold blocks to find a segment ID.
	foldBlocks := fe.GetFoldBlocks("scope-dec")
	if len(foldBlocks) == 0 {
		t.Fatal("expected fold blocks to exist")
	}

	// Decompress the first fold block.
	segID := foldBlocks[0].SegmentID
	content, err := fe.Decompress(segID, "scope-dec")
	if err != nil {
		t.Fatalf("Decompress failed: %v", err)
	}

	if !strings.Contains(content, "Segment:") {
		t.Error("decompressed content should contain segment header")
	}
}

func TestDecompress_SegmentNotFound_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	_, err := fe.Decompress("nonexistent-segment", "scope-1")
	if err == nil {
		t.Error("expected error for nonexistent segment")
	}
}

func TestDecompress_NilArchive_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	fe := NewFoldEngine(sm, nil, nil)

	_, err := fe.Decompress("any-segment", "scope-1")
	if err == nil {
		t.Error("expected error when archive is nil")
	}
}

func TestDecompress_NilEngine_ReturnsError(t *testing.T) {
	var fe *FoldEngine
	_, err := fe.Decompress("any-segment", "scope-1")
	if err == nil {
		t.Error("expected error when fold engine is nil")
	}
}

// ─── Helper Function Tests ────────────────────────────────────────────────

func TestBuildFoldSummaryBlock_Format(t *testing.T) {
	fe := NewFoldEngine(nil, nil, nil)

	blocks := []*FoldBlock{
		{
			BlockID:    "fold_b1",
			SegmentID:  "seg-1",
			TopicTitle: "Setting up project",
			Summary:    "Initialized Go module",
			Tier:       1,
		},
		{
			BlockID:    "fold_b2",
			SegmentID:  "seg-2",
			TopicTitle: "API implementation",
			Summary:    "Created HTTP handlers",
			Tier:       2,
		},
	}

	result := fe.buildFoldSummaryBlock(blocks, "scope-1")

	if !strings.Contains(result.Text, "Folded Conversation Segments") {
		t.Error("should contain header")
	}
	if !strings.Contains(result.Text, "Setting up project") {
		t.Error("should contain first topic title")
	}
	if !strings.Contains(result.Text, "API implementation") {
		t.Error("should contain second topic title")
	}
	if !strings.Contains(result.Text, "tier-1") {
		t.Error("should contain tier-1 label")
	}
	if !strings.Contains(result.Text, "tier-2") {
		t.Error("should contain tier-2 label")
	}
	if !strings.Contains(result.Text, "decompress") {
		t.Error("should mention decompress tool")
	}
}

func TestBuildFoldSummaryBlock_EmptyBlocks_ReturnsEmpty(t *testing.T) {
	fe := NewFoldEngine(nil, nil, nil)
	result := fe.buildFoldSummaryBlock(nil, "scope-1")
	if result.Text != "" {
		t.Error("expected empty text for no blocks")
	}
}

func TestInsertFoldBlock_InsertsAfterFirstBlock(t *testing.T) {
	blocks := []schemas.ContentBlock{
		{Text: "Role Instruction"},
		{Text: "Precedents"},
		{Text: "Tree Path"},
	}
	foldBlock := schemas.ContentBlock{Text: "Fold Summaries"}

	result := insertFoldBlock(blocks, foldBlock)

	if len(result) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(result))
	}
	if result[0].Text != "Role Instruction" {
		t.Error("first block should be Role Instruction")
	}
	if result[1].Text != "Fold Summaries" {
		t.Error("second block should be Fold Summaries")
	}
	if result[2].Text != "Precedents" {
		t.Error("third block should be Precedents")
	}
}

func TestInsertFoldBlock_EmptyBlocks_ReturnsJustFold(t *testing.T) {
	foldBlock := schemas.ContentBlock{Text: "Fold Summaries"}
	result := insertFoldBlock(nil, foldBlock)
	if len(result) != 1 || result[0].Text != "Fold Summaries" {
		t.Error("expected just the fold block")
	}
}

func TestEstimateFoldTokens(t *testing.T) {
	blocks := []schemas.ContentBlock{
		{Text: strings.Repeat("x", 400)}, // 100 tokens
		{Text: strings.Repeat("y", 800)}, // 200 tokens
	}
	total := estimateFoldTokens(blocks)
	if total != 300 {
		t.Errorf("expected 300 tokens, got %d", total)
	}
}

// ─── GetFoldBlocks/GetFoldBaseline Tests ─────────────────────────────────

func TestGetFoldBlocks_NoState_ReturnsNil(t *testing.T) {
	fe := NewFoldEngine(nil, nil, nil)
	if blocks := fe.GetFoldBlocks("nonexistent"); blocks != nil {
		t.Error("expected nil for nonexistent task scope")
	}
}

func TestGetFoldBaseline_NoState_ReturnsZero(t *testing.T) {
	fe := NewFoldEngine(nil, nil, nil)
	if baseline := fe.GetFoldBaseline("nonexistent"); baseline != 0 {
		t.Error("expected 0 for nonexistent task scope")
	}
}

// ─── SetSummarizer Tests ─────────────────────────────────────────────────

func TestFoldSealedSegments_CustomSummarizer(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	fe := NewFoldEngine(sm, archive, nil)

	fe.SetSummarizer(func(seg *MemorySegment) string {
		return "CUSTOM: " + seg.TopicTitle
	})

	// Create sealed segment.
	item1 := MemoryItem{
		NodeID:    "cust-item-1",
		Intent:    "custom topic",
		Summary:   "custom summary",
		Embedding: []float32{1.0, 0.0, 0.0},
		Timestamp: time.Now(),
	}
	item2 := MemoryItem{
		NodeID:    "cust-item-2",
		Intent:    "different topic",
		Summary:   "different summary",
		Embedding: []float32{0.0, 1.0, 0.0},
		Timestamp: time.Now(),
	}
	sm.AddItem(item1, "scope-cust")
	mustAppend(t, archive, item1)
	sm.AddItem(item2, "scope-cust")
	mustAppend(t, archive, item2)

	blocks := []schemas.ContentBlock{
		{Text: strings.Repeat("x", 10000), Metadata: map[string]any{}},
	}
	fe.FoldSealedSegments(blocks, "scope-cust", 100)

	foldBlocks := fe.GetFoldBlocks("scope-cust")
	for _, b := range foldBlocks {
		if !strings.HasPrefix(b.Summary, "CUSTOM:") {
			t.Errorf("expected custom summary prefix, got: %s", b.Summary)
		}
	}
}
