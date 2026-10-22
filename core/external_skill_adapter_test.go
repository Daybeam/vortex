package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestImportSkillDirectory_RegistersSOPWithIDAndTriggers is a regression test
// for a gap found on 2026-09-11/12: after the SOPTrigger schema migration
// (Triggers single->slice, Steps slice->map), an earlier working-tree state
// of ImportSkillDirectory compiled fine against the new schema but silently
// stopped populating SOP.ID and SOP.Triggers, leaving imported multi-step
// skills with an empty ID and zero triggers -- meaning they would never be
// surfaced by MatchSOPCandidates and would collide under the same failure
// mode found in the real on-disk weekly_macro_arbitrage_report.json (0
// triggers -> never auto-suggested).
func TestImportSkillDirectory_RegistersSOPWithIDAndTriggers(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: My Multi Step Skill\ndescription: Does a thing in steps\n---\n## Step 1\n1. Do the first thing.\n2. Do the second thing.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	n, err := adapter.ImportSkillDirectory(dir)
	if err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 imported skill, got %d", n)
	}

	if len(reg.SOPs) != 1 {
		t.Fatalf("expected 1 SOP registered, got %d", len(reg.SOPs))
	}
	for id, sop := range reg.SOPs {
		if sop.ID == "" {
			t.Errorf("SOP %q registered with empty ID", id)
		}
		if sop.ID != id {
			t.Errorf("SOP.ID %q does not match registry key %q", sop.ID, id)
		}
		if len(sop.Triggers) == 0 {
			t.Errorf("SOP %q registered with zero triggers -- would never be auto-suggested", id)
		}
		if len(sop.Steps) == 0 {
			t.Errorf("SOP %q registered with zero steps", id)
		}
	}
}

// --- scanSkillDir tests ---

