package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ─── Integration: PromptAssembler.Build + Prompt Governance ──────────────
//
// These tests verify the end-to-end wiring of the model-aware budget
// enforcement into PromptAssembler.Build, per
// docs/architecture/PROMPT_GOVERNANCE_AND_BUDGET_DESIGN.md.

// newGovernanceTestSpawner builds a minimal Spawner suitable for governance
// integration tests. The ExperienceStore is empty (no anti-patterns,
// no similar tasks, no failure modes) so the only blocks produced are
// the ones the test explicitly controls via Build's arguments.
func newGovernanceTestSpawner(t *testing.T) *Spawner {
	t.Helper()
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es, err := store.NewExperienceStore(tmpDir, ts, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}
	reg := &config.Registry{}
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })
	return NewSpawner(reg, ts, es, logger, nil, "outputs")
}

// TestBuild_PrecedentsCappedAtTopN verifies that Build caps injected
// precedents at precedentsTopN (2) per §4.2, regardless of how many
// are passed in.
func TestBuild_PrecedentsCappedAtTopN(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	// Pass 10 precedents — only 2 should be injected.
	precedents := make([]*schemas.DecisionNode, 10)
	for i := range precedents {
		precedents[i] = &schemas.DecisionNode{
			ID:        string(rune('a' + i)),
			Outcome:   "outcome-" + string(rune('a'+i)),
			Reasoning: "reasoning",
			Action:    "action",
		}
	}

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, nil, nil, false,
		precedents, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// Count how many precedent outcomes appear in the blocks.
	foundCount := 0
	for _, b := range blocks {
		for _, p := range precedents {
			if strings.Contains(b.Text, p.Outcome) {
				foundCount++
			}
		}
	}
	if foundCount > precedentsTopN {
		t.Errorf("expected at most %d precedents injected, got %d", precedentsTopN, foundCount)
	}
	if foundCount == 0 {
		t.Error("expected at least 1 precedent to be injected")
	}
}

// TestBuild_RoleInstructionCompressed verifies that an over-threshold
// Role.Instruction is compressed via head+tail retention per §4.1.
func TestBuild_RoleInstructionCompressed(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	// Build an instruction with 10 paragraphs, each 300 chars → 3000 total.
	paragraphs := make([]string, 10)
	for i := range paragraphs {
		paragraphs[i] = "UNIQUE_PARA_" + string(rune('A'+i)) + "_" + strings.Repeat("x", 280)
	}
	longInstruction := strings.Join(paragraphs, "\n\n")

	role := &config.Role{ID: "r1", Name: "test", Instruction: longInstruction}

	blocks, err := spawner.buildSystemPrompt(
		nil, role, nil, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, nil, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// Find the role instruction block.
	var roleBlock *schemas.ContentBlock
	for i := range blocks {
		if strings.Contains(blocks[i].Text, "# Role: test") {
			roleBlock = &blocks[i]
			break
		}
	}
	if roleBlock == nil {
		t.Fatal("role instruction block not found")
	}

	// Should contain the placeholder.
	if !strings.Contains(roleBlock.Text, instructionCompressedPlaceholder) {
		t.Errorf("over-threshold instruction should be compressed with placeholder, got: %s", roleBlock.Text)
	}
	// Should retain the first 3 paragraphs.
	for i := 0; i < 3; i++ {
		if !strings.Contains(roleBlock.Text, paragraphs[i]) {
			t.Errorf("compressed instruction should retain head paragraph %d", i)
		}
	}
	// Should retain the last 2 paragraphs.
	for i := 8; i < 10; i++ {
		if !strings.Contains(roleBlock.Text, paragraphs[i]) {
			t.Errorf("compressed instruction should retain tail paragraph %d", i)
		}
	}
	// Should NOT retain middle paragraphs.
	for i := 4; i < 8; i++ {
		if strings.Contains(roleBlock.Text, paragraphs[i]) {
			t.Errorf("compressed instruction should not retain middle paragraph %d", i)
		}
	}
}

// TestBuild_TreePathSlidingWindow verifies that resolved historical nodes
// are collapsed to ASSERT statements while recent active nodes stay expanded.
func TestBuild_TreePathSlidingWindow(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	path := []map[string]any{
		{"intent": "old-resolved-1", "status": schemas.NodeResolved, "summary": "did old thing 1"},
		{"intent": "old-resolved-2", "status": schemas.NodeResolved, "summary": "did old thing 2"},
		{"intent": "old-resolved-3", "status": schemas.NodeResolved, "summary": "did old thing 3"},
		{"intent": "old-resolved-4", "status": schemas.NodeResolved, "summary": "did old thing 4"},
		{"intent": "current-active", "status": schemas.NodeActive, "summary": ""},
	}
	mergedContext := map[string]any{"path": path}

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, mergedContext, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// Find the tree path block.
	var treeBlock *schemas.ContentBlock
	for i := range blocks {
		if strings.Contains(blocks[i].Text, "Context Tree Path") {
			treeBlock = &blocks[i]
			break
		}
	}
	if treeBlock == nil {
		t.Fatal("tree path block not found")
	}

	// The active node should be expanded.
	if !strings.Contains(treeBlock.Text, "ACTIVE") {
		t.Errorf("active node should be expanded, got: %s", treeBlock.Text)
	}
	if !strings.Contains(treeBlock.Text, "current-active") {
		t.Errorf("active node intent should appear, got: %s", treeBlock.Text)
	}
	// The old resolved nodes should be collapsed to ASSERT.
	if !strings.Contains(treeBlock.Text, "ASSERT") {
		t.Errorf("resolved nodes should be collapsed to ASSERT, got: %s", treeBlock.Text)
	}
}

// TestBuild_BudgetEnforced verifies that the model-aware budget is enforced
// when the total prompt exceeds the budget. We use a lightweight model
// (4000 token budget) and a massive Tier 3 block to trigger trimming.
func TestBuild_BudgetEnforced(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	// Use a lightweight model → 4000 token budget = 16000 chars.
	// Inject a massive tree path (Tier 3) that exceeds the budget.
	hugePath := make([]map[string]any, 100)
	for i := range hugePath {
		hugePath[i] = map[string]any{
			"intent":  "task-" + strings.Repeat("x", 200),
			"status":  schemas.NodeResolved,
			"summary": strings.Repeat("s", 200),
		}
	}
	mergedContext := map[string]any{"path": hugePath}

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "llama-3-8b"}, []string{}, mergedContext, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// The total tokens should not exceed the budget (4000) by more than
	// a small margin (the hard truncate is a best-effort ceiling).
	enforcer := NewPromptBudgetEnforcer(nil)
	totalTokens := enforcer.EstimateBlocksTokens(blocks)
	budget := config.DefaultSystemPromptBudgetLight

	// Allow some slack for protected blocks that can't be trimmed.
	if totalTokens > budget*3 {
		t.Errorf("total tokens %d far exceed budget %d — enforcement may not be working", totalTokens, budget)
	}
}

