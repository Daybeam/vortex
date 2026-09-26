package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// RepoFile represents a GitHub API file entry.
type RepoFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
}

// ResourceLoader handles fetching and caching remote/local content.

type ResourceLoader struct {
	client *http.Client
	Mu     sync.RWMutex
	cache  map[string]string

	// githubAPIBase/githubRawBase default to the real GitHub endpoints (set
	// by NewResourceLoader) and exist as fields rather than hardcoded string
	// literals purely so tests can point them at an httptest.Server instead
	// of making real network calls. ADDED (2026-07-12) alongside FetchCookbook.
	githubAPIBase string
	githubRawBase string

	// Persistence fields (ADDED 2026-07-14)
	cacheDir     string
	cacheEnabled bool
}

const maxResourceCacheEntries = 1000 // cap on in-memory cache; clearing is safe (re-fetches from source)

// cacheSetLocked sets a cache entry and caps the cache size to prevent
// unbounded growth. Caller must hold l.Mu (write lock).
func (l *ResourceLoader) cacheSetLocked(key, value string) {
	l.cache[key] = value
	if len(l.cache) > maxResourceCacheEntries {
		l.cache = make(map[string]string)
		l.cache[key] = value
	}
}

func NewResourceLoader() *ResourceLoader {
	return &ResourceLoader{
		client:        &http.Client{Timeout: 30 * time.Second},
		cache:         make(map[string]string),
		githubAPIBase: "https://api.github.com",
		githubRawBase: "https://raw.githubusercontent.com",
	}
}

func (l *ResourceLoader) SetCache(dir string, enabled bool) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	l.cacheDir = dir
	l.cacheEnabled = enabled
}

// Fetch content from a URL or local file path.
func (l *ResourceLoader) Fetch(ctx context.Context, source string) (string, error) {
	l.Mu.RLock()
	if val, ok := l.cache[source]; ok {
		l.Mu.RUnlock()
		return val, nil
	}
	l.Mu.RUnlock()

	var content []byte
	var err error

	// Handle local file
	if _, err := os.Stat(source); err == nil {
		content, err = os.ReadFile(source)
	} else {
		// Handle URL
		req, err := http.NewRequestWithContext(ctx, "GET", source, nil)
		if err != nil {
			return "", err
		}
		resp, err := l.client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		// audit H1: cap to prevent OOM from unbounded responses
		content, err = io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	}

	if err != nil {
		return "", err
	}

	res := string(content)
	l.Mu.Lock()
	l.cacheSetLocked(source, res)
	l.Mu.Unlock()
	return res, nil
}

// FetchRepoContents lists files in a GitHub repository folder.
func (l *ResourceLoader) FetchRepoContents(ctx context.Context, owner, repo, path string) ([]RepoFile, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents", owner, repo)
	if path != "" {
		url = fmt.Sprintf("%s/%s", url, path)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api returned status: %d", resp.StatusCode)
	}

	var files []RepoFile
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, err
	}

	return files, nil
}

// ============================================================================
// Cookbook repo support (ADDED 2026-07-12)
//
// role_cookbook_source(s) previously assumed a single fetchable resource
// (one URL or local file, handled by Fetch above). But the "official"
// prompting/engineering-principles source for a given model family is
// frequently a whole GitHub repo, not a single file -- e.g.
// github.com/anthropics/anthropic-cookbook or github.com/google-gemini/
// cookbook are each dozens of notebooks/docs across several folders, far
// too large to inject wholesale into a single role-generation prompt.
//
// FetchCookbook below is a drop-in replacement for Fetch at role_generator.go's
// call site: for a plain URL or local path it behaves identically to Fetch
// (backward compatible, zero config changes needed for existing single-file
// sources). For a repo-shaped source (github:owner/repo[@path] or a bare
// https://github.com/owner/repo URL) it instead lists the repo's file tree,
// scores files by keyword overlap against the role-generation task
// description, and fetches only the top-scoring subset -- retrieval, not
// wholesale injection, mirroring the same design already used for SOP
// candidate matching (core/sop_matcher.go) and skill recommendation
// (store/experience.go's QueryRelevantSkills).
// ============================================================================

const (
	cookbookMaxFiles      = 5    // max number of files injected into one prompt
	cookbookMaxFileBytes  = 6000 // per-file truncation cap
	cookbookRepoListLimit = 500  // safety cap on how many tree entries we even score
)

