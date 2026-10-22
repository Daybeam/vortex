package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
)

// initSkillTestApp creates a minimal App for testing skill-related actions.
func initSkillTestApp() *App {
	reg := &config.Registry{
		Skills:        make(map[string]*config.Skill),
		PendingSkills: make(map[string]*config.Skill),
	}
	app := &App{
		Registry: reg,
		Tier:     TierAdmin, // Admin tier — bypasses action allowlist
	}
	// Register subsystems so actions are available via registry.Invoke
	RegisterSubsystems(app)
	return app
}

// ─── sync_skills action tests (TODO-3) ──────────────────────────────────────

// TestSyncSkills_MissingOwner verifies the error path when owner is empty.
func TestSyncSkills_MissingOwner(t *testing.T) {
	app := initSkillTestApp()
	_, err := registry.Invoke(context.Background(), app, "admin", "sync_skills", map[string]any{
		"repo": "skills",
	})
	if err == nil {
		t.Fatal("expected error for missing owner")
	}
}

// TestSyncSkills_MissingRepo verifies the error path when repo is empty.
func TestSyncSkills_MissingRepo(t *testing.T) {
	app := initSkillTestApp()
	_, err := registry.Invoke(context.Background(), app, "admin", "sync_skills", map[string]any{
		"owner": "mattpocock",
	})
	if err == nil {
		t.Fatal("expected error for missing repo")
	}
}

// TestSyncSkills_MissingBoth verifies the error path when both are empty.
func TestSyncSkills_MissingBoth(t *testing.T) {
	app := initSkillTestApp()
	_, err := registry.Invoke(context.Background(), app, "admin", "sync_skills", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing owner and repo")
	}
}

// ─── promote_skill action tests (TODO-4) ────────────────────────────────────

// TestPromoteSkill_ActionSuccess tests the real promote_skill action handler
// (not simulated map manipulation). Verifies the skill moves from PendingSkills
// to Skills and becomes discoverable via Navigator.Refresh().
func TestPromoteSkill_ActionSuccess(t *testing.T) {
	app := initSkillTestApp()

	// Seed a pending skill
	skillID := "ext_test_skill"
	app.Registry.PendingSkills[skillID] = &config.Skill{
		ID:          skillID,
		Name:        "Test Skill",
		Capability:  "testing",
		Description: "A test skill",
		StagedAt:    time.Now(),
	}

	// Set up Navigator so Refresh() can be called
	app.Navigator = core.NewCapabilityNavigator(app.Registry)

	// Invoke the real action handler
	result, err := registry.Invoke(context.Background(), app, "admin", "promote_skill", map[string]any{
		"skill_id": skillID,
	})
	if err != nil {
		t.Fatalf("promote_skill failed: %v", err)
	}

	// Verify result shape
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if m["promoted_skill"] != skillID {
		t.Errorf("expected promoted_skill=%q, got %v", skillID, m["promoted_skill"])
	}

	// Verify skill moved from PendingSkills to Skills
	if _, ok := app.Registry.PendingSkills[skillID]; ok {
		t.Error("skill should have been removed from PendingSkills")
	}
	if skill, ok := app.Registry.Skills[skillID]; !ok {
		t.Error("skill should be in Skills after promotion")
	} else if skill.Name != "Test Skill" {
		t.Errorf("wrong skill name: got %q", skill.Name)
	}
}

// TestPromoteSkill_ActionNotFound tests the error path when the skill_id
// doesn't exist in PendingSkills.
func TestPromoteSkill_ActionNotFound(t *testing.T) {
	app := initSkillTestApp()

	_, err := registry.Invoke(context.Background(), app, "admin", "promote_skill", map[string]any{
		"skill_id": "ext_nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for non-existent skill")
	}
}

// TestPromoteSkill_ActionEmptyID tests the error path when skill_id is empty.
func TestPromoteSkill_ActionEmptyID(t *testing.T) {
	app := initSkillTestApp()

	_, err := registry.Invoke(context.Background(), app, "admin", "promote_skill", map[string]any{
		"skill_id": "",
	})
	if err == nil {
		t.Fatal("expected error for empty skill_id")
	}
}

// TestPromoteSkill_ActionMakesDiscoverable verifies that after promotion,
// the skill's capability appears in the CapabilityIndex — proving the
// Navigator.Refresh() call in the action handler works.
func TestPromoteSkill_ActionMakesDiscoverable(t *testing.T) {
	app := initSkillTestApp()

	skillID := "ext_discoverable"
	capability := "data-analysis"
	app.Registry.PendingSkills[skillID] = &config.Skill{
		ID:         skillID,
		Name:       "Data Analysis Skill",
		Capability: capability,
		StagedAt:   time.Now(),
	}

	app.Navigator = core.NewCapabilityNavigator(app.Registry)

	// Before promotion: capability should NOT be in index
	app.Navigator.Refresh()
	if app.Navigator.Index.Tags[capability] {
		t.Fatal("capability should NOT be discoverable before promotion")
	}

	// Promote
	_, err := registry.Invoke(context.Background(), app, "admin", "promote_skill", map[string]any{
		"skill_id": skillID,
	})
	if err != nil {
		t.Fatalf("promote failed: %v", err)
	}

	// After promotion: capability SHOULD be in index
	if !app.Navigator.Index.Tags[capability] {
		t.Error("capability should be discoverable after promotion")
	}
}

// ─── E2E: import → pending → promote → discoverable ─────────────────────────

// TestE2E_ImportPromoteDiscoverable tests the full flow:
// 1. Create a SKILL.md on disk
// 2. ImportSkillDirectory → goes to PendingSkills (not Skills)
// 3. promote_skill action → moves to Skills
// 4. CapabilityIndex → skill's capability is discoverable
func TestE2E_ImportPromoteDiscoverable(t *testing.T) {
	app := initSkillTestApp()
	app.Navigator = core.NewCapabilityNavigator(app.Registry)

	// 1. Create a skill directory with SKILL.md
	skillDir := t.TempDir()
	skillContent := "---\nname: E2E Skill\ndescription: End-to-end test skill\ncapability: e2e-testing\n---\n# E2E Skill\nThis is a test."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Import → should go to PendingSkills
	adapter := core.NewExternalSkillAdapter(app.Registry)
	imported, err := adapter.ImportSkillDirectory(skillDir)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if imported != 1 {
		t.Fatalf("expected 1 imported, got %d", imported)
	}
	if len(app.Registry.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(app.Registry.PendingSkills))
	}
	if len(app.Registry.Skills) != 0 {
		t.Error("no skills should be in Skills before promotion")
	}

	// 3. Promote via real action handler
	var skillID string
	for id := range app.Registry.PendingSkills {
		skillID = id
		break
	}
	_, err = registry.Invoke(context.Background(), app, "admin", "promote_skill", map[string]any{
		"skill_id": skillID,
	})
	if err != nil {
		t.Fatalf("promote failed: %v", err)
	}

	// 4. Verify discoverable in CapabilityIndex
	if !app.Navigator.Index.Tags["e2e-testing"] {
		t.Error("capability 'e2e-testing' should be discoverable after promotion")
	}
	if len(app.Registry.Skills) != 1 {
		t.Errorf("expected 1 active skill, got %d", len(app.Registry.Skills))
	}
	if len(app.Registry.PendingSkills) != 0 {
		t.Errorf("expected 0 pending skills, got %d", len(app.Registry.PendingSkills))
	}
}