func TestScanSkillDir_FindsReferences(t *testing.T) {
	tmpDir := t.TempDir()
	refDir := filepath.Join(tmpDir, "references")
	if err := os.MkdirAll(refDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "guide.md"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "scorecard.md"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	m := scanSkillDir(tmpDir)
	if m == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(m.References) != 2 {
		t.Errorf("expected 2 references, got %d", len(m.References))
	}
}

func TestScanSkillDir_FindsAllCategories(t *testing.T) {
	tmpDir := t.TempDir()

	for _, sub := range []string{"references", "scripts", "assets"} {
		dir := filepath.Join(tmpDir, sub)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	m := scanSkillDir(tmpDir)
	if m == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(m.References) != 1 || len(m.Scripts) != 1 || len(m.Assets) != 1 {
		t.Errorf("expected 1/1/1, got %d/%d/%d",
			len(m.References), len(m.Scripts), len(m.Assets))
	}
}

func TestScanSkillDir_IgnoresNestedDirs(t *testing.T) {
	tmpDir := t.TempDir()
	refDir := filepath.Join(tmpDir, "references")
	nestedDir := filepath.Join(refDir, "nested")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write a file in the nested dir — should be ignored (one level deep only)
	if err := os.WriteFile(filepath.Join(nestedDir, "deep.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// Write a file at the top level — should be found
	if err := os.WriteFile(filepath.Join(refDir, "top.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	m := scanSkillDir(tmpDir)
	if m == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(m.References) != 1 {
		t.Errorf("expected 1 reference (nested dir ignored), got %d: %v",
			len(m.References), m.References)
	}
}

func TestScanSkillDir_EmptyDirReturnsNil(t *testing.T) {
	tmpDir := t.TempDir()
	m := scanSkillDir(tmpDir)
	if m != nil {
		t.Error("expected nil manifest for empty dir")
	}
}

func TestScanSkillDir_NoSkillDirReturnsNil(t *testing.T) {
	m := scanSkillDir("/nonexistent/path/that/does/not/exist")
	if m != nil {
		t.Error("expected nil manifest for non-existent dir")
	}
}

// --- parseProvidesList tests ---

func TestParseProvidesList_Brackets(t *testing.T) {
	result := parseProvidesList("[game, threejs, 3d]")
	if len(result) != 3 {
		t.Fatalf("expected 3 items, got %d: %v", len(result), result)
	}
	expected := []string{"game", "threejs", "3d"}
	for i, v := range expected {
		if result[i] != v {
			t.Errorf("expected[%d]=%s, got %s", i, v, result[i])
		}
	}
}

func TestParseProvidesList_CommaSeparated(t *testing.T) {
	result := parseProvidesList("game, threejs, 3d")
	if len(result) != 3 {
		t.Fatalf("expected 3 items, got %d: %v", len(result), result)
	}
}

func TestParseProvidesList_Empty(t *testing.T) {
	result := parseProvidesList("")
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestParseProvidesList_QuotedValues(t *testing.T) {
	result := parseProvidesList(`["game", "threejs"]`)
	if len(result) != 2 {
		t.Fatalf("expected 2 items, got %d: %v", len(result), result)
	}
	if result[0] != "game" || result[1] != "threejs" {
		t.Errorf("expected game/threejs, got %v", result)
	}
}

// ─── Large skill file handling tests (2026-10-09) ─────────────────────────
//
// These tests verify that a 57KB+ single-file SKILL.md is handled correctly
// without modifying the original file. The system should:
//   1. Import it as ExecutionModeDirectory (metadata-only prompt injection)
//   2. Store the full body as an SOP task (not in the system prompt)
//   3. Produce a small SkillMounter.Activate fragment (metadata only)
//   4. Handle files with very long lines (>64KB) without scanner failure

// TestImportSkillDirectory_LargeSkillFile verifies that a 57KB SKILL.md file
// is imported correctly and registered as ExecutionModeDirectory. The full
// body must go into SOP.Steps[].Task, not into the skill's SystemPrompt.
func TestImportSkillDirectory_LargeSkillFile(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "huge_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Generate a ~57KB body: 1000 lines of ~57 chars each.
	var bodyLines []string
	for i := 0; i < 1000; i++ {
		bodyLines = append(bodyLines, "## Step "+strings.Repeat("x", 50))
	}
	body := strings.Join(bodyLines, "\n")
	frontmatter := "---\nname: Huge Skill\ndescription: A 57KB skill for testing\n---\n"
	content := frontmatter + body

	if size := len(content); size < 50*1024 {
		t.Fatalf("test content too small: %d bytes (need ~57KB)", size)
	}

	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	n, err := adapter.ImportSkillDirectory(dir)
	if err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 imported skill, got %d", n)
	}

	// 1. Verify skill is registered as ExecutionModeDirectory
	var skill *config.Skill
	for _, s := range reg.PendingSkills {
		skill = s
		break
	}
	if skill == nil {
		t.Fatal("no skill registered")
	}
	if skill.ExecutionMode != config.ExecutionModeDirectory {
		t.Errorf("expected ExecutionModeDirectory, got %v", skill.ExecutionMode)
	}

	// 2. Verify the full body is stored in SOP.Steps[].Task, not in Implementations
	if len(reg.SOPs) != 1 {
		t.Fatalf("expected 1 SOP, got %d", len(reg.SOPs))
	}
	for _, sop := range reg.SOPs {
		if len(sop.Steps) != 1 {
			t.Fatalf("expected 1 step, got %d", len(sop.Steps))
		}
		for _, step := range sop.Steps {
			if len(step.Task) < 50*1024 {
				t.Errorf("SOP step task too small: %d bytes (expected ~57KB body)", len(step.Task))
			}
			if !strings.Contains(step.Task, "## Step") {
				t.Error("SOP step task should contain body content")
			}
		}
	}

	// 3. Verify the skill's Implementations do NOT contain the large body
	for _, impl := range skill.Implementations {
		if len(impl.SystemPrompt) > 1000 {
			t.Errorf("skill SystemPrompt should be small (metadata only), got %d bytes", len(impl.SystemPrompt))
		}
	}
}

// TestImportSkillDirectory_LargeSkill_PromptFragmentCapped verifies the L2
// body injection cap (docs/plan/AGENTIC_SKILL_EXPLORATION_DESIGN.md G1):
// SkillMounter.Activate loads the SKILL.md body for a matched skill
// (agentskills.io progressive disclosure level 2), but a 57KB body must be
// truncated to skillBodyPromptCap so it cannot overflow the context — the
// concern pinned by this test since before L2 existed.
func TestImportSkillDirectory_LargeSkill_PromptFragmentCapped(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "big_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Generate a ~57KB body with a unique marker.
	body := strings.Repeat("UNIQUE_MARKER_LINE_CONTENT_HERE\n", 1000)
	content := "---\nname: Big Skill\ndescription: Large skill test\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)
	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatal(err)
	}

	// Find the imported skill and activate it.
	for _, skill := range reg.PendingSkills {
		mounter := &SkillMounter{}
		fragment := mounter.Activate(skill)

		// L2: the instructions ARE loaded (body start present)…
		if !strings.Contains(fragment, "UNIQUE_MARKER_LINE_CONTENT_HERE") {
			t.Error("prompt fragment should contain the (truncated) body content — L2 injection")
		}
		// …but bounded: the fragment must stay far below the full 57KB body
		// (cap + description + section headers + truncation notice overhead).
		if len(fragment) >= skillBodyPromptCap+2000 {
			t.Errorf("prompt fragment too large: %d bytes (expected < cap+2KB overhead)", len(fragment))
		}
		if len(fragment) >= len(body) {
			t.Errorf("prompt fragment must not contain the full body: %d bytes vs %d", len(fragment), len(body))
		}
		// The fragment SHOULD contain the description.
		if !strings.Contains(fragment, "Large skill test") {
			t.Error("prompt fragment should contain skill description")
		}
		// Truncation must be signposted so the agent knows to read the rest.
		if !strings.Contains(fragment, "truncated") {
			t.Error("prompt fragment should contain a truncation notice")
		}
		return
	}
	t.Fatal("no skill found in registry")
}

// TestParseExternalSkillFile_LongLine is a regression test for the scanner
// buffer fix (2026-10-09). The default bufio.NewScanner buffer is 64KB per
// line. A SKILL.md with a single line >64KB (e.g. a minified code block)
// would silently fail. After the fix, it should parse correctly.
func TestParseExternalSkillFile_LongLine(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "long_line_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a file with a single body line >64KB (the default scanner limit).
	longLine := strings.Repeat("A", 70*1024) // 70KB single line
	content := "---\nname: Long Line Skill\ndescription: Has a very long line\n---\n" + longLine
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	meta, err := parseExternalSkillFile(skillPath)
	if err != nil {
		t.Fatalf("parseExternalSkillFile failed on long line: %v", err)
	}
	if meta == nil {
		t.Fatal("expected non-nil meta, got nil (scanner may have silently failed)")
	}
	if meta.Name != "Long Line Skill" {
		t.Errorf("expected name 'Long Line Skill', got %q", meta.Name)
	}
	if len(meta.Body) < 70*1024 {
		t.Errorf("body too small: %d bytes (expected >=70KB)", len(meta.Body))
	}
}

// TestImportSkillDirectory_LargeSkill_BodyInSOPAndTruncatedInPrompt is an
// end-to-end test verifying the complete flow: large SKILL.md → import →
// registry → mount → prompt fragment. The FULL body must be in
// SOP.Steps[].Task; the prompt fragment receives only the capped prefix
// (L2 injection, G1) so the context cannot overflow — the concern this test
// has pinned since before L2 body injection existed.
func TestImportSkillDirectory_LargeSkill_BodyInSOPAndTruncatedInPrompt(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "e2e_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Generate ~57KB of body content with a unique sentinel.
	// Include "## " to trigger SOP registration (ImportSkillDirectory only
	// registers an SOP when the body contains "## " or "1.").
	sentinel := "E2E_SENTINEL_" + strings.Repeat("Z", 200)
	var bodyLines []string
	bodyLines = append(bodyLines, "## Execution Steps")
	for i := 0; i < 200; i++ {
		bodyLines = append(bodyLines, sentinel)
	}
	body := strings.Join(bodyLines, "\n")
	content := "---\nname: E2E Large Skill\ndescription: End-to-end large skill test\n---\n" + body

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 1: Import
	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)
	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatal(err)
	}

	// Step 2: Verify FULL body is in SOP.Steps[].Task
	bodyInSOP := false
	for _, sop := range reg.SOPs {
		for _, step := range sop.Steps {
			if strings.Count(step.Task, sentinel) == 200 {
				bodyInSOP = true
			}
		}
	}
	if !bodyInSOP {
		t.Error("full body should be stored in SOP.Steps[].Task")
	}

	// Step 3: Verify the prompt fragment carries only the truncated prefix
	fragment := ""
	for _, skill := range reg.PendingSkills {
		mounter := &SkillMounter{}
		fragment = mounter.Activate(skill)
		break
	}
	if fragment == "" {
		t.Fatal("no skill activated")
	}
	got := strings.Count(fragment, sentinel)
	if got == 0 {
		t.Error("fragment should contain the truncated body prefix (L2 injection)")
	}
	if got >= 200 {
		t.Errorf("fragment must not contain the full body: %d sentinel occurrences (would overflow context)", got)
	}
	if len(fragment) >= skillBodyPromptCap+2000 {
		t.Errorf("fragment too large: %d bytes (expected < cap+2KB overhead)", len(fragment))
	}
}

// ─── deriveCapability tests (TODO-1: remove hardcoded "external_discipline") ──
//
// Regression test for SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-1.
// Before this fix, ImportSkillDirectory hardcoded Capability="external_discipline"
// for all external skills, causing SemanticRetriever to return every external
// skill on any query matching "external" or "discipline" (noise, not signal).
// After the fix, capability is derived from frontmatter or description.

func TestDeriveCapability_ExplicitFrontmatter(t *testing.T) {
	meta := &ExternalSkillMeta{Capability: "futures-trading"}
	if got := deriveCapability(meta); got != "futures-trading" {
		t.Errorf("expected 'futures-trading', got %q", got)
	}
}

func TestDeriveCapability_FromDescription(t *testing.T) {
	meta := &ExternalSkillMeta{Description: "Analyzes futures market data and positions"}
	if got := deriveCapability(meta); got != "analyzes" {
		t.Errorf("expected 'analyzes' (first word ≥3 chars), got %q", got)
	}
}

func TestDeriveCapability_FallbackToExternal(t *testing.T) {
	meta := &ExternalSkillMeta{}
	if got := deriveCapability(meta); got != "external" {
		t.Errorf("expected 'external' fallback, got %q", got)
	}
}

func TestDeriveCapability_NotExternalDiscipline(t *testing.T) {
	// The old hardcoded value must never appear as the derived capability
	// unless the frontmatter explicitly says so.
	meta := &ExternalSkillMeta{Description: "Some skill description"}
	if got := deriveCapability(meta); got == "external_discipline" {
		t.Error("deriveCapability must not return 'external_discipline' — that was the bug")
	}
}

func TestImportSkillDirectory_CapabilityNotHardcoded(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "futures_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Futures Analyzer\ndescription: Analyzes futures market data\n---\n## Step 1\n1. Do analysis.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}

	for id, skill := range reg.PendingSkills {
		if skill.Capability == "external_discipline" {
			t.Errorf("skill %q still has hardcoded Capability 'external_discipline' — TODO-1 not fixed", id)
		}
		if skill.Capability == "" {
			t.Errorf("skill %q has empty Capability", id)
		}
	}
}

func TestImportSkillDirectory_ExplicitCapabilityFromFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "custom_cap_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Custom Cap Skill\ndescription: Some description\ncapability: data-analysis\n---\n## Step 1\n1. Do work.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}

	for _, skill := range reg.PendingSkills {
		if skill.Capability != "data-analysis" {
			t.Errorf("expected Capability 'data-analysis' from frontmatter, got %q", skill.Capability)
		}
	}
}

// ─── TODO-4: Pending state + promote gate tests ──────────────────────────────
//
// Regression tests for SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-4.
// External skills go to PendingSkills (not Skills), making them invisible to
// CapabilityIndex and all retrievers. They must be explicitly promoted.

func TestImportSkillDirectory_GoesToPendingNotActive(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Test Skill\ndescription: A test skill\n---\n## Step 1\n1. Do something.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}

	// Skill should be in PendingSkills, NOT in Skills
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}
	if len(reg.Skills) != 0 {
		t.Errorf("expected 0 active skills (should all be pending), got %d", len(reg.Skills))
	}
}

func TestImportSkillDirectory_PendingSkillNotInCapabilityIndex(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "indexed_skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Indexed Skill\ndescription: Should not be discoverable until promoted\ncapability: data-analysis\n---\n## Step 1\n1. Analyze.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}

	// Build capability index — pending skill should NOT appear
	idx := NewCapabilityIndex(reg)
	if idx.Tags["data-analysis"] {
		t.Error("pending skill's capability 'data-analysis' should NOT be in CapabilityIndex tags")
	}
}

func TestPromoteSkill_MovesFromPendingToActive(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "promote_me")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Promote Me\ndescription: Skill to be promoted\ncapability: trading\n---\n## Step 1\n1. Trade.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{}
	adapter := NewExternalSkillAdapter(reg)

	if _, err := adapter.ImportSkillDirectory(dir); err != nil {
		t.Fatalf("ImportSkillDirectory failed: %v", err)
	}

	// Find the pending skill ID
	var skillID string
	for id := range reg.PendingSkills {
		skillID = id
		break
	}
	if skillID == "" {
		t.Fatal("no pending skill found")
	}

	// Promote: move from PendingSkills to Skills
	reg.Mu.Lock()
	skill := reg.PendingSkills[skillID]
	if reg.Skills == nil {
		reg.Skills = make(map[string]*config.Skill)
	}
	reg.Skills[skillID] = skill
	delete(reg.PendingSkills, skillID)
	reg.Mu.Unlock()

	// Verify: skill is now in Skills, not in PendingSkills
	if len(reg.PendingSkills) != 0 {
		t.Errorf("expected 0 pending skills after promotion, got %d", len(reg.PendingSkills))
	}
	if len(reg.Skills) != 1 {
		t.Fatalf("expected 1 active skill after promotion, got %d", len(reg.Skills))
	}

	// Verify: skill is now in CapabilityIndex
	idx := NewCapabilityIndex(reg)
	if !idx.Tags["trading"] {
		t.Error("promoted skill's capability 'trading' should be in CapabilityIndex tags")
	}
}