// TestBuild_ProtectedBlocksSurviveBudgetPressure verifies that protected
// blocks (role rules, tool usage, output contract) are never trimmed even
// when the budget is severely exceeded.
func TestBuild_ProtectedBlocksSurviveBudgetPressure(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	role := &config.Role{
		ID:          "r1",
		Name:        "test",
		Rules:       "UNIQUE_HARD_RULE_MARKER_DO_NOT_TRIM",
		Instruction: "short",
	}

	// Massive tree path to trigger budget pressure.
	hugePath := make([]map[string]any, 50)
	for i := range hugePath {
		hugePath[i] = map[string]any{
			"intent":  strings.Repeat("x", 200),
			"status":  schemas.NodeResolved,
			"summary": strings.Repeat("s", 200),
		}
	}
	mergedContext := map[string]any{"path": hugePath}

	blocks, err := spawner.buildSystemPrompt(
		nil, role, nil, "coding",
		&config.ProviderConfig{Model: "llama-3-8b"}, []string{}, mergedContext, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// The hard rule marker must survive.
	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "UNIQUE_HARD_RULE_MARKER_DO_NOT_TRIM") {
			found = true
			break
		}
	}
	if !found {
		t.Error("protected hard rules must survive budget enforcement")
	}
}

// TestBuild_LightModelBudgetTighterThanFlagship verifies that a lightweight
// model gets a tighter budget than a flagship model, causing more aggressive
// trimming for the same input.
func TestBuild_LightModelBudgetTighterThanFlagship(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	// Build a moderately large tree path.
	path := make([]map[string]any, 20)
	for i := range path {
		path[i] = map[string]any{
			"intent":  "task-" + strings.Repeat("x", 100),
			"status":  schemas.NodeResolved,
			"summary": strings.Repeat("s", 100),
		}
	}
	mergedContext := map[string]any{"path": path}

	lightBlocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "llama-3-8b"}, []string{}, mergedContext, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("light model buildSystemPrompt: %v", err)
	}

	flagshipBlocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, mergedContext, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("flagship model buildSystemPrompt: %v", err)
	}

	enforcer := NewPromptBudgetEnforcer(nil)
	lightTokens := enforcer.EstimateBlocksTokens(lightBlocks)
	flagshipTokens := enforcer.EstimateBlocksTokens(flagshipBlocks)

	// The light model should have a smaller or equal output (tighter budget).
	if lightTokens > flagshipTokens {
		t.Errorf(
			"light model (%d tokens) should not produce more tokens than flagship (%d) for the same input",
			lightTokens, flagshipTokens,
		)
	}
}

