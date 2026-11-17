package core

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
)

// makeTarball builds an in-memory tar.gz with the given entries.
// Each entry's name should include the top-level "repo-main/" prefix
// (which FetchArchive strips during extraction).
func makeTarball(t *testing.T, entries []struct {
	name     string
	content  string
	typeflag byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gzw.Close()
	return buf.Bytes()
}

// newArchiveTestServers creates httptest servers for the codeload (tarball)
// and API (default branch lookup) endpoints, returning the server URLs and
// a cleanup function.
func newArchiveTestServers(t *testing.T, tarballData []byte) (codeloadURL, apiURL string, cleanup func()) {
	t.Helper()
	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(tarballData)
	}))
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	cleanup = func() {
		codeloadSrv.Close()
		apiSrv.Close()
	}
	return codeloadSrv.URL, apiSrv.URL, cleanup
}

func newArchiveTestLoader(codeloadURL, apiURL string) *ResourceLoader {
	return &ResourceLoader{
		client:            http.DefaultClient,
		cache:             make(map[string]string),
		githubAPIBase:     apiURL,
		githubRawBase:     "http://localhost",
		githubCodeloadBase: codeloadURL,
	}
}

// cleanSkillCache removes the persistent cache directory for the given
// owner-repo combination. Called before tests to ensure a clean slate
// and after tests to avoid leaving temp files.
func cleanSkillCache(t *testing.T, owner, repo string) {
	t.Helper()
	cacheDir := filepath.Join(os.TempDir(), "orch-skills-cache", owner+"-"+repo)
	os.RemoveAll(cacheDir)
}

func TestGitHubRepoSource_Discover_JSONFiles(t *testing.T) {
	cleanSkillCache(t, "test-json", "skills")
	defer cleanSkillCache(t, "test-json", "skills")

	codeReviewJSON, _ := json.Marshal(config.Skill{
		ID:          "code-review",
		Name:        "Code Review",
		Capability:  "code-review",
		Description: "Review code for best practices",
		Implementations: map[string]config.SkillImplementation{
			"default": {SystemPrompt: "You are a code reviewer"},
		},
	})
	webScraperJSON, _ := json.Marshal(config.Skill{
		ID:          "web-scraper",
		Name:        "Web Scraper",
		Capability:  "web-scraping",
		Description: "Scrape web pages",
		Implementations: map[string]config.SkillImplementation{
			"default": {SystemPrompt: "You are a web scraper"},
		},
	})

	tarballData := makeTarball(t, []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/code-review.json", string(codeReviewJSON), tar.TypeReg},
		{"repo-main/web-scraper.json", string(webScraperJSON), tar.TypeReg},
	})

	codeloadURL, apiURL, cleanup := newArchiveTestServers(t, tarballData)
	defer cleanup()

	loader := newArchiveTestLoader(codeloadURL, apiURL)
	src, err := NewGitHubRepoSource("test-github", DiscoveryKindSkill, "github:test-json/skills", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != "code-review" {
		t.Errorf("expected first item ID 'code-review', got %q", items[0].ID)
	}
	if items[0].SourceName != "test-github" {
		t.Errorf("expected SourceName 'test-github', got %q", items[0].SourceName)
	}
	if items[0].TrustLevel != TrustLevelUntrusted {
		t.Errorf("expected TrustLevel %q, got %q", TrustLevelUntrusted, items[0].TrustLevel)
	}
	if items[0].SkillDir != "" {
		t.Errorf("expected empty SkillDir for prompt-only skill, got %q", items[0].SkillDir)
	}
}

