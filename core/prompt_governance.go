package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── Prompt Governance & Model-Aware Dynamic Budget ──────────────────────
//
// Design ref: docs/architecture/PROMPT_GOVERNANCE_AND_BUDGET_DESIGN.md
//
// PromptBudgetEnforcer is the runtime two-level interception layer for the
// System Prompt. It is a pure observer+transformer: given a slice of
// ContentBlocks and a token budget, it returns a possibly-trimmed slice.
// It never touches the Spawner, the task store, or the budget guard.
//
// The enforcer implements the tiered trimming pipeline from §5:
//
//	Tier 3 (trim first): Tree Path, Precedents, Few-Shots
//	Tier 2 (trim next):  Cookbook, Failure Profiles
//	Tier 1 (compress):   Role Instruction (keep head + tail paragraphs)
//
// Protected zones (Role name + Hard Rules + Tool Usage + Output Contract)
// are never trimmed — they carry CacheControl="ephemeral" and are tagged
// with the protected marker in Metadata.

const (
	// GovernanceMarker is the Metadata key used to tag a block's governance tier.
	GovernanceMarker = "__gov_tier"

	// GovTierProtected marks a block that must never be trimmed.
	GovTierProtected = "protected"
	// GovTier1 marks Role Instruction (compressible via head+tail retention).
	GovTier1 = "tier1_role_instruction"
	// GovTier2 marks Cookbook / Failure Profiles (trimmable).
	GovTier2 = "tier2_cookbook_failure"
	// GovTier3 marks Tree Path / Precedents / Few-Shots (trim first).
	GovTier3 = "tier3_tree_precedent_fewshot"

	// charsPerToken mirrors config.CharsPerToken for token estimation.
	charsPerToken = 4

	// roleInstructionCompressThreshold is the char length above which
	// CompressRoleInstruction kicks in. Below this, the instruction is
	// returned verbatim.
	roleInstructionCompressThreshold = 2000

	// precedentsTopN is the hard cap on injected precedents (§4.2).
	precedentsTopN = 2

	// treePathActiveWindow is the number of recent Active/Running nodes
	// kept fully expanded (§4.5). Older resolved nodes collapse to ASSERT.
	treePathActiveWindow = 3

	// instructionHeadParagraphs / instructionTailParagraphs control the
	// head/tail retention pattern for Role Instruction compression (§4.1).
	instructionHeadParagraphs = 3
	instructionTailParagraphs = 2

	instructionCompressedPlaceholder = "[... instruction body compressed for brevity ...]"
)

// PromptBudgetEnforcer applies the model-aware System Prompt budget.
// Construct one per PromptAssembler (or per-call; it is stateless).
type PromptBudgetEnforcer struct {
	logger *Logger
}

// NewPromptBudgetEnforcer creates an enforcer. logger may be nil — the
// enforcer is a pure transformer and only logs when it actually trims.
func NewPromptBudgetEnforcer(logger *Logger) *PromptBudgetEnforcer {
	return &PromptBudgetEnforcer{logger: logger}
}

// EstimateTokens returns a rough token count for a text block using the
// ~4 chars/token heuristic. This is intentionally conservative (English+
// code averages ~4; CJK is denser but we prefer over-estimating budget
// usage to under-estimating).
func (e *PromptBudgetEnforcer) EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	n := len(text) / charsPerToken
	if n == 0 {
		n = 1
	}
	return n
}

// EstimateBlocksTokens sums the token estimate across all blocks.
func (e *PromptBudgetEnforcer) EstimateBlocksTokens(blocks []schemas.ContentBlock) int {
	total := 0
	for i := range blocks {
		total += e.EstimateTokens(blocks[i].Text)
	}
	return total
}

// ─── Tier 1: Role Instruction Compression ────────────────────────────────