// FetchCookbook resolves a cookbook source that may be either a single
// fetchable resource (existing Fetch behavior, unchanged) or a repo-shaped
// reference. taskHint (typically the role-generation task description) is
// used only for repo-shaped sources, to select which files are relevant
// enough to actually fetch and inject.
func (l *ResourceLoader) FetchCookbook(ctx context.Context, source, taskHint string) (string, error) {
	// FIX (2026-07-25): defense-in-depth against a nil receiver -- see the
	// matching guard added in core/role_generator.go's GenerateRoleObjects
	// (the 2026-07-24 addendum documented a real, reproducible nil-pointer
	// panic here when a RoleGenerator's resourceLoader field was nil). That
	// guard is the primary fix for the known call path; this one protects
	// against any other/future caller reaching a nil *ResourceLoader directly.
	if l == nil {
		return "", fmt.Errorf("FetchCookbook called on a nil ResourceLoader")
	}

	owner, repo, path, isRepo := parseGitHubRepoSource(source)
	if !isRepo {
		return l.Fetch(ctx, source)
	}

	cacheKey := "cookbook-repo:" + source + "::" + taskHint
	l.Mu.RLock()
	if val, ok := l.cache[cacheKey]; ok {
		l.Mu.RUnlock()
		return val, nil
	}
	l.Mu.RUnlock()

	// Persistence check (ADDED 2026-07-14)
	if l.cacheEnabled && l.cacheDir != "" {
		if content, err := l.loadCookbookFromDisk(source, taskHint); err == nil && content != "" {
			l.Mu.Lock()
			l.cacheSetLocked(cacheKey, content)
			l.Mu.Unlock()
			return content, nil
		}
	}

	tree, err := l.fetchRepoTreeRecursive(ctx, owner, repo, path)
	if err != nil {
		return "", fmt.Errorf("failed to list cookbook repo %s/%s: %w", owner, repo, err)
	}
	if len(tree) == 0 {
		return "", fmt.Errorf("cookbook repo %s/%s (path %q) has no fetchable text/notebook files", owner, repo, path)
	}

	selected := selectRelevantCookbookFiles(tree, taskHint, cookbookMaxFiles)

	var sb strings.Builder
	for _, f := range selected {
		content, ferr := l.fetchRaw(ctx, f.DownloadURL)
		if ferr != nil {
			// One unreadable file (e.g. transient network blip, or GitHub API
			// rate limiting on the raw content host) should not fail the
			// whole cookbook -- skip it and use whatever else was fetched.
			continue
		}
		if len(content) > cookbookMaxFileBytes {
			content = content[:cookbookMaxFileBytes] + "\n...[truncated]"
		}
		fmt.Fprintf(&sb, "### %s\n%s\n\n", f.Path, content)
	}

	result := sb.String()
	if result == "" {
		return "", fmt.Errorf("cookbook repo %s/%s: all %d selected file(s) failed to fetch", owner, repo, len(selected))
	}

	// Save to disk if enabled (ADDED 2026-07-14)
	if l.cacheEnabled && l.cacheDir != "" {
		_ = l.saveCookbookToDisk(source, taskHint, result)
	}

	l.Mu.Lock()
	l.cacheSetLocked(cacheKey, result)
	l.Mu.Unlock()
	return result, nil
}

// Persistence helpers (ADDED 2026-07-14)

func (l *ResourceLoader) loadCookbookFromDisk(source, taskHint string) (string, error) {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(source+"::"+taskHint)))

	ptrPath := filepath.Join(l.cacheDir, key+".ptr")
	if contentHash, err := os.ReadFile(ptrPath); err == nil {
		contentPath := filepath.Join(l.cacheDir, string(contentHash)+".txt")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	legacyPath := filepath.Join(l.cacheDir, key+".txt")
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (l *ResourceLoader) saveCookbookToDisk(source, taskHint, content string) error {
	if err := os.MkdirAll(l.cacheDir, 0755); err != nil {
		return err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(source+"::"+taskHint)))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	contentPath := filepath.Join(l.cacheDir, contentHash+".txt")
	if _, err := os.Stat(contentPath); os.IsNotExist(err) {
		if err := os.WriteFile(contentPath, []byte(content), 0644); err != nil {
			return err
		}
	}

	ptrPath := filepath.Join(l.cacheDir, key+".ptr")
	return os.WriteFile(ptrPath, []byte(contentHash), 0644)
}