func TestGitHubRepoSource_Discover_MDFiles(t *testing.T) {
	cleanSkillCache(t, "test-md", "skills")
	defer cleanSkillCache(t, "test-md", "skills")

	tarballData := makeTarball(t, []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/prompt-engineering.md", "# Prompt Engineering\n\nYou are a prompt engineering expert.", tar.TypeReg},
	})

	codeloadURL, apiURL, cleanup := newArchiveTestServers(t, tarballData)
	defer cleanup()

	loader := newArchiveTestLoader(codeloadURL, apiURL)
	src, err := NewGitHubRepoSource("test-md", DiscoveryKindSkill, "github:test-md/skills", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "prompt-engineering" {
		t.Errorf("expected ID 'prompt-engineering', got %q", items[0].ID)
	}
	if items[0].Name != "Prompt Engineering" {
		t.Errorf("expected Name 'Prompt Engineering', got %q", items[0].Name)
	}
	if items[0].Capability != "general" {
		t.Errorf("expected Capability 'general', got %q", items[0].Capability)
	}
	if items[0].SkillDir != "" {
		t.Errorf("expected empty SkillDir for prompt-only skill, got %q", items[0].SkillDir)
	}
}

func TestGitHubRepoSource_Discover_EmptyRepo(t *testing.T) {
	cleanSkillCache(t, "test-empty", "skills")
	defer cleanSkillCache(t, "test-empty", "skills")

	tarballData := makeTarball(t, []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
	})

	codeloadURL, apiURL, cleanup := newArchiveTestServers(t, tarballData)
	defer cleanup()

	loader := newArchiveTestLoader(codeloadURL, apiURL)
	src, err := NewGitHubRepoSource("test-empty", DiscoveryKindSkill, "github:test-empty/skills", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestGitHubRepoSource_Discover_SkipsNonSkillFiles(t *testing.T) {
	cleanSkillCache(t, "test-skip", "skills")
	defer cleanSkillCache(t, "test-skip", "skills")

	skillJSON, _ := json.Marshal(config.Skill{ID: "skill", Name: "Skill"})

	tarballData := makeTarball(t, []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skill.json", string(skillJSON), tar.TypeReg},
		{"repo-main/image.png", "binary-data", tar.TypeReg},
		{"repo-main/README.md", "# README\nNot a skill.", tar.TypeReg},
	})

	codeloadURL, apiURL, cleanup := newArchiveTestServers(t, tarballData)
	defer cleanup()

	loader := newArchiveTestLoader(codeloadURL, apiURL)
	src, err := NewGitHubRepoSource("test-skip", DiscoveryKindSkill, "github:test-skip/skills", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items (skill.json + README.md), got %d", len(items))
	}
}

// TestGitHubRepoSource_Discover_DirectoryBasedSkill verifies that SKILL.md
// files in subdirectories are discovered with SkillDir set, preserving
// scripts/assets/references — fixing Gap 3.1 from SKILL_MARKETPLACE_REPO_CONTRACT_DESIGN.md.
func TestGitHubRepoSource_Discover_DirectoryBasedSkill(t *testing.T) {
	cleanSkillCache(t, "test-dir", "skills-repo")
	defer cleanSkillCache(t, "test-dir", "skills-repo")

	skillContent := "---\nname: pdf-report\ndescription: Generate PDF reports\n---\n# PDF Report\nInstructions here."

	tarballData := makeTarball(t, []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skills/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/SKILL.md", skillContent, tar.TypeReg},
		{"repo-main/skills/pdf-report/scripts/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/scripts/generate.py", "print('generating PDF')", tar.TypeReg},
		{"repo-main/skills/pdf-report/assets/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/assets/template.j2", "{{title}}", tar.TypeReg},
		{"repo-main/skills/pdf-report/references/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/references/format-spec.md", "# Format Spec\nDetails here.", tar.TypeReg},
	})

	codeloadURL, apiURL, cleanup := newArchiveTestServers(t, tarballData)
	defer cleanup()

	loader := newArchiveTestLoader(codeloadURL, apiURL)
	src, err := NewGitHubRepoSource("test-dir", DiscoveryKindSkill, "github:test-dir/skills-repo", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (SKILL.md in subdirectory), got %d", len(items))
	}

	item := items[0]
	if item.ID != "ext_pdf_report" {
		t.Errorf("expected ID 'ext_pdf_report', got %q", item.ID)
	}
	if item.Name != "pdf-report" {
		t.Errorf("expected Name 'pdf-report', got %q", item.Name)
	}
	if item.SkillDir == "" {
		t.Fatal("expected non-empty SkillDir for directory-based skill")
	}

	skillDir := item.SkillDir
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md not found at SkillDir %s: %v", skillDir, err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "scripts", "generate.py")); err != nil {
		t.Errorf("scripts/generate.py not found at SkillDir %s: %v", skillDir, err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "assets", "template.j2")); err != nil {
		t.Errorf("assets/template.j2 not found at SkillDir %s: %v", skillDir, err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "references", "format-spec.md")); err != nil {
		t.Errorf("references/format-spec.md not found at SkillDir %s: %v", skillDir, err)
	}
}

