package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// TestPendingSkillGC_ExpiredSkillRemoved verifies that a pending skill whose
// StagedAt + TTL has expired is removed from both disk and PendingSkills.
//
// Regression test for TODO-5: Staging TTL GC.
// See docs/plan/SKILL_MARKETPLACE_INTEGRATION_DESIGN.md TODO-5.
func TestPendingSkillGC_ExpiredSkillRemoved(t *testing.T) {
	// Create a temp skill directory to simulate on-disk skill
	skillDir := t.TempDir()
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("# test"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		PendingSkills: map[string]*config.Skill{
			"ext_test": {
				ID:       "ext_test",
				Name:     "test",
				SkillDir: skillDir,
				// Staged 200 hours ago — exceeds default 168h TTL
				StagedAt: time.Now().Add(-200 * time.Hour),
			},
		},
	}

	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	// Verify removed from PendingSkills
	if _, ok := reg.PendingSkills["ext_test"]; ok {
		t.Error("skill should have been removed from PendingSkills")
	}

	// Verify directory removed from disk
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Error("skill directory should have been removed from disk")
	}
}

// TestPendingSkillGC_NonExpiredSkillPreserved verifies that a pending skill
// within its TTL is NOT removed.
func TestPendingSkillGC_NonExpiredSkillPreserved(t *testing.T) {
	skillDir := t.TempDir()
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("# test"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		PendingSkills: map[string]*config.Skill{
			"ext_fresh": {
				ID:       "ext_fresh",
				Name:     "fresh",
				SkillDir: skillDir,
				// Staged 1 hour ago — well within 168h TTL
				StagedAt: time.Now().Add(-1 * time.Hour),
			},
		},
	}

	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}

	// Verify still in PendingSkills
	skill, ok := reg.PendingSkills["ext_fresh"]
	if !ok {
		t.Fatal("skill should still be in PendingSkills")
	}

	// Verify directory still on disk
	if _, err := os.Stat(skill.SkillDir); os.IsNotExist(err) {
		t.Error("skill directory should still exist on disk")
	}
}

// TestPendingSkillGC_PromotedSkillNotAffected verifies that skills in
// Registry.Skills (promoted) are NOT swept — only PendingSkills is swept.
func TestPendingSkillGC_PromotedSkillNotAffected(t *testing.T) {
	skillDir := t.TempDir()
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("# test"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		// Promoted skill — in Skills, NOT in PendingSkills
		Skills: map[string]*config.Skill{
			"ext_promoted": {
				ID:       "ext_promoted",
				Name:     "promoted",
				SkillDir: skillDir,
				// Even with an old StagedAt, this should NOT be swept
				// because it's in Skills, not PendingSkills
				StagedAt: time.Now().Add(-500 * time.Hour),
			},
		},
		PendingSkills: map[string]*config.Skill{},
	}

	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 0 {
		t.Fatalf("expected 0 removed (promoted skills not swept), got %d", removed)
	}

	// Verify still in Skills
	if _, ok := reg.Skills["ext_promoted"]; !ok {
		t.Error("promoted skill should still be in Skills")
	}

	// Verify directory still on disk
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		t.Error("promoted skill directory should still exist")
	}
}

// TestPendingSkillGC_CustomTTL verifies that StagingRetentionHours from
// SystemSettings is used as the TTL instead of the default 168h.
func TestPendingSkillGC_CustomTTL(t *testing.T) {
	skillDir := t.TempDir()
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("# test"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		System: config.SystemSettings{
			StagingRetentionHours: 2, // 2 hours TTL
		},
		PendingSkills: map[string]*config.Skill{
			"ext_short_ttl": {
				ID:       "ext_short_ttl",
				Name:     "short_ttl",
				SkillDir: skillDir,
				// Staged 3 hours ago — exceeds 2h custom TTL
				StagedAt: time.Now().Add(-3 * time.Hour),
			},
		},
	}

	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 1 {
		t.Fatalf("expected 1 removed with 2h custom TTL, got %d", removed)
	}

	if _, ok := reg.PendingSkills["ext_short_ttl"]; ok {
		t.Error("skill should have been removed with custom TTL")
	}
}

// TestPendingSkillGC_MixedExpiry verifies that in a batch of pending skills,
// only the expired ones are removed while fresh ones are preserved.
func TestPendingSkillGC_MixedExpiry(t *testing.T) {
	freshDir := t.TempDir()
	os.WriteFile(filepath.Join(freshDir, "SKILL.md"), []byte("# fresh"), 0644)

	expiredDir := t.TempDir()
	os.WriteFile(filepath.Join(expiredDir, "SKILL.md"), []byte("# expired"), 0644)

	reg := &config.Registry{
		PendingSkills: map[string]*config.Skill{
			"ext_fresh": {
				ID:       "ext_fresh",
				SkillDir: freshDir,
				StagedAt: time.Now().Add(-10 * time.Hour), // within 168h
			},
			"ext_expired": {
				ID:       "ext_expired",
				SkillDir: expiredDir,
				StagedAt: time.Now().Add(-200 * time.Hour), // exceeds 168h
			},
		},
	}

	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	// Fresh preserved
	if _, ok := reg.PendingSkills["ext_fresh"]; !ok {
		t.Error("fresh skill should still be in PendingSkills")
	}
	if _, err := os.Stat(freshDir); os.IsNotExist(err) {
		t.Error("fresh skill directory should still exist")
	}

	// Expired removed
	if _, ok := reg.PendingSkills["ext_expired"]; ok {
		t.Error("expired skill should have been removed from PendingSkills")
	}
	if _, err := os.Stat(expiredDir); !os.IsNotExist(err) {
		t.Error("expired skill directory should have been removed")
	}
}

// TestPendingSkillGC_ZeroStagedAtSkipped verifies that a skill with zero
// StagedAt (missing timestamp) is skipped, not expired.
func TestPendingSkillGC_ZeroStagedAtSkipped(t *testing.T) {
	skillDir := t.TempDir()
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# test"), 0644)

	reg := &config.Registry{
		PendingSkills: map[string]*config.Skill{
			"ext_no_ts": {
				ID:       "ext_no_ts",
				SkillDir: skillDir,
				StagedAt: time.Time{}, // zero value
			},
		},
	}

	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 0 {
		t.Fatalf("expected 0 removed for zero StagedAt, got %d", removed)
	}

	if _, ok := reg.PendingSkills["ext_no_ts"]; !ok {
		t.Error("skill with zero StagedAt should be preserved")
	}
}