// parseGitHubRepoSource recognizes two repo-shaped source syntaxes:
//   - "github:owner/repo" or "github:owner/repo@some/subdir"
//   - a bare "https://github.com/owner/repo" URL (no further path segments;
//     this deliberately does NOT parse /tree/<branch>/<path> URLs -- paste
//     the github: form instead if a subdirectory is needed, to avoid the
//     added complexity of branch-vs-path disambiguation for an edge case)
//
// Anything else (plain http(s) URL to a single file/page, or a local path)
// returns isRepo=false so callers fall back to the existing single-fetch
// behavior unchanged.
func parseGitHubRepoSource(source string) (owner, repo, path string, isRepo bool) {
	if strings.HasPrefix(source, "github:") {
		rest := strings.TrimPrefix(source, "github:")
		ownerRepo := rest
		if idx := strings.Index(rest, "@"); idx >= 0 {
			ownerRepo = rest[:idx]
			path = strings.Trim(rest[idx+1:], "/")
		}
		parts := strings.SplitN(ownerRepo, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", false
		}
		return parts[0], parts[1], path, true
	}

	const prefix = "https://github.com/"
	if strings.HasPrefix(source, prefix) {
		rest := strings.TrimSuffix(strings.TrimPrefix(source, prefix), "/")
		segs := strings.Split(rest, "/")
		if len(segs) == 2 && segs[0] != "" && segs[1] != "" {
			return segs[0], segs[1], "", true
		}
	}

	return "", "", "", false
}

// fetchRepoTreeRecursive lists all files under path (or the whole repo if
// path is empty) using GitHub's recursive git trees API, filtered down to
// files that plausibly contain prose/prompting guidance (looksLikeCookbookContent).
// Unlike FetchRepoContents (single-directory, non-recursive, used by
// SkillSyncer and left untouched here), this walks the entire subtree in
// one API call so a deeply-nested official cookbook repo doesn't require
// N directory-by-directory requests.
func (l *ResourceLoader) fetchRepoTreeRecursive(ctx context.Context, owner, repo, path string) ([]RepoFile, error) {
	branch, err := l.fetchDefaultBranch(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", l.githubAPIBase, owner, repo, branch)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	l.setGitHubHeaders(req)

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxErrorBody)) // audit H1: cap error body
		return nil, fmt.Errorf("github tree API returned status %d: %s", resp.StatusCode, truncateBytesForLog(body, 300))
	}

	var parsed struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var files []RepoFile
	for _, item := range parsed.Tree {
		if item.Type != "blob" {
			continue
		}
		if path != "" && !strings.HasPrefix(item.Path, path+"/") && item.Path != path {
			continue
		}
		if !looksLikeCookbookContent(item.Path) {
			continue
		}
		files = append(files, RepoFile{
			Name:        item.Path,
			Path:        item.Path,
			Type:        "file",
			DownloadURL: fmt.Sprintf("%s/%s/%s/%s/%s", l.githubRawBase, owner, repo, branch, item.Path),
		})
		if len(files) >= cookbookRepoListLimit {
			break
		}
	}
	return files, nil
}

func (l *ResourceLoader) fetchDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", l.githubAPIBase, owner, repo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	l.setGitHubHeaders(req)

	resp, err := l.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxErrorBody)) // audit H1: cap error body
		return "", fmt.Errorf("github repos API returned status %d: %s", resp.StatusCode, truncateBytesForLog(body, 300))
	}

	var parsed struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.DefaultBranch == "" {
		return "main", nil
	}
	return parsed.DefaultBranch, nil
}

func (l *ResourceLoader) fetchRaw(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d fetching %s", resp.StatusCode, url)
	}
	// audit H1: cap to prevent OOM from unbounded responses
	body, err := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// setGitHubHeaders sets the standard Accept header, plus an Authorization