// TestGitHubRepoSource_Discover_PathPrefix verifies that the source path
// prefix (specified via @ in the source URL) is respected — only skills
// under the specified path are discovered.
func TestGitHubRepoSource_Discover_PathPrefix(t *testing.T) {
	cleanSkillCache(t, "test-path", "repo")
	defer cleanSkillCache(t, "test-path", "repo")

	skillContent := "---\nname: my-skill\ndescription: A skill\n---\n# My Skill\nInstructions."

	tarballData := makeTarball(t, []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skills/", "", tar.TypeDir},
		{"repo-main/skills/my-skill/", "", tar.TypeDir},
		{"repo-main/skills/my-skill/SKILL.md", skillContent, tar.TypeReg},
		{"repo-main/other/", "", tar.TypeDir},
		{"repo-main/other/SKILL.md", "---\nname: other-skill\ndescription: Other\n---\n# Other", tar.TypeReg},
	})

	codeloadURL, apiURL, cleanup := newArchiveTestServers(t, tarballData)
	defer cleanup()

	loader := newArchiveTestLoader(codeloadURL, apiURL)
	src, err := NewGitHubRepoSource("test-path", DiscoveryKindSkill, "github:test-path/repo@skills", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item under skills/ path, got %d", len(items))
	}
	if items[0].Name != "my-skill" {
		t.Errorf("expected Name 'my-skill', got %q", items[0].Name)
	}
}

func TestGitHubRepoSource_NewGitHubRepoSource_InvalidURL(t *testing.T) {
	loader := NewResourceLoader()
	_, err := NewGitHubRepoSource("test-bad", DiscoveryKindSkill, "not-a-github-url", loader)
	if err == nil {
		t.Fatal("expected error for invalid source URL, got nil")
	}
}

func TestGitHubRepoSource_Name(t *testing.T) {
	loader := NewResourceLoader()
	src, err := NewGitHubRepoSource("my-github", DiscoveryKindSkill, "github:owner/repo", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}
	if src.Name() != "my-github" {
		t.Errorf("expected Name() 'my-github', got %q", src.Name())
	}
}

func TestGitHubRepoSource_Kind(t *testing.T) {
	loader := NewResourceLoader()
	src, err := NewGitHubRepoSource("my-github", DiscoveryKindSkill, "github:owner/repo", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}
	if src.Kind() != DiscoveryKindSkill {
		t.Errorf("expected Kind() %q, got %q", DiscoveryKindSkill, src.Kind())
	}
}

func TestParseSkillFileToDiscoveredItem_JSON(t *testing.T) {
	skillJSON := `{"id":"test-skill","name":"Test Skill","capability":"testing","description":"A test skill"}`
	item, err := parseSkillFileToDiscoveredItem("test-skill.json", skillJSON)
	if err != nil {
		t.Fatalf("parseSkillFileToDiscoveredItem failed: %v", err)
	}
	if item.ID != "test-skill" {
		t.Errorf("expected ID 'test-skill', got %q", item.ID)
	}
	if item.Name != "Test Skill" {
		t.Errorf("expected Name 'Test Skill', got %q", item.Name)
	}
	if item.Capability != "testing" {
		t.Errorf("expected Capability 'testing', got %q", item.Capability)
	}
}

func TestParseSkillFileToDiscoveredItem_MD(t *testing.T) {
	mdContent := "# My Skill\n\nYou are an expert at something."
	item, err := parseSkillFileToDiscoveredItem("my-skill.md", mdContent)
	if err != nil {
		t.Fatalf("parseSkillFileToDiscoveredItem failed: %v", err)
	}
	if item.ID != "my-skill" {
		t.Errorf("expected ID 'my-skill', got %q", item.ID)
	}
	if item.Name != "My Skill" {
		t.Errorf("expected Name 'My Skill', got %q", item.Name)
	}
	if item.Capability != "general" {
		t.Errorf("expected Capability 'general', got %q", item.Capability)
	}
	if item.Implementations["default"].SystemPrompt != mdContent {
		t.Errorf("expected SystemPrompt to be the full MD content")
	}
}