// CompressRoleInstruction retains the first instructionHeadParagraphs and
// last instructionTailParagraphs paragraphs of the instruction, replacing
// the middle with a placeholder. If the instruction has fewer than
// (head+tail) paragraphs, it is returned verbatim.
//
// Paragraphs are split on blank-line boundaries (\n\n). This preserves
// markdown structure (each paragraph is typically one logical block).
func (e *PromptBudgetEnforcer) CompressRoleInstruction(text string) string {
	if len(text) <= roleInstructionCompressThreshold {
		return text
	}
	paragraphs := strings.Split(text, "\n\n")
	keep := instructionHeadParagraphs + instructionTailParagraphs
	if len(paragraphs) <= keep {
		return text
	}

	head := paragraphs[:instructionHeadParagraphs]
	tail := paragraphs[len(paragraphs)-instructionTailParagraphs:]

	var sb strings.Builder
	sb.WriteString(strings.Join(head, "\n\n"))
	sb.WriteString("\n\n")
	sb.WriteString(instructionCompressedPlaceholder)
	sb.WriteString("\n\n")
	sb.WriteString(strings.Join(tail, "\n\n"))
	return sb.String()
}

// ─── Tier 3: Precedents Top-N + Single-Sentence Summary ──────────────────

// TopNPrecedents returns at most n precedents. Per §4.2, the hard cap is
// precedentsTopN (2). The caller passes n=precedentsTopN in the normal path;
// exposing n as a parameter makes the cap testable.
func (e *PromptBudgetEnforcer) TopNPrecedents(precedents []*schemas.DecisionNode, n int) []*schemas.DecisionNode {
	if len(precedents) == 0 || n <= 0 {
		return nil
	}
	if n > precedentsTopN {
		n = precedentsTopN
	}
	if len(precedents) <= n {
		return precedents
	}
	return precedents[:n]
}

// SummarizePrecedent produces a single-sentence summary of a DecisionNode
// per §4.2: Task Intent + 1-Sentence Reasoning Summary + Action Taken.
// Long Reasoning is truncated to the first sentence (up to 200 chars).
func (e *PromptBudgetEnforcer) SummarizePrecedent(p *schemas.DecisionNode) string {
	if p == nil {
		return ""
	}
	reasoning := firstSentence(p.Reasoning, 200)
	return fmt.Sprintf("- **Intent**: %s\n  **Reasoning**: %s\n  **Action**: %s", p.Outcome, reasoning, p.Action)
}

// firstSentence returns the first sentence of s, capped at maxChars.
// A sentence boundary is one of . ! ? followed by a space or end-of-string.
func firstSentence(s string, maxChars int) string {
	if s == "" {
		return ""
	}
	if maxChars <= 0 {
		maxChars = 200
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '.' || c == '!' || c == '?') && (i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\n' || s[i+1] == '\t') {
			if i+1 <= maxChars {
				return s[:i+1]
			}
			break
		}
	}
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…"
}

// ─── Tier 2: Cookbook TOC + Anchor Indexing ──────────────────────────────

// IndexCookbook extracts the Table of Contents from a cookbook markdown
// document and returns TOC + the section matching the current capability
// anchor. Per §4.3, the full cookbook text is never injected.
//
// The TOC is built from markdown headings (lines starting with # / ## / ###).
// The anchor section is the heading whose text (lowercased) contains the
// capability (lowercased), plus its body up to the next heading of the
// same or higher level.
func (e *PromptBudgetEnforcer) IndexCookbook(content, capability string) string {
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")

	type heading struct {
		level int
		text  string
		line  int
	}
	var headings []heading
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			text := strings.TrimSpace(trimmed[level:])
			if text != "" {
				headings = append(headings, heading{level: level, text: text, line: i})
			}
		}
	}

	if len(headings) == 0 {
		// No headings — fall back to a char-capped prefix.
		if len(content) > 2000 {
			return content[:2000] + "\n[... cookbook truncated ...]"
		}
		return content
	}

	var sb strings.Builder
	sb.WriteString("# Model Execution Cookbook (TOC)\n\n")
	for _, h := range headings {
		indent := strings.Repeat("  ", h.level-1)
		sb.WriteString(fmt.Sprintf("%s- %s\n", indent, h.text))
	}

	// Anchor section: find the heading matching the capability.
	if capability != "" {
		lowerCap := strings.ToLower(capability)
		for idx, h := range headings {
			if strings.Contains(strings.ToLower(h.text), lowerCap) {
				startLine := h.line
				endLine := len(lines)
				if idx+1 < len(headings) {
					endLine = headings[idx+1].line
				}
				sb.WriteString("\n\n## Anchor: ")
				sb.WriteString(h.text)
				sb.WriteString("\n\n")
				sectionBody := strings.Join(lines[startLine:endLine], "\n")
				if len(sectionBody) > 3000 {
					sectionBody = sectionBody[:3000] + "\n[... section truncated ...]"
				}
				sb.WriteString(sectionBody)
				break
			}
		}
	}
	return sb.String()
}