// header if GITHUB_TOKEN is set in the environment -- unauthenticated GitHub
// API requests are limited to 60/hour per IP, which a single cookbook
// resolution (2 API calls: default-branch lookup + recursive tree listing;
// the actual file content fetches go through raw.githubusercontent.com,
// which does not count against the api.github.com rate limit) comfortably
// fits within even repeatedly, but an optional token costs nothing to
// support and helps if GITHUB_TOKEN happens to already be set for other
// reasons (e.g. private cookbook repos).
func (l *ResourceLoader) setGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// looksLikeCookbookContent filters a repo tree down to files plausibly
// containing prose/prompting guidance rather than code, data, or build
// artifacts. Notebooks (.ipynb) are included deliberately -- official
// cookbook repos (e.g. anthropic-cookbook) are frequently almost entirely
// notebooks, mixing markdown guidance cells with code cells; the existing
// role-generation prompt already instructs the model to "intelligently
// extract only the relevant engineering principles... Do not include raw
// code" when the cookbook resource looks like a notebook, so raw .ipynb
// JSON reaching the model is an accepted, already-handled case.
func looksLikeCookbookContent(path string) bool {
	lower := strings.ToLower(path)
	for _, skip := range []string{"/node_modules/", "/.git/", "/dist/", "/build/", "/__pycache__/", "/vendor/"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	for _, ext := range []string{".md", ".mdx", ".txt", ".rst", ".ipynb"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// selectRelevantCookbookFiles scores each candidate file by keyword overlap
// between its path (directory names + filename carry real signal in most
// cookbook repos, e.g. "patterns/agents/orchestrator_worker.ipynb" vs a task
// about building a coding-review role) and taskHint, then returns the
// top-scoring maxFiles files. If taskHint is empty, or nothing scores above
// zero, falls back to the first maxFiles files in tree order rather than
// returning nothing -- role generation should still have *something* to
// work with over failing outright because keyword matching didn't find a
// confident hit, the same "recall over precision, let the LLM sort it out"
// principle already used by SOP candidate matching (core/sop_matcher.go).
func selectRelevantCookbookFiles(files []RepoFile, taskHint string, maxFiles int) []RepoFile {
	if len(files) <= maxFiles {
		return files
	}

	hintWords := cookbookTokenize(taskHint)
	if len(hintWords) == 0 {
		return files[:maxFiles]
	}

	type scored struct {
		file  RepoFile
		score int
	}
	candidates := make([]scored, 0, len(files))
	for _, f := range files {
		pathWords := cookbookTokenize(strings.NewReplacer("/", " ", "_", " ", "-", " ").Replace(f.Path))
		score := 0
		for w := range hintWords {
			if pathWords[w] {
				score++
			}
		}
		candidates = append(candidates, scored{file: f, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	if candidates[0].score == 0 {
		return files[:maxFiles]
	}

	out := make([]RepoFile, 0, maxFiles)
	for _, c := range candidates {
		if len(out) >= maxFiles {
			break
		}
		if c.score == 0 {
			break
		}
		out = append(out, c.file)
	}
	if len(out) == 0 {
		return files[:maxFiles]
	}
	return out
}

// cookbookTokenize lowercases and splits on non-alphanumeric boundaries,
// filtering very short tokens and a small English stopword list -- same
// lightweight, dependency-free approach already used by
// store/experience.go's tokenize for skill-relevance scoring (see that
// file's TestQueryRelevantSkills_OverlapAndNoFalsePositive comment for why
// stopword filtering matters: without it, two unrelated file paths can
// share only common words like "the"/"a" and falsely score as related).
func cookbookTokenize(s string) map[string]bool {
	var stopwords = map[string]bool{
		"the": true, "a": true, "an": true, "of": true, "to": true, "in": true,
		"on": true, "for": true, "and": true, "or": true, "are": true, "is": true,
		"was": true, "were": true, "be": true, "this": true, "that": true,
		"with": true, "as": true, "at": true, "by": true, "it": true, "if": true,
		"please": true, "me": true, "my": true, "your": true, "you": true,
	}

	out := make(map[string]bool)
	var cur strings.Builder
	flush := func() {
		w := strings.ToLower(cur.String())
		cur.Reset()
		if len(w) >= 3 && !stopwords[w] {
			out[w] = true
		}
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// truncateBytesForLog truncates a byte slice to n bytes for safe inclusion
// in an error message, appending a marker if truncation occurred.
func truncateBytesForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...[truncated]"
}
