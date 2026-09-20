package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the cookbook-repo support added 2026-07-12: role_cookbook_source(s)
// previously assumed a single fetchable resource, but official prompting
// guides are frequently a whole GitHub repo (e.g. anthropic-cookbook,
// google-gemini/cookbook), far too large to inject wholesale. These tests
// cover the pure parsing/scoring logic (no network) and the full
// FetchCookbook flow against a mocked GitHub API + raw-content server.

func TestParseGitHubRepoSource_GithubPrefixNoPath(t *testing.T) {
	owner, repo, path, isRepo := parseGitHubRepoSource("github:anthropics/anthropic-cookbook")
	if !isRepo || owner != "anthropics" || repo != "anthropic-cookbook" || path != "" {
		t.Fatalf("got owner=%q repo=%q path=%q isRepo=%v", owner, repo, path, isRepo)
	}
}

func TestParseGitHubRepoSource_GithubPrefixWithPath(t *testing.T) {
	owner, repo, path, isRepo := parseGitHubRepoSource("github:anthropics/anthropic-cookbook@patterns/agents")
	if !isRepo || owner != "anthropics" || repo != "anthropic-cookbook" || path != "patterns/agents" {
		t.Fatalf("got owner=%q repo=%q path=%q isRepo=%v", owner, repo, path, isRepo)
	}
}

func TestParseGitHubRepoSource_BareGitHubURL(t *testing.T) {
	owner, repo, path, isRepo := parseGitHubRepoSource("https://github.com/google-gemini/cookbook")
	if !isRepo || owner != "google-gemini" || repo != "cookbook" || path != "" {
		t.Fatalf("got owner=%q repo=%q path=%q isRepo=%v", owner, repo, path, isRepo)
	}
}

func TestParseGitHubRepoSource_BareGitHubURL_TrailingSlash(t *testing.T) {
	owner, repo, _, isRepo := parseGitHubRepoSource("https://github.com/google-gemini/cookbook/")
	if !isRepo || owner != "google-gemini" || repo != "cookbook" {
		t.Fatalf("got owner=%q repo=%q isRepo=%v", owner, repo, isRepo)
	}
}

func TestParseGitHubRepoSource_PlainURL_NotRepo(t *testing.T) {
	_, _, _, isRepo := parseGitHubRepoSource("https://docs.anthropic.com/en/docs/build-with-claude/prompt-engineering/overview")
	if isRepo {
		t.Fatal("expected a plain non-GitHub URL to not be parsed as repo-shaped")
	}
}

func TestParseGitHubRepoSource_LocalPath_NotRepo(t *testing.T) {
	_, _, _, isRepo := parseGitHubRepoSource("/root/.openclaw/workspace/cookbook.md")
	if isRepo {
		t.Fatal("expected a local path to not be parsed as repo-shaped")
	}
}

func TestParseGitHubRepoSource_TreeURL_NotSupported(t *testing.T) {
	// Deliberately unsupported per the doc comment -- /tree/<branch>/<path>
	// URLs should NOT be parsed as repo-shaped (avoids branch/path
	// disambiguation complexity); use the github: form for subdirectories.
	_, _, _, isRepo := parseGitHubRepoSource("https://github.com/anthropics/anthropic-cookbook/tree/main/patterns")
	if isRepo {
		t.Fatal("expected a /tree/ URL to not be parsed as repo-shaped")
	}
}