// ─── Tier 2/3: Skill Progressive Disclosure ──────────────────────────────

// SkillSummary is the minimal info needed for progressive disclosure.
type SkillSummary struct {
	ID          string
	Name        string
	Description string
	Capability  string
}

// ProgressiveDisclosureSkills returns the index-only form of multiple skills
// per §4.4: each skill contributes just its Trigger + one-line Description.
// When only a single skill is loaded, its full prompt is preserved by the
// caller (this function is only invoked when len > 1).
func (e *PromptBudgetEnforcer) ProgressiveDisclosureSkills(skills []SkillSummary) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# Active Skills (Progressive Disclosure)\n\n")
	sb.WriteString("The following skills are active. Use `skill_view` to load full details on demand.\n\n")
	for _, s := range skills {
		desc := s.Description
		if desc == "" {
			desc = s.Capability
		}
		if idx := strings.IndexAny(desc, "\n."); idx > 0 {
			desc = desc[:idx]
		}
		if len(desc) > 120 {
			desc = desc[:120] + "…"
		}
		sb.WriteString(fmt.Sprintf("- **%s** (`%s`): %s\n", s.Name, s.ID, desc))
	}
	return sb.String()
}

// ─── Tier 3: DAG Tree Path Sliding Window ────────────────────────────────

// SlidingWindowTreePath collapses resolved historical nodes to single-line
// ASSERT statements and keeps only the most recent treePathActiveWindow
// Active/Running nodes fully expanded. Per §4.5.
//
// Returns a new slice; the input is not mutated. The order is preserved
// (ASSERT lines for resolved nodes appear in their original position, but
// as single lines instead of multi-line blocks).
func (e *PromptBudgetEnforcer) SlidingWindowTreePath(path []map[string]any) []map[string]any {
	if len(path) == 0 {
		return path
	}

	// Find the indices of the last treePathActiveWindow active/non-resolved nodes.
	activeIndices := make([]int, 0, treePathActiveWindow)
	for i := len(path) - 1; i >= 0 && len(activeIndices) < treePathActiveWindow; i-- {
		node := path[i]
		status, ok := node["status"].(schemas.NodeStatus)
		if !ok {
			// Unknown status — treat as active (keep expanded).
			activeIndices = append(activeIndices, i)
			continue
		}
		if status != schemas.NodeResolved && status != schemas.NodeAbandoned {
			activeIndices = append(activeIndices, i)
		}
	}
	keepExpanded := make(map[int]bool, len(activeIndices))
	for _, idx := range activeIndices {
		keepExpanded[idx] = true
	}

	result := make([]map[string]any, 0, len(path))
	for i, node := range path {
		if keepExpanded[i] {
			result = append(result, node)
			continue
		}
		// Collapse to ASSERT form.
		intent, _ := node["intent"].(string)
		summary, _ := node["summary"].(string)
		collapsed := map[string]any{
			"intent":      intent,
			"status":      node["status"],
			"summary":     summary,
			"__collapsed": fmt.Sprintf("ASSERT [%s]: %s", intent, summary),
		}
		result = append(result, collapsed)
	}
	return result
}

