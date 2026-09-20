package core

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── EstimateTokens ──────────────────────────────────────────────────────

func TestEstimateTokens(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)

	if got := e.EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\") = %d, want 0", got)
	}
	if got := e.EstimateTokens("abcd"); got != 1 {
		t.Errorf("EstimateTokens(\"abcd\") = %d, want 1", got)
	}
	if got := e.EstimateTokens("abcdefgh"); got != 2 {
		t.Errorf("EstimateTokens(\"abcdefgh\") = %d, want 2", got)
	}
	// 1-3 chars → 1 token (minimum).
	if got := e.EstimateTokens("a"); got != 1 {
		t.Errorf("EstimateTokens(\"a\") = %d, want 1", got)
	}
	if got := e.EstimateTokens("abc"); got != 1 {
		t.Errorf("EstimateTokens(\"abc\") = %d, want 1", got)
	}
}

func TestEstimateBlocksTokens(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	blocks := []schemas.ContentBlock{
		{Text: "abcdefgh"},         // 2 tokens
		{Text: "abcdefghijklmnop"}, // 4 tokens
	}
	if got := e.EstimateBlocksTokens(blocks); got != 6 {
		t.Errorf("EstimateBlocksTokens = %d, want 6", got)
	}
}

// ─── CompressRoleInstruction ─────────────────────────────────────────────

func TestCompressRoleInstruction_ShortUnchanged(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	text := "You are a helpful assistant.\n\nFollow the rules."
	if got := e.CompressRoleInstruction(text); got != text {
		t.Errorf("short instruction should be unchanged, got: %s", got)
	}
}

func TestCompressRoleInstruction_AtThresholdUnchanged(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	// Exactly at the threshold → unchanged.
	text := strings.Repeat("a", roleInstructionCompressThreshold)
	if got := e.CompressRoleInstruction(text); got != text {
		t.Errorf("instruction at threshold should be unchanged")
	}
}

func TestCompressRoleInstruction_OverThresholdCompresses(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	// Build an instruction with 10 paragraphs, each 300 chars → 3000 total.
	// Each paragraph is unique so we can verify which are retained.
	paragraphs := make([]string, 10)
	for i := range paragraphs {
		paragraphs[i] = fmt.Sprintf("PARA_%d_%s", i, strings.Repeat("x", 290))
	}
	text := strings.Join(paragraphs, "\n\n")

	got := e.CompressRoleInstruction(text)
	if got == text {
		t.Fatal("over-threshold instruction should be compressed")
	}
	if !strings.Contains(got, instructionCompressedPlaceholder) {
		t.Errorf("compressed instruction should contain placeholder, got: %s", got)
	}
	// Should contain the first 3 paragraphs.
	for i := 0; i < 3; i++ {
		if !strings.Contains(got, paragraphs[i]) {
			t.Errorf("compressed instruction should retain head paragraph %d", i)
		}
	}
	// Should contain the last 2 paragraphs.
	for i := 8; i < 10; i++ {
		if !strings.Contains(got, paragraphs[i]) {
			t.Errorf("compressed instruction should retain tail paragraph %d", i)
		}
	}
	// Should NOT contain the middle paragraphs (4-7).
	for i := 4; i < 8; i++ {
		if strings.Contains(got, paragraphs[i]) {
			t.Errorf("compressed instruction should not retain middle paragraph %d", i)
		}
	}
}

func TestCompressRoleInstruction_FewParagraphsUnchanged(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	// Over threshold but fewer than head+tail paragraphs → unchanged.
	text := strings.Repeat("a", 3000) // single long paragraph, no \n\n
	if got := e.CompressRoleInstruction(text); got != text {
		t.Errorf("single-paragraph instruction should be unchanged even if over threshold")
	}
}

// ─── TopNPrecedents ──────────────────────────────────────────────────────

func TestTopNPrecedents_CapsAtN(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	precedents := make([]*schemas.DecisionNode, 10)
	for i := range precedents {
		precedents[i] = &schemas.DecisionNode{ID: string(rune('a' + i))}
	}
	// Request 2 (the hard cap) — should get exactly 2.
	got := e.TopNPrecedents(precedents, precedentsTopN)
	if len(got) != precedentsTopN {
		t.Errorf("TopNPrecedents(10, %d) returned %d, want %d", precedentsTopN, len(got), precedentsTopN)
	}
}

