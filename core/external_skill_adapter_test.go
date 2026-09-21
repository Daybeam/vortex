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