func TestLooksLikeCookbookContent(t *testing.T) {
	cases := map[string]bool{
		"patterns/agents/orchestrator_worker.ipynb": true,
		"README.md":                     true,
		"docs/prompting_guide.mdx":      true,
		"src/index.ts":                  false,
		"node_modules/foo/package.json": false,
		".git/config":                   false,
		"images/diagram.png":            false,
		"requirements.txt":              true,
	}
	for path, want := range cases {
		if got := looksLikeCookbookContent(path); got != want {
			t.Errorf("looksLikeCookbookContent(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCookbookTokenize_FiltersStopwordsAndShortTokens(t *testing.T) {
	words := cookbookTokenize("Please build a coding review role for the team")
	for _, sw := range []string{"please", "a", "for", "the"} {
		if words[sw] {
			t.Errorf("expected stopword %q to be filtered out", sw)
		}
	}
	if !words["build"] || !words["coding"] || !words["review"] {
		t.Errorf("expected meaningful words to survive tokenization, got %v", words)
	}
}

func TestSelectRelevantCookbookFiles_PicksHighestOverlap(t *testing.T) {
	files := []RepoFile{
		{Path: "patterns/agents/orchestrator_worker.ipynb"},
		{Path: "misc/metaprompt.ipynb"},
		{Path: "tool_use/parallel_tools.ipynb"},
		{Path: "multimodal/reading_charts_graphs.ipynb"},
		{Path: "third_party/wikipedia.ipynb"},
		{Path: "finance/reports.ipynb"},
		{Path: "extended_thinking/extended_thinking.ipynb"},
	}
	selected := selectRelevantCookbookFiles(files, "build a coding orchestrator agent role that uses parallel tools", 3)
	// Only orchestrator_worker.ipynb (matches "orchestrator") and
	// parallel_tools.ipynb (matches "parallel"+"tools") actually share any
	// keyword with the hint -- the other 5 candidates score 0 and are
	// correctly excluded rather than padded in just to reach maxFiles=3.
	if len(selected) != 2 {
		t.Fatalf("expected 2 files with nonzero overlap score, got %d: %v", len(selected), selected)
	}
	joined := ""
	for _, f := range selected {
		joined += f.Path + " "
	}
	if !strings.Contains(joined, "orchestrator_worker") {
		t.Errorf("expected orchestrator_worker.ipynb to be selected, got %v", selected)
	}
	if !strings.Contains(joined, "parallel_tools") {
		t.Errorf("expected parallel_tools.ipynb to be selected, got %v", selected)
	}
}

func TestSelectRelevantCookbookFiles_NoOverlap_FallsBackToFirstN(t *testing.T) {
	files := make([]RepoFile, 8)
	for i := range files {
		files[i] = RepoFile{Path: "unrelated_topic_file.ipynb"}
	}
	selected := selectRelevantCookbookFiles(files, "zzz qqq xxx nonsense words that match nothing", 3)
	if len(selected) != 3 {
		t.Fatalf("expected fallback to first 3 files, got %d", len(selected))
	}
}

func TestSelectRelevantCookbookFiles_FewerThanMax_ReturnsAll(t *testing.T) {
	files := []RepoFile{{Path: "a.md"}, {Path: "b.md"}}
	selected := selectRelevantCookbookFiles(files, "anything", 5)
	if len(selected) != 2 {
		t.Fatalf("expected all 2 files returned unfiltered, got %d", len(selected))
	}
}

// newMockGitHubResourceLoader builds a ResourceLoader pointed at two
// httptest servers standing in for api.github.com and
// raw.githubusercontent.com, so FetchCookbook's full repo-shaped flow can
// be exercised without any real network access.
func newMockGitHubResourceLoader(t *testing.T, apiHandler, rawHandler http.HandlerFunc) *ResourceLoader {
	t.Helper()
	apiSrv := httptest.NewServer(apiHandler)
	t.Cleanup(apiSrv.Close)
	rawSrv := httptest.NewServer(rawHandler)
	t.Cleanup(rawSrv.Close)

	return &ResourceLoader{
		client:        http.DefaultClient,
		cache:         make(map[string]string),
		githubAPIBase: apiSrv.URL,
		githubRawBase: rawSrv.URL,
	}
}

func TestFetchCookbook_RepoShaped_SelectsAndFetchesRelevantFiles(t *testing.T) {
	treeResponse := map[string]any{
		"tree": []map[string]any{
			{"path": "patterns/agents/orchestrator_worker.ipynb", "type": "blob"},
			{"path": "misc/metaprompt.ipynb", "type": "blob"},
			{"path": "third_party/wikipedia.ipynb", "type": "blob"},
			{"path": "images/diagram.png", "type": "blob"}, // filtered by looksLikeCookbookContent
			{"path": "patterns", "type": "tree"},           // filtered: not a blob
		},
	}
	treeBody, _ := json.Marshal(treeResponse)

	var apiCalls []string
	apiHandler := func(w http.ResponseWriter, r *http.Request) {
		apiCalls = append(apiCalls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/git/trees/"):
			_, _ = w.Write(treeBody)
		case strings.HasSuffix(r.URL.Path, "/repos/anthropics/anthropic-cookbook"):
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	rawHandler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Guidance content for " + r.URL.Path))
	}

	loader := newMockGitHubResourceLoader(t, apiHandler, rawHandler)

	content, err := loader.FetchCookbook(context.Background(), "github:anthropics/anthropic-cookbook",
		"build a coding orchestrator agent role")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(content, "orchestrator_worker.ipynb") {
		t.Errorf("expected the relevant file's path header in the result, got: %s", content)
	}
	if strings.Contains(content, "diagram.png") {
		t.Error("expected the image file to have been filtered out by looksLikeCookbookContent")
	}
	if len(apiCalls) != 2 {
		t.Errorf("expected exactly 2 api.github.com calls (default branch + tree), got %d: %v", len(apiCalls), apiCalls)
	}

	// Second call with the identical source+taskHint should hit the cache and
	// make zero additional API calls.
	callsBefore := len(apiCalls)
	_, err = loader.FetchCookbook(context.Background(), "github:anthropics/anthropic-cookbook",
		"build a coding orchestrator agent role")
	if err != nil {
		t.Fatalf("unexpected error on cached call: %v", err)
	}
	if len(apiCalls) != callsBefore {
		t.Errorf("expected cached call to make no new API requests, went from %d to %d calls", callsBefore, len(apiCalls))
	}
}

func TestFetchCookbook_NonRepoSource_FallsBackToFetch(t *testing.T) {
	// A plain URL should behave identically to the pre-existing Fetch --
	// verifies backward compatibility for existing single-file
	// role_cookbook_source(s) configs, zero config changes required.
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain single-file cookbook content"))
	}))
	defer fileSrv.Close()

	loader := NewResourceLoader()
	content, err := loader.FetchCookbook(context.Background(), fileSrv.URL, "irrelevant task hint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "plain single-file cookbook content" {
		t.Fatalf("expected FetchCookbook to fall back to plain Fetch behavior, got: %q", content)
	}
}

func TestFetchCookbook_RepoWithNoMatchingFiles_Errors(t *testing.T) {
	emptyTree, _ := json.Marshal(map[string]any{"tree": []map[string]any{
		{"path": "src/main.go", "type": "blob"}, // not cookbook content
	}})

	apiHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/git/trees/") {
			_, _ = w.Write(emptyTree)
			return
		}
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}
	rawHandler := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }

	loader := newMockGitHubResourceLoader(t, apiHandler, rawHandler)
	_, err := loader.FetchCookbook(context.Background(), "github:someorg/code-only-repo", "anything")
	if err == nil {
		t.Fatal("expected an error when the repo has no cookbook-shaped files, got nil")
	}
}

func TestFetchCookbook_RepoPathFilter_OnlyIncludesSubdirectory(t *testing.T) {
	treeBody, _ := json.Marshal(map[string]any{"tree": []map[string]any{
		{"path": "patterns/agents/a.ipynb", "type": "blob"},
		{"path": "misc/b.ipynb", "type": "blob"}, // outside the requested "patterns" path
	}})
	apiHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/git/trees/") {
			_, _ = w.Write(treeBody)
			return
		}
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}
	rawHandler := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("content")) }

	loader := newMockGitHubResourceLoader(t, apiHandler, rawHandler)
	content, err := loader.FetchCookbook(context.Background(), "github:anthropics/anthropic-cookbook@patterns", "agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(content, "patterns/agents/a.ipynb") {
		t.Errorf("expected in-path file to be included, got: %s", content)
	}
	if strings.Contains(content, "misc/b.ipynb") {
		t.Errorf("expected out-of-path file to be excluded, got: %s", content)
	}
}