func TestTopNPrecedents_CapsAtHardLimit(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	precedents := make([]*schemas.DecisionNode, 10)
	for i := range precedents {
		precedents[i] = &schemas.DecisionNode{ID: string(rune('a' + i))}
	}
	// Request 100 but hard cap is precedentsTopN (2).
	got := e.TopNPrecedents(precedents, 100)
	if len(got) != precedentsTopN {
		t.Errorf("TopNPrecedents(10, 100) returned %d, want hard cap %d", len(got), precedentsTopN)
	}
}

func TestTopNPrecedents_FewerThanN(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	precedents := []*schemas.DecisionNode{{ID: "a"}, {ID: "b"}}
	got := e.TopNPrecedents(precedents, 5)
	if len(got) != 2 {
		t.Errorf("TopNPrecedents(2, 5) returned %d, want 2", len(got))
	}
}

func TestTopNPrecedents_Empty(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.TopNPrecedents(nil, 5); got != nil {
		t.Errorf("TopNPrecedents(nil, 5) = %v, want nil", got)
	}
	if got := e.TopNPrecedents(nil, 0); got != nil {
		t.Errorf("TopNPrecedents(nil, 0) = %v, want nil", got)
	}
}

// ─── SummarizePrecedent ──────────────────────────────────────────────────

func TestSummarizePrecedent(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	p := &schemas.DecisionNode{
		Outcome:   "refactored auth module",
		Reasoning: "The auth module had circular imports. I broke them by extracting an interface.",
		Action:    "extracted AuthService interface",
	}
	got := e.SummarizePrecedent(p)
	if !strings.Contains(got, "refactored auth module") {
		t.Errorf("summary should contain Outcome, got: %s", got)
	}
	if !strings.Contains(got, "extracted AuthService interface") {
		t.Errorf("summary should contain Action, got: %s", got)
	}
}

func TestSummarizePrecedent_LongReasoningTruncated(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	p := &schemas.DecisionNode{
		Outcome:   "task",
		Reasoning: strings.Repeat("This is a very long reasoning. ", 50),
		Action:    "action",
	}
	got := e.SummarizePrecedent(p)
	// The reasoning should be truncated to the first sentence.
	if strings.Contains(got, strings.Repeat("This is a very long reasoning. ", 50)) {
		t.Errorf("summary should truncate long reasoning, got: %s", got)
	}
}

func TestSummarizePrecedent_Nil(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.SummarizePrecedent(nil); got != "" {
		t.Errorf("SummarizePrecedent(nil) = %q, want empty", got)
	}
}

// ─── firstSentence ───────────────────────────────────────────────────────

