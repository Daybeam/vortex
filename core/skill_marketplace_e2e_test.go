package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// ─── PendingSkillGC lifecycle tests (TODO-5) ────────────────────────────────

// TestPendingSkillGC_StartStop verifies that Start() launches the goroutine
// and Stop() exits cleanly without hanging.
func TestPendingSkillGC_StartStop(t *testing.T) {
	reg := &config.Registry{
		PendingSkills: make(map[string]*config.Skill),
	}
	gc := NewPendingSkillGC(reg)

	// Start should launch the goroutine
	gc.Start()

	// Stop should exit cleanly — if it hangs, the test will timeout
	done := make(chan struct{})
	go func() {
		gc.Stop()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung — goroutine did not exit")
	}
}

// TestPendingSkillGC_StopIdempotent verifies that calling Stop() multiple
// times does not panic (close on closed channel).
func TestPendingSkillGC_StopIdempotent(t *testing.T) {
	reg := &config.Registry{
		PendingSkills: make(map[string]*config.Skill),
	}
	gc := NewPendingSkillGC(reg)
	gc.Start()

	// Multiple stops should not panic
	gc.Stop()
	gc.Stop()
	gc.Stop()
}

// TestPendingSkillGC_StopWithoutStart verifies that Stop() is safe to call
// even if Start() was never called.
func TestPendingSkillGC_StopWithoutStart(t *testing.T) {
	reg := &config.Registry{
		PendingSkills: make(map[string]*config.Skill),
	}
	gc := NewPendingSkillGC(reg)

	// Should not panic
	gc.Stop()
}

// ─── E2E: import → pending → TTL expires → GC removes ───────────────────────

// TestE2E_ImportTTLExpireGCRemoves tests the full flow:
// 1. Create a SKILL.md on disk
// 2. ImportSkillDirectory → goes to PendingSkills with StagedAt = now
// 3. Manually age the StagedAt to exceed TTL
// 4. Run GC sweep → skill removed from PendingSkills + directory deleted
func TestE2E_ImportTTLExpireGCRemoves(t *testing.T) {
	skillDir := t.TempDir()
	skillContent := "---\nname: TTL Test\ndescription: Skill that will expire\n---\n# TTL Test"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		PendingSkills: make(map[string]*config.Skill),
	}

	// 1. Import
	adapter := NewExternalSkillAdapter(reg)
	imported, err := adapter.ImportSkillDirectory(skillDir)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if imported != 1 {
		t.Fatalf("expected 1 imported, got %d", imported)
	}

	// 2. Verify in PendingSkills
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}

	// 3. Age the StagedAt to exceed default 168h TTL
	for _, skill := range reg.PendingSkills {
		skill.StagedAt = time.Now().Add(-200 * time.Hour)
	}

	// 4. Run GC sweep
	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	// 5. Verify removed from PendingSkills
	if len(reg.PendingSkills) != 0 {
		t.Error("pending skills should be empty after GC")
	}

	// 6. Verify directory removed from disk
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Error("skill directory should have been removed from disk")
	}
}

// ─── E2E: import → pending → promote → GC runs → promoted survives ──────────

// TestE2E_ImportPromoteGCSurvives tests the full flow:
// 1. Create a SKILL.md on disk
// 2. ImportSkillDirectory → goes to PendingSkills
// 3. Promote: move from PendingSkills to Skills (simulating promote_skill)
// 4. Age the StagedAt to exceed TTL
// 5. Run GC sweep → promoted skill is NOT removed (it's in Skills, not PendingSkills)
func TestE2E_ImportPromoteGCSurvives(t *testing.T) {
	skillDir := t.TempDir()
	skillContent := "---\nname: Survivor\ndescription: Skill that will be promoted\n---\n# Survivor"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		Skills:        make(map[string]*config.Skill),
		PendingSkills: make(map[string]*config.Skill),
	}

	// 1. Import
	adapter := NewExternalSkillAdapter(reg)
	if _, err := adapter.ImportSkillDirectory(skillDir); err != nil {
		t.Fatalf("import failed: %v", err)
	}

	// 2. Get the skill ID
	var skillID string
	for id := range reg.PendingSkills {
		skillID = id
		break
	}

	// 3. Promote: move from PendingSkills to Skills
	reg.Mu.Lock()
	skill := reg.PendingSkills[skillID]
	reg.Skills[skillID] = skill
	delete(reg.PendingSkills, skillID)
	reg.Mu.Unlock()

	// 4. Age the StagedAt to exceed TTL — but it's in Skills now, not PendingSkills
	skill.StagedAt = time.Now().Add(-500 * time.Hour)

	// 5. Run GC sweep
	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 0 {
		t.Fatalf("expected 0 removed (promoted skill should survive), got %d", removed)
	}

	// 6. Verify skill still in Skills
	if _, ok := reg.Skills[skillID]; !ok {
		t.Error("promoted skill should still be in Skills")
	}

	// 7. Verify directory still exists
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		t.Error("promoted skill directory should still exist")
	}
}

// ─── E2E: import multiple → promote one → GC removes only expired pending ────

// TestE2E_MixedPendingPromotedGC tests a mixed scenario:
// 1. Import two skills → both in PendingSkills
// 2. Promote one → moves to Skills
// 3. Age the remaining pending skill past TTL
// 4. Run GC → only the expired pending skill is removed; promoted survives
func TestE2E_MixedPendingPromotedGC(t *testing.T) {
	// Create two skill directories
	dir1 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "SKILL.md"), []byte("---\nname: Skill One\n---\n# One"), 0644)

	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "SKILL.md"), []byte("---\nname: Skill Two\n---\n# Two"), 0644)

	reg := &config.Registry{
		Skills:        make(map[string]*config.Skill),
		PendingSkills: make(map[string]*config.Skill),
	}

	// Import both
	adapter := NewExternalSkillAdapter(reg)
	adapter.ImportSkillDirectory(dir1)
	adapter.ImportSkillDirectory(dir2)

	if len(reg.PendingSkills) != 2 {
		t.Fatalf("expected 2 pending skills, got %d", len(reg.PendingSkills))
	}

	// Promote the first one
	var promotedID, pendingID string
	for id, skill := range reg.PendingSkills {
		if promotedID == "" {
			promotedID = id
			reg.Mu.Lock()
			reg.Skills[id] = skill
			delete(reg.PendingSkills, id)
			reg.Mu.Unlock()
		} else {
			pendingID = id
		}
	}

	// Age the remaining pending skill past TTL
	reg.PendingSkills[pendingID].StagedAt = time.Now().Add(-200 * time.Hour)

	// Run GC
	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 1 {
		t.Fatalf("expected 1 removed (expired pending), got %d", removed)
	}

	// Promoted skill survives
	if _, ok := reg.Skills[promotedID]; !ok {
		t.Error("promoted skill should survive GC")
	}

	// Expired pending skill removed
	if _, ok := reg.PendingSkills[pendingID]; ok {
		t.Error("expired pending skill should be removed")
	}
}