// ─── agentskills.io standard adaptation (docs/plan/AGENTIC_SKILL_EXPLORATION_DESIGN.md) ──

// TestParseExternalSkillFile_StandardFields verifies G2/G3/G6: compatibility,
// allowed-tools (space-separated), license, and the nested metadata map are
// parsed from frontmatter.
func TestParseExternalSkillFile_StandardFields(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "pdf-extraction")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" +
		"name: pdf-extraction\n" +
		"description: Extracts tables from PDF files\n" +
		"license: MIT\n" +
		"compatibility: Requires Python 3.14+ and uv\n" +
		"allowed-tools: read_file write_file run_command\n" +
		"metadata:\n" +
		"  author: someone\n" +
		"  version: \"1.2\"\n" +
		"---\nDo the thing."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	meta, err := parseExternalSkillFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Compatibility != "Requires Python 3.14+ and uv" {
		t.Errorf("Compatibility = %q", meta.Compatibility)
	}
	if len(meta.AllowedTools) != 3 || meta.AllowedTools[0] != "read_file" || meta.AllowedTools[2] != "run_command" {
		t.Errorf("AllowedTools = %v", meta.AllowedTools)
	}
	if meta.License != "MIT" {
		t.Errorf("License = %q", meta.License)
	}
	if meta.Metadata == nil || meta.Metadata["author"] != "someone" || meta.Metadata["version"] != "1.2" {
		t.Errorf("Metadata = %v", meta.Metadata)
	}
	if meta.Body != "Do the thing." {
		t.Errorf("Body = %q", meta.Body)
	}
}