func TestSkillToDiscoveredItem(t *testing.T) {
	skill := &config.Skill{
		ID:          "test",
		Name:        "Test",
		Capability:  "testing",
		Description: "A test",
		Implementations: map[string]config.SkillImplementation{
			"default": {SystemPrompt: "prompt"},
		},
	}
	item := skillToDiscoveredItem(skill)
	if item.ID != "test" {
		t.Errorf("expected ID 'test', got %q", item.ID)
	}
	if item.Name != "Test" {
		t.Errorf("expected Name 'Test', got %q", item.Name)
	}
	if item.Kind != DiscoveryKindSkill {
		t.Errorf("expected Kind %q, got %q", DiscoveryKindSkill, item.Kind)
	}
}

func TestDiscoveredItemToSkill(t *testing.T) {
	item := DiscoveredItem{
		Kind:        DiscoveryKindSkill,
		ID:          "test",
		Name:        "Test",
		Capability:  "testing",
		Description: "A test",
		Implementations: map[string]config.SkillImplementation{
			"default": {SystemPrompt: "prompt"},
		},
	}
	skill := discoveredItemToSkill(item)
	if skill.ID != "test" {
		t.Errorf("expected ID 'test', got %q", skill.ID)
	}
	if skill.Name != "Test" {
		t.Errorf("expected Name 'Test', got %q", skill.Name)
	}
	if skill.Source != config.SourceExternalDynamic {
		t.Errorf("expected Source %q, got %q", config.SourceExternalDynamic, skill.Source)
	}
	if skill.ExecutionMode != "" {
		t.Errorf("expected empty ExecutionMode for prompt-only skill, got %q", skill.ExecutionMode)
	}
	if skill.SkillDir != "" {
		t.Errorf("expected empty SkillDir for prompt-only skill, got %q", skill.SkillDir)
	}
}

// TestDiscoveredItemToSkill_WithSkillDir verifies that when SkillDir is set,
// the skill gets ExecutionMode=Directory, SkillDir, and a scanned Manifest.
// This is the core of the Gap 3.1 fix for the Discover → ingest pipeline.
func TestDiscoveredItemToSkill_WithSkillDir(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "my-skill")
	os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755)
	os.MkdirAll(filepath.Join(skillDir, "references"), 0755)
	os.WriteFile(filepath.Join(skillDir, "scripts", "run.sh"), []byte("#!/bin/bash\necho hello"), 0644)
	os.WriteFile(filepath.Join(skillDir, "references", "spec.md"), []byte("# Spec"), 0644)

	item := DiscoveredItem{
		Kind:        DiscoveryKindSkill,
		ID:          "ext_my_skill",
		Name:        "my-skill",
		Capability:  "testing",
		Description: "A directory-based skill",
		SkillDir:    skillDir,
		Implementations: map[string]config.SkillImplementation{
			"default": {SystemPrompt: "prompt"},
		},
	}
	skill := discoveredItemToSkill(item)
	if skill.ExecutionMode != config.ExecutionModeDirectory {
		t.Errorf("expected ExecutionMode %q, got %q", config.ExecutionModeDirectory, skill.ExecutionMode)
	}
	if skill.SkillDir != skillDir {
		t.Errorf("expected SkillDir %q, got %q", skillDir, skill.SkillDir)
	}
	if skill.Manifest == nil {
		t.Fatal("expected non-nil Manifest for directory-based skill")
	}
	if len(skill.Manifest.Scripts) != 1 {
		t.Errorf("expected 1 script in Manifest, got %d", len(skill.Manifest.Scripts))
	}
	if len(skill.Manifest.References) != 1 {
		t.Errorf("expected 1 reference in Manifest, got %d", len(skill.Manifest.References))
	}
}
