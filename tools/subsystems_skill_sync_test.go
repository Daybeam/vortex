package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
)

// TestSyncSkills_TarballPreservesDirectoryStructure verifies that sync_skills
// downloads the full repo tarball (not just .md files), preserving scripts/,
// assets/, and references/ directories — fixing Gap 3.1 from
// SKILL_MARKETPLACE_REPO_CONTRACT_DESIGN.md.
func TestSyncSkills_TarballPreservesDirectoryStructure(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	skillContent := "---\nname: Dir Skill\ndescription: A directory-mode skill\n---\n# Dir Skill\nUses scripts."

	entries := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skills/dir-skill/", "", tar.TypeDir},
		{"repo-main/skills/dir-skill/SKILL.md", skillContent, tar.TypeReg},
		{"repo-main/skills/dir-skill/scripts/run.py", "print('hello')", tar.TypeReg},
		{"repo-main/skills/dir-skill/assets/logo.png", "PNGBYTES", tar.TypeReg},
		{"repo-main/skills/dir-skill/references/guide.md", "# Guide", tar.TypeReg},
	}

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		tw.WriteHeader(hdr)
		if e.typeflag == tar.TypeReg {
			tw.Write([]byte(e.content))
		}
	}
	tw.Close()
	gzw.Close()

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer codeloadSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	defer apiSrv.Close()

	loader := core.NewResourceLoader()
	loader.SetGitHubBases(apiSrv.URL, "http://localhost", codeloadSrv.URL)

	rootDir := t.TempDir()
	app := &App{
		Registry: &config.Registry{
			Skills:        make(map[string]*config.Skill),
			PendingSkills: make(map[string]*config.Skill),
		},
		Tier:           TierAdmin,
		ResourceLoader: loader,
		RootDir:        rootDir,
	}
	RegisterSubsystems(app)

	result, err := registry.Invoke(context.Background(), app, "admin", "sync_skills", map[string]any{
		"owner": "test",
		"repo":  "repo",
	})
	if err != nil {
		t.Fatalf("sync_skills failed: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if m["imported_count"].(int) < 1 {
		t.Fatalf("expected at least 1 imported skill, got %v", m["imported_count"])
	}

	// Verify the skill has a SkillDir pointing to the persistent cache
	for _, skill := range app.Registry.PendingSkills {
		if skill.SkillDir == "" {
			t.Error("skill SkillDir is empty — should point to persistent cache")
			continue
		}
		// SkillDir should point to a directory that still exists
		if _, err := os.Stat(skill.SkillDir); err != nil {
			t.Errorf("skill SkillDir %s does not exist after handler return: %v", skill.SkillDir, err)
		}
		// SKILL.md should be readable
		if _, err := os.Stat(filepath.Join(skill.SkillDir, "SKILL.md")); err != nil {
			t.Errorf("SKILL.md not found in SkillDir %s: %v", skill.SkillDir, err)
		}
		// Scripts should be preserved (Gap 3.1 fix)
		scriptsDir := filepath.Join(skill.SkillDir, "scripts")
		if _, err := os.Stat(filepath.Join(scriptsDir, "run.py")); err != nil {
			t.Errorf("scripts/run.py not found in SkillDir %s: %v", skill.SkillDir, err)
		}
		// Assets should be preserved (Gap 3.1 fix)
		assetsDir := filepath.Join(skill.SkillDir, "assets")
		if _, err := os.Stat(filepath.Join(assetsDir, "logo.png")); err != nil {
			t.Errorf("assets/logo.png not found in SkillDir %s: %v", skill.SkillDir, err)
		}
		// References should be preserved (Gap 3.1 fix)
		referencesDir := filepath.Join(skill.SkillDir, "references")
		if _, err := os.Stat(filepath.Join(referencesDir, "guide.md")); err != nil {
			t.Errorf("references/guide.md not found in SkillDir %s: %v", skill.SkillDir, err)
		}
	}
}

// TestSyncSkills_PromotedSkillReadsBody verifies that after sync_skills +
// promote_skill, the skill's SKILL.md body can be read via readSkillBody —
// fixing Gap 3.2 (SkillDir dangling → silent failure on promote).
func TestSyncSkills_PromotedSkillReadsBody(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	skillContent := "---\nname: Promote Test\ndescription: Test promote after sync\n---\n# Promote Test\nBody content here."

	entries := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skills/promote-test/", "", tar.TypeDir},
		{"repo-main/skills/promote-test/SKILL.md", skillContent, tar.TypeReg},
	}

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		tw.WriteHeader(hdr)
		if e.typeflag == tar.TypeReg {
			tw.Write([]byte(e.content))
		}
	}
	tw.Close()
	gzw.Close()

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer codeloadSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	defer apiSrv.Close()

	loader := core.NewResourceLoader()
	loader.SetGitHubBases(apiSrv.URL, "http://localhost", codeloadSrv.URL)

	rootDir := t.TempDir()
	app := &App{
		Registry: &config.Registry{
			Skills:        make(map[string]*config.Skill),
			PendingSkills: make(map[string]*config.Skill),
		},
		Tier:           TierAdmin,
		ResourceLoader: loader,
		RootDir:        rootDir,
	}
	RegisterSubsystems(app)

	// 1. Sync skills
	_, err := registry.Invoke(context.Background(), app, "admin", "sync_skills", map[string]any{
		"owner": "test",
		"repo":  "repo",
	})
	if err != nil {
		t.Fatalf("sync_skills failed: %v", err)
	}

	if len(app.Registry.PendingSkills) == 0 {
		t.Fatal("expected pending skills after sync")
	}

	// 2. Promote the skill
	var skillID string
	for id := range app.Registry.PendingSkills {
		skillID = id
		break
	}

	_, err = registry.Invoke(context.Background(), app, "admin", "promote_skill", map[string]any{
		"skill_id": skillID,
	})
	if err != nil {
		t.Fatalf("promote_skill failed: %v", err)
	}

	// 3. Verify the promoted skill's SkillDir still exists and SKILL.md is readable
	// This is the core of Gap 3.2: before the fix, SkillDir pointed to a deleted
	// tempDir, and readSkillBody returned empty → silent failure.
	skill := app.Registry.Skills[skillID]
	if skill == nil {
		t.Fatal("promoted skill not found in Skills registry")
	}
	if skill.SkillDir == "" {
		t.Fatal("promoted skill has empty SkillDir")
	}
	if _, err := os.Stat(skill.SkillDir); err != nil {
		t.Fatalf("promoted skill SkillDir %s does not exist (Gap 3.2): %v", skill.SkillDir, err)
	}
	skillMD := filepath.Join(skill.SkillDir, "SKILL.md")
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("SKILL.md not found at promoted skill SkillDir %s (Gap 3.2): %v", skill.SkillDir, err)
	}

	// 4. Verify readSkillBody returns non-empty content
	mounter := &core.SkillMounter{}
	body := mounter.Activate(skill)
	if body == "" {
		t.Error("SkillMounter.Activate returned empty body — SkillDir may be dangling (Gap 3.2)")
	}
}