// TestParseExternalSkillFile_MetadataThenTopLevelKey ensures a top-level key
// after a nested metadata block is still parsed (state machine exits the map).
func TestParseExternalSkillFile_MetadataThenTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "meta-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" +
		"name: meta-skill\n" +
		"metadata:\n" +
		"  key: value\n" +
		"capability: after-meta\n" +
		"---\nBody."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	meta, err := parseExternalSkillFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Metadata["key"] != "value" {
		t.Errorf("Metadata = %v", meta.Metadata)
	}
	if meta.Capability != "after-meta" {
		t.Errorf("Capability = %q (top-level key after metadata block was swallowed?)", meta.Capability)
	}
}

// TestValidateStandardSkillMeta covers G5 for the strict standard import path.
func TestValidateStandardSkillMeta(t *testing.T) {
	valid := &ExternalSkillMeta{Name: "pdf-extraction", Description: "d"}

	cases := []struct {
		name string
		meta *ExternalSkillMeta
		dir  string
		ok   bool
	}{
		{"valid", valid, "pdf-extraction", true},
		{"name-dir mismatch", &ExternalSkillMeta{Name: "other-name", Description: "d"}, "pdf-extraction", false},
		{"uppercase name", &ExternalSkillMeta{Name: "PDF-Extraction", Description: "d"}, "PDF-Extraction", false},
		{"space in name", &ExternalSkillMeta{Name: "pdf extraction", Description: "d"}, "pdf extraction", false},
		{"leading hyphen", &ExternalSkillMeta{Name: "-pdf", Description: "d"}, "-pdf", false},
		{"double hyphen", &ExternalSkillMeta{Name: "pdf--x", Description: "d"}, "pdf--x", false},
		{"name too long", &ExternalSkillMeta{Name: strings.Repeat("a", 65), Description: "d"}, strings.Repeat("a", 65), false},
		{"empty description", &ExternalSkillMeta{Name: "pdf-extraction"}, "pdf-extraction", false},
		{"description too long", &ExternalSkillMeta{Name: "pdf-extraction", Description: strings.Repeat("d", 1025)}, "pdf-extraction", false},
	}
	for _, tc := range cases {
		path := filepath.Join("root", tc.dir, "SKILL.md")
		err := validateStandardSkillMeta(tc.meta, path)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}

// TestImportStandardSkills_StrictSkipsNonCompliant verifies G5 wiring: the
// strict path skips non-compliant skills (with a warning) but imports
// compliant ones; the lenient ImportSkillDirectory still imports both.
func TestImportStandardSkills_StrictSkipsNonCompliant(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"good-skill", "Bad Skill"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill := func(sub, name string) {
		content := "---\nname: " + name + "\ndescription: d\n---\n## S\n1. x."
		if err := os.WriteFile(filepath.Join(dir, sub, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill("good-skill", "good-skill")
	writeSkill("Bad Skill", "Bad Skill")

	strictReg := &config.Registry{}
	n, err := NewExternalSkillAdapter(strictReg).ImportStandardSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("strict import count = %d, want 1", n)
	}
	if _, ok := strictReg.PendingSkills["ext_good_skill"]; !ok {
		t.Error("compliant skill missing from PendingSkills")
	}
	if len(strictReg.PendingSkills) != 1 {
		t.Errorf("non-compliant skill should be skipped, PendingSkills = %d", len(strictReg.PendingSkills))
	}

	lenientReg := &config.Registry{}
	n, err = NewExternalSkillAdapter(lenientReg).ImportSkillDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("lenient import count = %d, want 2 (legacy path tolerates non-standard names)", n)
	}
}

// TestImportStandardDiscoveryPaths verifies G4: project and user level
// .agents/skills/ directories are scanned; missing directories are a no-op.
func TestImportStandardDiscoveryPaths(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	// Distinct skill names per level: identical names dedup to one registry entry.
	for _, spec := range []struct{ base, name string }{
		{project, "proj-skill"},
		{user, "user-skill"},
	} {
		skillDir := filepath.Join(spec.base, ".agents", "skills", spec.name)
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + spec.name + "\ndescription: d\n---\n## S\n1. x."
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	reg := &config.Registry{}
	if n := ImportStandardDiscoveryPaths(reg, project, user); n != 2 {
		t.Errorf("imported = %d, want 2", n)
	}
	if len(reg.PendingSkills) != 2 {
		t.Errorf("PendingSkills = %d, want 2", len(reg.PendingSkills))
	}

	// Missing dirs → zero, no panic, no error surfaced.
	reg2 := &config.Registry{}
	if n := ImportStandardDiscoveryPaths(reg2, filepath.Join(project, "nope"), ""); n != 0 {
		t.Errorf("imported = %d, want 0 for missing dirs", n)
	}
}