// RenderCollapsedTreePath renders a path that has been through
// SlidingWindowTreePath, emitting single-line ASSERT for collapsed nodes
// and the full multi-line form for expanded nodes. This mirrors the
// existing tree-path rendering in PromptAssembler.Build but respects the
// sliding window.
func (e *PromptBudgetEnforcer) RenderCollapsedTreePath(path []map[string]any, archetype config.ModelArchetype) string {
	if len(path) == 0 {
		return ""
	}
	var sb strings.Builder
	if archetype == config.ArchetypeDiffusion {
		sb.WriteString("# Global State Assertions (Constraints)\n")
	} else {
		sb.WriteString("# Context Tree Path\n")
	}

	for _, node := range path {
		if collapsed, ok := node["__collapsed"].(string); ok {
			sb.WriteString(collapsed)
			sb.WriteString("\n")
			continue
		}
		status, _ := node["status"].(schemas.NodeStatus)
		intent, _ := node["intent"].(string)
		summary, _ := node["summary"].(string)

		if archetype == config.ArchetypeDiffusion {
			if status == schemas.NodeResolved && summary != "" {
				fmt.Fprintf(&sb, "ASSERT [%s]: %s\n", intent, summary)
			} else {
				fmt.Fprintf(&sb, "ACTIVE_CONSTRAINT: Focus on %s\n", intent)
			}
		} else {
			if status == schemas.NodeResolved && summary != "" {
				fmt.Fprintf(&sb, "## [RESOLVED] %s\n%s\n\n", intent, summary)
			} else {
				fmt.Fprintf(&sb, "## [ACTIVE] %s\n(Currently working on this branch)\n\n", intent)
			}
		}
	}

	// Diffusion models always get the ignore-rule at the end, regardless of
	// whether the last node was collapsed or expanded.
	if archetype == config.ArchetypeDiffusion {
		sb.WriteString("RULE: Ignore all historical constraints contradicting the above.\n")
	}
	return sb.String()
}

// ─── Budget Enforcement Pipeline ─────────────────────────────────────────

// EnforceBudget is the main entry point. If the total estimated tokens of
// blocks is within budgetTokens, blocks is returned unchanged. Otherwise the
// tiered trimming pipeline is applied:
//
//  1. Tier 3 blocks (Tree Path / Precedents / Few-Shots) are dropped first.
//  2. If still over, Tier 2 blocks (Cookbook / Failure Profiles) are dropped.
//  3. If still over, Tier 1 blocks (Role Instruction) are compressed via
//     CompressRoleInstruction (head+tail retention, never fully dropped).
//  4. Protected blocks are never touched.
//
// The function logs a governance event when trimming occurs (if logger != nil).
func (e *PromptBudgetEnforcer) EnforceBudget(blocks []schemas.ContentBlock, budgetTokens int) []schemas.ContentBlock {
	if budgetTokens <= 0 {
		return blocks
	}
	total := e.EstimateBlocksTokens(blocks)
	if total <= budgetTokens {
		return blocks
	}

	result := make([]schemas.ContentBlock, len(blocks))
	copy(result, blocks)

	originalTotal := total

	// Phase 1: drop Tier 3 blocks (cheapest to lose).
	result, total = e.dropTier(result, GovTier3, total, budgetTokens)
	if total <= budgetTokens {
		e.logTrim(originalTotal, total, budgetTokens, "tier3_dropped")
		return result
	}

	// Phase 2: drop Tier 2 blocks.
	result, total = e.dropTier(result, GovTier2, total, budgetTokens)
	if total <= budgetTokens {
		e.logTrim(originalTotal, total, budgetTokens, "tier2_dropped")
		return result
	}

	// Phase 3: compress Tier 1 blocks (Role Instruction).
	result, total = e.compressTier1(result, total, budgetTokens)
	if total <= budgetTokens {
		e.logTrim(originalTotal, total, budgetTokens, "tier1_compressed")
		return result
	}

	// Still over budget after all tiers — last resort: truncate the largest
	// non-protected block's text to fit. This is the hard ceiling.
	result = e.hardTruncate(result, budgetTokens)
	e.logTrim(originalTotal, e.EstimateBlocksTokens(result), budgetTokens, "hard_truncate")
	return result
}