// TestBuild_FewShotsTaggedTier3 verifies that few-shot blocks are tagged
// as Tier 3 (trim first) so they're the first to go under budget pressure.
func TestBuild_FewShotsTaggedTier3(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	fewShots := []string{"example 1", "example 2"}

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, nil, fewShots, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// Find the few-shot block and verify it's tagged Tier 3.
	for _, b := range blocks {
		if strings.Contains(b.Text, "Tool Call Examples") {
			tier, ok := b.Metadata[GovernanceMarker].(string)
			if !ok {
				t.Fatal("few-shot block should have governance tier marker")
			}
			if tier != GovTier3 {
				t.Errorf("few-shot block tier = %q, want %q", tier, GovTier3)
			}
			return
		}
	}
	t.Fatal("few-shot block not found")
}

// TestBuild_CookbookIndexedNotFull verifies that cookbook injection uses
// TOC + anchor indexing rather than the full text.
func TestBuild_CookbookIndexedNotFull(t *testing.T) {
	// We can't easily inject a cookbook without a real ResourceLoader,
	// but we can verify the IndexCookbook function directly.
	enforcer := NewPromptBudgetEnforcer(nil)
	fullCookbook := `# Cookbook

## Coding
Long coding content that should not appear in full.

## Research
Long research content.
`
	indexed := enforcer.IndexCookbook(fullCookbook, "coding")
	if !strings.Contains(indexed, "TOC") {
		t.Errorf("indexed cookbook should have TOC, got: %s", indexed)
	}
	if strings.Contains(indexed, "Long research content.") {
		t.Errorf("indexed cookbook should not include unrelated section body, got: %s", indexed)
	}
}

// TestBuild_SingleSkillFullPrompt verifies that a single skill gets its
// full prompt (no progressive disclosure when there's no contention).
func TestBuild_SingleSkillFullPrompt(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	// Register a skill in the registry.
	spawner.registry.Skills = map[string]*config.Skill{
		"s1": {
			ID:          "s1",
			Name:        "CodingSkill",
			Capability:  "coding",
			Description: "A coding skill.",
			Implementations: map[string]config.SkillImplementation{
				"default": {SystemPrompt: "UNIQUE_FULL_SKILL_PROMPT_MARKER"},
			},
		},
	}

	hub := NewContextHub(spawner.registry, nil, spawner.expStore)
	hub.Registry = spawner.registry

	blocks, err := spawner.buildSystemPrompt(
		hub, &config.Role{ID: "r1", Name: "test"}, []string{"s1"}, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, nil, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "UNIQUE_FULL_SKILL_PROMPT_MARKER") {
			found = true
			break
		}
	}
	if !found {
		t.Error("single skill should inject its full prompt, not progressive disclosure")
	}
}

// TestBuild_MultipleSkillsProgressiveDisclosure verifies that multiple
// skills use progressive disclosure (Trigger + one-line Description only).
func TestBuild_MultipleSkillsProgressiveDisclosure(t *testing.T) {
	spawner := newGovernanceTestSpawner(t)

	spawner.registry.Skills = map[string]*config.Skill{
		"s1": {
			ID:          "s1",
			Name:        "CodingSkill",
			Capability:  "coding",
			Description: "A coding skill.",
			Implementations: map[string]config.SkillImplementation{
				"default": {SystemPrompt: "FULL_PROMPT_S1_SHOULD_NOT_APPEAR"},
			},
		},
		"s2": {
			ID:          "s2",
			Name:        "ResearchSkill",
			Capability:  "research",
			Description: "A research skill.",
			Implementations: map[string]config.SkillImplementation{
				"default": {SystemPrompt: "FULL_PROMPT_S2_SHOULD_NOT_APPEAR"},
			},
		},
	}

	hub := NewContextHub(spawner.registry, nil, spawner.expStore)
	hub.Registry = spawner.registry

	blocks, err := spawner.buildSystemPrompt(
		hub, &config.Role{ID: "r1", Name: "test"}, []string{"s1", "s2"}, "coding",
		&config.ProviderConfig{Model: "gpt-4o"}, []string{}, nil, nil, false,
		nil, "do the task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	// Should have progressive disclosure header.
	foundPD := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Progressive Disclosure") {
			foundPD = true
		}
		if strings.Contains(b.Text, "FULL_PROMPT_S1_SHOULD_NOT_APPEAR") {
			t.Error("full skill prompt should not appear under progressive disclosure")
		}
		if strings.Contains(b.Text, "FULL_PROMPT_S2_SHOULD_NOT_APPEAR") {
			t.Error("full skill prompt should not appear under progressive disclosure")
		}
	}
	if !foundPD {
		t.Error("expected progressive disclosure header for multiple skills")
	}
}