func TestFirstSentence(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"Hello world.", "Hello world."},
		{"Hello world. Next sentence.", "Hello world."},
		{"First! Second.", "First!"},
		{"Question? Answer.", "Question?"},
		{"No period here", "No period here"},
	}
	for _, tc := range cases {
		if got := firstSentence(tc.input, 200); got != tc.want {
			t.Errorf("firstSentence(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFirstSentence_Truncation(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := firstSentence(long, 100)
	// 100 chars + "…" (3 bytes in UTF-8) = 103 bytes max.
	if len(got) > 103 {
		t.Errorf("firstSentence should truncate to ~100 chars + ellipsis, got %d bytes", len(got))
	}
}

// ─── IndexCookbook ───────────────────────────────────────────────────────

func TestIndexCookbook_Empty(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.IndexCookbook("", "coding"); got != "" {
		t.Errorf("IndexCookbook(\"\", ...) = %q, want empty", got)
	}
}

func TestIndexCookbook_BuildsTOC(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	content := `# Cookbook

## Section A
Content for A.

## Section B
Content for B.

### Subsection B1
More content.
`
	got := e.IndexCookbook(content, "")
	if !strings.Contains(got, "Model Execution Cookbook (TOC)") {
		t.Errorf("should contain TOC header, got: %s", got)
	}
	if !strings.Contains(got, "Section A") {
		t.Errorf("TOC should list Section A, got: %s", got)
	}
	if !strings.Contains(got, "Section B") {
		t.Errorf("TOC should list Section B, got: %s", got)
	}
	if !strings.Contains(got, "Subsection B1") {
		t.Errorf("TOC should list Subsection B1, got: %s", got)
	}
}

func TestIndexCookbook_AnchorSection(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	content := `# Cookbook

## Coding Patterns
Use these patterns for coding tasks.

## Research Patterns
Use these patterns for research.
`
	got := e.IndexCookbook(content, "coding")
	if !strings.Contains(got, "Anchor:") {
		t.Errorf("should contain anchor section, got: %s", got)
	}
	if !strings.Contains(got, "Coding Patterns") {
		t.Errorf("anchor should be on Coding Patterns, got: %s", got)
	}
	if !strings.Contains(got, "Use these patterns for coding tasks.") {
		t.Errorf("anchor should include section body, got: %s", got)
	}
	// Should NOT include the research section body.
	if strings.Contains(got, "Use these patterns for research.") {
		t.Errorf("anchor should not include unrelated section body, got: %s", got)
	}
}

func TestIndexCookbook_NoHeadingsFallback(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	content := strings.Repeat("plain text line\n", 200)
	got := e.IndexCookbook(content, "coding")
	if got == "" {
		t.Error("should return fallback for heading-less content")
	}
	if !strings.Contains(got, "plain text line") {
		t.Error("fallback should contain original text")
	}
}

// ─── ProgressiveDisclosureSkills ─────────────────────────────────────────

func TestProgressiveDisclosureSkills_Empty(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.ProgressiveDisclosureSkills(nil); got != "" {
		t.Errorf("ProgressiveDisclosureSkills(nil) = %q, want empty", got)
	}
}

func TestProgressiveDisclosureSkills_IndexForm(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	skills := []SkillSummary{
		{ID: "s1", Name: "Coding", Description: "Helps with code generation and review.", Capability: "coding"},
		{ID: "s2", Name: "Research", Description: "Searches the web for information.", Capability: "research"},
	}
	got := e.ProgressiveDisclosureSkills(skills)
	if !strings.Contains(got, "Progressive Disclosure") {
		t.Errorf("should contain progressive disclosure header, got: %s", got)
	}
	if !strings.Contains(got, "Coding") {
		t.Errorf("should list Coding skill, got: %s", got)
	}
	if !strings.Contains(got, "Research") {
		t.Errorf("should list Research skill, got: %s", got)
	}
	if !strings.Contains(got, "skill_view") {
		t.Errorf("should mention skill_view tool, got: %s", got)
	}
}

func TestProgressiveDisclosureSkills_LongDescriptionTruncated(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	skills := []SkillSummary{
		{ID: "s1", Name: "S", Description: strings.Repeat("d", 200), Capability: "c"},
	}
	got := e.ProgressiveDisclosureSkills(skills)
	if strings.Contains(got, strings.Repeat("d", 200)) {
		t.Errorf("long description should be truncated, got: %s", got)
	}
}

// ─── SlidingWindowTreePath ───────────────────────────────────────────────

func TestSlidingWindowTreePath_Empty(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.SlidingWindowTreePath(nil); got != nil {
		t.Errorf("SlidingWindowTreePath(nil) = %v, want nil", got)
	}
}

func TestSlidingWindowTreePath_CollapsesResolved(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	path := []map[string]any{
		{"intent": "old-task-1", "status": schemas.NodeResolved, "summary": "did thing 1"},
		{"intent": "old-task-2", "status": schemas.NodeResolved, "summary": "did thing 2"},
		{"intent": "old-task-3", "status": schemas.NodeResolved, "summary": "did thing 3"},
		{"intent": "old-task-4", "status": schemas.NodeResolved, "summary": "did thing 4"},
		{"intent": "current-task", "status": schemas.NodeActive, "summary": ""},
	}

	got := e.SlidingWindowTreePath(path)
	if len(got) != len(path) {
		t.Fatalf("sliding window should preserve length, got %d want %d", len(got), len(path))
	}

	collapsedCount := 0
	expandedCount := 0
	for _, node := range got {
		if _, ok := node["__collapsed"]; ok {
			collapsedCount++
		} else {
			expandedCount++
		}
	}
	// Only the last active node should be expanded; the rest are resolved and
	// should be collapsed (since they're outside the active window of 3).
	if expandedCount != 1 {
		t.Errorf("expected 1 expanded node (the active one), got %d", expandedCount)
	}
	if collapsedCount != 4 {
		t.Errorf("expected 4 collapsed nodes, got %d", collapsedCount)
	}
}

func TestSlidingWindowTreePath_KeepsRecentActive(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	path := []map[string]any{
		{"intent": "resolved-1", "status": schemas.NodeResolved, "summary": "done 1"},
		{"intent": "active-1", "status": schemas.NodeActive, "summary": ""},
		{"intent": "active-2", "status": schemas.NodeActive, "summary": ""},
		{"intent": "active-3", "status": schemas.NodeActive, "summary": ""},
	}

	got := e.SlidingWindowTreePath(path)
	// The 3 active nodes should be expanded; the resolved one collapsed.
	if _, ok := got[0]["__collapsed"]; !ok {
		t.Error("resolved node should be collapsed")
	}
	for i := 1; i <= 3; i++ {
		if _, ok := got[i]["__collapsed"]; ok {
			t.Errorf("active node %d should be expanded", i)
		}
	}
}

func TestSlidingWindowTreePath_CollapsedForm(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	path := []map[string]any{
		{"intent": "old-task", "status": schemas.NodeResolved, "summary": "did the thing"},
		{"intent": "current", "status": schemas.NodeActive, "summary": ""},
	}
	got := e.SlidingWindowTreePath(path)
	collapsed, ok := got[0]["__collapsed"].(string)
	if !ok {
		t.Fatal("first node should have __collapsed string")
	}
	if !strings.Contains(collapsed, "ASSERT") {
		t.Errorf("collapsed form should contain ASSERT, got: %s", collapsed)
	}
	if !strings.Contains(collapsed, "old-task") {
		t.Errorf("collapsed form should contain intent, got: %s", collapsed)
	}
	if !strings.Contains(collapsed, "did the thing") {
		t.Errorf("collapsed form should contain summary, got: %s", collapsed)
	}
}

// ─── RenderCollapsedTreePath ─────────────────────────────────────────────

func TestRenderCollapsedTreePath_Empty(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.RenderCollapsedTreePath(nil, config.ArchetypeAR); got != "" {
		t.Errorf("RenderCollapsedTreePath(nil) = %q, want empty", got)
	}
}

func TestRenderCollapsedTreePath_AR(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	path := []map[string]any{
		{"intent": "task-1", "status": schemas.NodeResolved, "summary": "completed task 1"},
		{"intent": "task-2", "status": schemas.NodeActive, "summary": ""},
	}
	windowed := e.SlidingWindowTreePath(path)
	got := e.RenderCollapsedTreePath(windowed, config.ArchetypeAR)
	if !strings.Contains(got, "Context Tree Path") {
		t.Errorf("AR render should have Context Tree Path header, got: %s", got)
	}
	if !strings.Contains(got, "ASSERT") {
		t.Errorf("should contain ASSERT for collapsed node, got: %s", got)
	}
	if !strings.Contains(got, "ACTIVE") {
		t.Errorf("should contain ACTIVE for active node, got: %s", got)
	}
}

func TestRenderCollapsedTreePath_Diffusion(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	path := []map[string]any{
		{"intent": "constraint-1", "status": schemas.NodeResolved, "summary": "must be red"},
		{"intent": "constraint-2", "status": schemas.NodeActive, "summary": ""},
	}
	windowed := e.SlidingWindowTreePath(path)
	got := e.RenderCollapsedTreePath(windowed, config.ArchetypeDiffusion)
	if !strings.Contains(got, "Global State Assertions") {
		t.Errorf("Diffusion render should have Global State Assertions header, got: %s", got)
	}
	if !strings.Contains(got, "ACTIVE_CONSTRAINT") {
		t.Errorf("Diffusion should use ACTIVE_CONSTRAINT, got: %s", got)
	}
	if !strings.Contains(got, "RULE: Ignore all historical constraints") {
		t.Errorf("Diffusion should end with ignore rule, got: %s", got)
	}
}

// ─── EnforceBudget ───────────────────────────────────────────────────────

func TestEnforceBudget_UnderBudgetUnchanged(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	blocks := []schemas.ContentBlock{
		{Text: "short block 1"},
		{Text: "short block 2"},
	}
	got := e.EnforceBudget(blocks, 10000)
	if len(got) != len(blocks) {
		t.Errorf("under-budget blocks should be unchanged, got %d blocks want %d", len(got), len(blocks))
	}
}

func TestEnforceBudget_ZeroBudgetUnchanged(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	blocks := []schemas.ContentBlock{{Text: "test"}}
	got := e.EnforceBudget(blocks, 0)
	if len(got) != 1 || got[0].Text != "test" {
		t.Errorf("zero budget should return blocks unchanged")
	}
}

func TestEnforceBudget_DropsTier3First(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	tier3Text := strings.Repeat("t3 ", 1000) // ~3000 chars = 750 tokens
	blocks := []schemas.ContentBlock{
		TagBlock(schemas.ContentBlock{Text: "protected"}, GovTierProtected),
		TagBlock(schemas.ContentBlock{Text: tier3Text}, GovTier3),
	}
	// Budget is small enough that Tier 3 must be dropped.
	got := e.EnforceBudget(blocks, 10)
	for _, b := range got {
		if strings.Contains(b.Text, "t3") {
			t.Error("Tier 3 block should have been dropped")
		}
	}
}

func TestEnforceBudget_ProtectedNeverDropped(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	protectedText := strings.Repeat("p ", 10000) // very large
	blocks := []schemas.ContentBlock{
		MarkProtected(schemas.ContentBlock{Text: protectedText}),
	}
	// Tiny budget — but protected blocks are never dropped.
	got := e.EnforceBudget(blocks, 10)
	if len(got) != 1 {
		t.Fatalf("protected block should never be dropped, got %d blocks", len(got))
	}
	if got[0].Text != protectedText {
		t.Error("protected block text should be unchanged")
	}
}

func TestEnforceBudget_TieredPipeline(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	tier3Text := strings.Repeat("t3 ", 500)
	tier2Text := strings.Repeat("t2 ", 500)
	tier1Text := strings.Repeat("t1 ", 500)
	blocks := []schemas.ContentBlock{
		MarkProtected(schemas.ContentBlock{Text: "protected"}),
		TagBlock(schemas.ContentBlock{Text: tier1Text}, GovTier1),
		TagBlock(schemas.ContentBlock{Text: tier2Text}, GovTier2),
		TagBlock(schemas.ContentBlock{Text: tier3Text}, GovTier3),
	}
	// Budget requires dropping Tier 3 + Tier 2.
	got := e.EnforceBudget(blocks, 200)
	hasTier3 := false
	hasTier2 := false
	for _, b := range got {
		if strings.Contains(b.Text, "t3") {
			hasTier3 = true
		}
		if strings.Contains(b.Text, "t2") {
			hasTier2 = true
		}
	}
	if hasTier3 {
		t.Error("Tier 3 should be dropped first")
	}
	if hasTier2 {
		t.Error("Tier 2 should be dropped second")
	}
}

func TestEnforceBudget_CompressesTier1(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	// Build a Tier 1 block that's over the compression threshold.
	paragraphs := make([]string, 10)
	for i := range paragraphs {
		paragraphs[i] = strings.Repeat("a", 300)
	}
	tier1Text := strings.Join(paragraphs, "\n\n")

	blocks := []schemas.ContentBlock{
		MarkProtected(schemas.ContentBlock{Text: "protected"}),
		TagBlock(schemas.ContentBlock{Text: tier1Text}, GovTier1),
	}
	// Budget requires compressing Tier 1.
	got := e.EnforceBudget(blocks, 400)
	for _, b := range got {
		if strings.Contains(b.Text, instructionCompressedPlaceholder) {
			return // success: Tier 1 was compressed
		}
	}
	t.Error("Tier 1 should have been compressed")
}

// ─── TagBlock / MarkProtected ────────────────────────────────────────────

func TestTagBlock(t *testing.T) {
	block := schemas.ContentBlock{Text: "test"}
	tagged := TagBlock(block, GovTier1)
	tier, ok := tagged.Metadata[GovernanceMarker].(string)
	if !ok {
		t.Fatal("TagBlock should set GovernanceMarker in Metadata")
	}
	if tier != GovTier1 {
		t.Errorf("TagBlock tier = %q, want %q", tier, GovTier1)
	}
}

func TestMarkProtected(t *testing.T) {
	block := schemas.ContentBlock{Text: "test"}
	protected := MarkProtected(block)
	tier, ok := protected.Metadata[GovernanceMarker].(string)
	if !ok {
		t.Fatal("MarkProtected should set GovernanceMarker in Metadata")
	}
	if tier != GovTierProtected {
		t.Errorf("MarkProtected tier = %q, want %q", tier, GovTierProtected)
	}
}

func TestTagBlock_PreservesExistingMetadata(t *testing.T) {
	block := schemas.ContentBlock{
		Text:     "test",
		Metadata: map[string]any{"existing": "value"},
	}
	tagged := TagBlock(block, GovTier2)
	if tagged.Metadata["existing"] != "value" {
		t.Error("TagBlock should preserve existing Metadata")
	}
	if tagged.Metadata[GovernanceMarker] != GovTier2 {
		t.Error("TagBlock should add GovernanceMarker")
	}
}