// dropTier removes all blocks tagged with the given tier marker and returns
// the reduced slice + new total. Protected blocks are never dropped even if
// they happen to carry the tier marker (they shouldn't, but be safe).
func (e *PromptBudgetEnforcer) dropTier(blocks []schemas.ContentBlock, tier string, total, budget int) ([]schemas.ContentBlock, int) {
	result := make([]schemas.ContentBlock, 0, len(blocks))
	for i := range blocks {
		tierVal, _ := blocks[i].Metadata[GovernanceMarker].(string)
		if tierVal == tier && tierVal != GovTierProtected {
			total -= e.EstimateTokens(blocks[i].Text)
			if total <= budget {
				// Keep remaining blocks (no need to drop more).
				result = append(result, blocks[i+1:]...)
				return result, total
			}
			continue
		}
		result = append(result, blocks[i])
	}
	return result, total
}

// compressTier1 applies CompressRoleInstruction to every Tier 1 block.
func (e *PromptBudgetEnforcer) compressTier1(blocks []schemas.ContentBlock, total, budget int) ([]schemas.ContentBlock, int) {
	for i := range blocks {
		tierVal, _ := blocks[i].Metadata[GovernanceMarker].(string)
		if tierVal != GovTier1 {
			continue
		}
		original := blocks[i].Text
		compressed := e.CompressRoleInstruction(original)
		if compressed == original {
			continue
		}
		total -= e.EstimateTokens(original) - e.EstimateTokens(compressed)
		blocks[i].Text = compressed
		if total <= budget {
			return blocks, total
		}
	}
	return blocks, total
}

// hardTruncate is the last-resort ceiling: it truncates the largest
// non-protected block to fit the remaining budget. If there are no
// non-protected blocks, it returns the blocks unchanged (we cannot
// trim protected zones).
func (e *PromptBudgetEnforcer) hardTruncate(blocks []schemas.ContentBlock, budget int) []schemas.ContentBlock {
	remaining := budget
	// First pass: subtract protected blocks (they're kept in full).
	for i := range blocks {
		tierVal, _ := blocks[i].Metadata[GovernanceMarker].(string)
		if tierVal == GovTierProtected {
			remaining -= e.EstimateTokens(blocks[i].Text)
		}
	}
	if remaining < 0 {
		// Even protected blocks exceed budget — nothing we can do.
		return blocks
	}
	// Second pass: truncate non-protected blocks to fit.
	for i := range blocks {
		tierVal, _ := blocks[i].Metadata[GovernanceMarker].(string)
		if tierVal == GovTierProtected {
			continue
		}
		tok := e.EstimateTokens(blocks[i].Text)
		if tok <= remaining {
			remaining -= tok
			continue
		}
		// Truncate this block to `remaining` tokens.
		maxChars := remaining * charsPerToken
		if maxChars < 0 {
			maxChars = 0
		}
		if maxChars < len(blocks[i].Text) {
			blocks[i].Text = blocks[i].Text[:maxChars] + "\n[... truncated ...]"
		}
		remaining = 0
	}
	return blocks
}

// logTrim emits a governance event when trimming occurs. This is an
// observability signal, not a control signal (per the cost-governance
// principle: alerting ≠ blocking).
func (e *PromptBudgetEnforcer) logTrim(original, final, budget int, strategy string) {
	if e.logger == nil {
		return
	}
	e.logger.Log(EventCacheInefficiency, "", "", map[string]any{
		"event":           "prompt_budget_trimmed",
		"strategy":        strategy,
		"original_tokens": original,
		"final_tokens":    final,
		"budget_tokens":   budget,
		"reduced_by":      original - final,
	})
}

// ─── Block Tagging Helpers ───────────────────────────────────────────────

// TagBlock returns a copy of block with the governance tier marker set in
// Metadata. Used by PromptAssembler.Build to classify blocks before passing
// to EnforceBudget.
func TagBlock(block schemas.ContentBlock, tier string) schemas.ContentBlock {
	if block.Metadata == nil {
		block.Metadata = make(map[string]any)
	}
	block.Metadata[GovernanceMarker] = tier
	return block
}

// MarkProtected tags a block as a protected zone (never trimmed).
func MarkProtected(block schemas.ContentBlock) schemas.ContentBlock {
	return TagBlock(block, GovTierProtected)
}
