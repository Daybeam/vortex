package core

import (
	"os"
	"path/filepath"
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
