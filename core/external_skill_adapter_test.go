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
	for _, s := range reg.Skills {
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

// TestImportSkillDirectory_LargeSkill_PromptFragmentSmall verifies that
// SkillMounter.Activate produces a small prompt fragment for a large
// directory-mode skill. The fragment should contain only metadata
// (name + description + manifest), NOT the full 57KB body.
func TestImportSkillDirectory_LargeSkill_PromptFragmentSmall(t *testing.T) {
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
	for _, skill := range reg.Skills {
		mounter := &SkillMounter{}
		fragment := mounter.Activate(skill)

		// The fragment should be small (metadata only).
		if len(fragment) > 500 {
			t.Errorf("prompt fragment too large: %d bytes (expected metadata only, <500)", len(fragment))
		}

		// The fragment must NOT contain the body content.
		if strings.Contains(fragment, "UNIQUE_MARKER_LINE_CONTENT_HERE") {
			t.Error("prompt fragment should NOT contain the full body content")
		}

		// The fragment SHOULD contain the description.
		if !strings.Contains(fragment, "Large skill test") {
			t.Error("prompt fragment should contain skill description")
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

// TestImportSkillDirectory_LargeSkill_BodyInSOPNotInPrompt is an end-to-end
// test verifying the complete flow: large SKILL.md → import → registry →
// mount → prompt fragment. The body must be in SOP.Steps[].Task and must
// NOT appear in the mounter's prompt fragment.
func TestImportSkillDirectory_LargeSkill_BodyInSOPNotInPrompt(t *testing.T) {
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

	// Step 2: Verify body is in SOP.Steps[].Task
	bodyInSOP := false
	for _, sop := range reg.SOPs {
		for _, step := range sop.Steps {
			if strings.Contains(step.Task, sentinel) {
				bodyInSOP = true
			}
		}
	}
	if !bodyInSOP {
		t.Error("body should be stored in SOP.Steps[].Task")
	}

	// Step 3: Verify body is NOT in the prompt fragment
	bodyInPrompt := false
	for _, skill := range reg.Skills {
		mounter := &SkillMounter{}
		fragment := mounter.Activate(skill)
		if strings.Contains(fragment, sentinel) {
			bodyInPrompt = true
		}
	}
	if bodyInPrompt {
		t.Error("body should NOT appear in the prompt fragment (would overflow context)")
	}
}
