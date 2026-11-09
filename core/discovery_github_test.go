package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestGitHubRepoSource_Discover_JSONFiles(t *testing.T) {
	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-review.json":
			skill := config.Skill{
				ID:          "code-review",
				Name:        "Code Review",
				Capability:  "code-review",
				Description: "Review code for best practices",
				Implementations: map[string]config.SkillImplementation{
					"default": {SystemPrompt: "You are a code reviewer"},
				},
			}
			json.NewEncoder(w).Encode(skill)
		case "/web-scraper.json":
			skill := config.Skill{
				ID:          "web-scraper",
				Name:        "Web Scraper",
				Capability:  "web-scraping",
				Description: "Scrape web pages",
				Implementations: map[string]config.SkillImplementation{
					"default": {SystemPrompt: "You are a web scraper"},
				},
			}
			json.NewEncoder(w).Encode(skill)
		}
	}))
	defer rawSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files := []RepoFile{
			{Name: "code-review.json", Type: "file", DownloadURL: rawSrv.URL + "/code-review.json"},
			{Name: "web-scraper.json", Type: "file", DownloadURL: rawSrv.URL + "/web-scraper.json"},
		}
		json.NewEncoder(w).Encode(files)
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:        http.DefaultClient,
		cache:         make(map[string]string),
		githubAPIBase: apiSrv.URL,
		githubRawBase: rawSrv.URL,
	}

	src, err := NewGitHubRepoSource("test-github", DiscoveryKindSkill, "github:test/skills", loader)
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
}

func TestGitHubRepoSource_Discover_MDFiles(t *testing.T) {
	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# Prompt Engineering\n\nYou are a prompt engineering expert."))
	}))
	defer rawSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files := []RepoFile{
			{Name: "prompt-engineering.md", Type: "file", DownloadURL: rawSrv.URL + "/prompt-engineering.md"},
		}
		json.NewEncoder(w).Encode(files)
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:        http.DefaultClient,
		cache:         make(map[string]string),
		githubAPIBase: apiSrv.URL,
		githubRawBase: rawSrv.URL,
	}

	src, err := NewGitHubRepoSource("test-md", DiscoveryKindSkill, "github:test/skills", loader)
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
}

func TestGitHubRepoSource_Discover_EmptyRepo(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]RepoFile{})
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:        http.DefaultClient,
		cache:         make(map[string]string),
		githubAPIBase: apiSrv.URL,
		githubRawBase: "http://localhost",
	}

	src, err := NewGitHubRepoSource("test-empty", DiscoveryKindSkill, "github:test/skills", loader)
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

func TestGitHubRepoSource_Discover_SkipsNonFileEntries(t *testing.T) {
	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/skill.json" {
			skill := config.Skill{ID: "skill", Name: "Skill"}
			json.NewEncoder(w).Encode(skill)
		}
	}))
	defer rawSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files := []RepoFile{
			{Name: "subdir", Type: "dir", DownloadURL: ""},
			{Name: "skill.json", Type: "file", DownloadURL: rawSrv.URL + "/skill.json"},
			{Name: "image.png", Type: "file", DownloadURL: rawSrv.URL + "/image.png"},
		}
		json.NewEncoder(w).Encode(files)
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:        http.DefaultClient,
		cache:         make(map[string]string),
		githubAPIBase: apiSrv.URL,
		githubRawBase: rawSrv.URL,
	}

	src, err := NewGitHubRepoSource("test-skip", DiscoveryKindSkill, "github:test/skills", loader)
	if err != nil {
		t.Fatalf("NewGitHubRepoSource failed: %v", err)
	}

	items, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (dir and .png skipped), got %d", len(items))
	}
	if items[0].ID != "skill" {
		t.Errorf("expected item ID 'skill', got %q", items[0].ID)
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
}
