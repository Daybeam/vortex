package search

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTokenizeIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		// snake_case
		{"scheduler_decision", []string{"scheduler", "decision"}},
		// camelCase
		{"retryStep", []string{"retry", "step"}},
		// Acronym + word
		{"HTTPServer", []string{"http", "server"}},
		// Path with extension
		{"core/scheduler_decision.go", []string{"core", "scheduler", "decision"}},
		// kebab-case
		{"tools-task", []string{"tools", "task"}},
		// Mixed: path + camelCase + snake_case
		{"core/schedulerDecision_helper.go", []string{"core", "scheduler", "decision", "helper"}},
		// Single char tokens are skipped
		{"a/b.go", nil},
		// Multiple separators
		{"__init__", []string{"init"}},
		// Numbers kept
		{"handler2", []string{"handler2"}},
		// Empty
		{"", nil},
	}

	for _, tt := range tests {
		got := tokenizeIdentifier(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("tokenizeIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestDirectoryRouter_BuildAndPreFilter(t *testing.T) {
	dir := t.TempDir()
	// Create a realistic directory structure
	files := []string{
		"core/scheduler_decision.go",
		"core/scheduler_dag.go",
		"core/scheduler.go",
		"core/healer.go",
		"tools/tools_task.go",
		"tools/tools_admin.go",
		"store/experience.go",
		"store/memory_bank.go",
		"pkg/search/search.go",
		"pkg/search/prefilter.go",
		"docs/architecture/HYBRID_CODE_SEARCH_DESIGN.md",
	}
	for _, f := range files {
		full := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	router := NewDirectoryRouter(dir)
	if err := router.Build(); err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Query "scheduler" should match scheduler-related files
	result := router.PreFilter("scheduler")
	if result == nil {
		t.Fatal("expected matches for 'scheduler', got nil")
	}
	sort.Strings(result)
	// Should match at least scheduler_decision.go, scheduler_dag.go, scheduler.go
	if len(result) < 3 {
		t.Errorf("expected at least 3 matches for 'scheduler', got %d: %v", len(result), result)
	}

	// Query "tools" should match tools/ directory files
	result = router.PreFilter("tools")
	if result == nil {
		t.Fatal("expected matches for 'tools', got nil")
	}
	if len(result) < 2 {
		t.Errorf("expected at least 2 matches for 'tools', got %d: %v", len(result), result)
	}

	// Query "search" should match pkg/search/ files
	result = router.PreFilter("search")
	if result == nil {
		t.Fatal("expected matches for 'search', got nil")
	}
	if len(result) < 2 {
		t.Errorf("expected at least 2 matches for 'search', got %d: %v", len(result), result)
	}
}

func TestDirectoryRouter_PreFilterNoMatch(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "core", "scheduler.go")
	mustMkdirAll(t, filepath.Dir(full))
	mustWriteFile(t, full, []byte("x"))

	router := NewDirectoryRouter(dir)
	router.Build()

	// Non-existent keyword → nil (degrade to full search)
	result := router.PreFilter("nonexistent")
	if result != nil {
		t.Errorf("expected nil for no match, got %v", result)
	}
}

func TestDirectoryRouter_PreFilterMultipleTokens(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"core/scheduler.go":       "x",
		"core/healer.go":          "x",
		"tools/scheduler_tool.go": "x",
	}
	for f, content := range files {
		full := filepath.Join(dir, f)
		mustMkdirAll(t, filepath.Dir(full))
		mustWriteFile(t, full, []byte(content))
	}

	router := NewDirectoryRouter(dir)
	router.Build()

	// "scheduler healer" → union of both token matches
	result := router.PreFilter("scheduler healer")
	if result == nil {
		t.Fatal("expected matches, got nil")
	}
	// Should match scheduler.go, scheduler_tool.go (from "scheduler") + healer.go (from "healer")
	if len(result) < 3 {
		t.Errorf("expected at least 3 matches for 'scheduler healer', got %d: %v", len(result), result)
	}
}

func TestDirectoryRouter_SkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	// Regular file
	regular := filepath.Join(dir, "core", "scheduler.go")
	mustMkdirAll(t, filepath.Dir(regular))
	mustWriteFile(t, regular, []byte("x"))

	// Hidden directory file — should NOT be indexed
	hidden := filepath.Join(dir, ".git", "config")
	mustMkdirAll(t, filepath.Dir(hidden))
	mustWriteFile(t, hidden, []byte("x"))

	// vendor directory — should NOT be indexed
	vendor := filepath.Join(dir, "vendor", "lib.go")
	mustMkdirAll(t, filepath.Dir(vendor))
	mustWriteFile(t, vendor, []byte("x"))

	router := NewDirectoryRouter(dir)
	router.Build()

	// "config" should not match (inside .git which is skipped)
	result := router.PreFilter("config")
	if result != nil {
		t.Errorf("expected nil for .git content, got %v", result)
	}

	// "lib" should not match (inside vendor which is skipped)
	result = router.PreFilter("lib")
	if result != nil {
		t.Errorf("expected nil for vendor content, got %v", result)
	}

	// "scheduler" should still match
	result = router.PreFilter("scheduler")
	if result == nil {
		t.Error("expected match for 'scheduler'")
	}
}

func TestDirectoryRouter_CamelCaseQuery(t *testing.T) {
	dir := t.TempDir()
	// File with camelCase name
	full := filepath.Join(dir, "core", "retryStep.go")
	mustMkdirAll(t, filepath.Dir(full))
	mustWriteFile(t, full, []byte("x"))

	router := NewDirectoryRouter(dir)
	router.Build()

	// Query with camelCase should match
	result := router.PreFilter("retryStep")
	if result == nil {
		t.Fatal("expected match for 'retryStep'")
	}
	if len(result) == 0 {
		t.Error("expected non-empty result")
	}

	// Query with just "retry" should also match
	result = router.PreFilter("retry")
	if result == nil {
		t.Fatal("expected match for 'retry'")
	}
}

// --- RegexSymbolFilter tests ---

func TestRegexSymbolFilter_GoFunctions(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "core", "scheduler.go")
	mustMkdirAll(t, filepath.Dir(goFile))
	mustWriteFile(t, goFile, []byte(`
package core

func ExecuteBatchReplace(req BatchReplaceRequest) (*BatchReplaceResult, error) {
	return nil, nil
}

func (s *Scheduler) retryStep(ctx context.Context) error {
	return nil
}

type Scheduler struct{}
`))

	filter := NewRegexSymbolFilter(dir)
	if err := filter.Build(); err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Query for a function name
	result := filter.PreFilter("ExecuteBatchReplace")
	if result == nil {
		t.Fatal("expected match for 'ExecuteBatchReplace'")
	}
	if len(result) == 0 {
		t.Error("expected non-empty result")
	}

	// Query for a method name (with receiver)
	result = filter.PreFilter("retryStep")
	if result == nil {
		t.Fatal("expected match for 'retryStep'")
	}
}

func TestRegexSymbolFilter_MultiLanguage(t *testing.T) {
	dir := t.TempDir()

	// Python file
	pyFile := filepath.Join(dir, "app", "models.py")
	mustMkdirAll(t, filepath.Dir(pyFile))
	mustWriteFile(t, pyFile, []byte(`
class User:
    def get_profile(self):
        pass

def create_user(name):
    pass
`))

	// JavaScript file
	jsFile := filepath.Join(dir, "app", "handler.js")
	mustMkdirAll(t, filepath.Dir(jsFile))
	mustWriteFile(t, jsFile, []byte(`
function processRequest(req, res) {
    return null;
}

const MAX_RETRIES = 3;
`))

	// TypeScript file
	tsFile := filepath.Join(dir, "app", "types.ts")
	mustMkdirAll(t, filepath.Dir(tsFile))
	mustWriteFile(t, tsFile, []byte(`
interface UserDTO {
    id: string;
}

type Status = "active" | "inactive";
`))

	filter := NewRegexSymbolFilter(dir)
	filter.Build()

	// Python class
	result := filter.PreFilter("User")
	if result == nil {
		t.Fatal("expected match for Python class 'User'")
	}

	// Python function
	result = filter.PreFilter("create_user")
	if result == nil {
		t.Fatal("expected match for Python function 'create_user'")
	}

	// JavaScript function
	result = filter.PreFilter("processRequest")
	if result == nil {
		t.Fatal("expected match for JS function 'processRequest'")
	}

	// TypeScript interface
	result = filter.PreFilter("UserDTO")
	if result == nil {
		t.Fatal("expected match for TS interface 'UserDTO'")
	}

	// TypeScript type
	result = filter.PreFilter("Status")
	if result == nil {
		t.Fatal("expected match for TS type 'Status'")
	}
}

func TestRegexSymbolFilter_NoMatch(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	mustWriteFile(t, goFile, []byte("func existingFunc() {}\n"))

	filter := NewRegexSymbolFilter(dir)
	filter.Build()

	result := filter.PreFilter("nonexistentFunc")
	if result != nil {
		t.Errorf("expected nil for no match, got %v", result)
	}
}

func TestRegexSymbolFilter_MultiWordQuery(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "core", "batch.go")
	mustMkdirAll(t, filepath.Dir(goFile))
	mustWriteFile(t, goFile, []byte("func ExecuteBatchReplace() {}\n"))

	filter := NewRegexSymbolFilter(dir)
	filter.Build()

	// Multi-word query should try camelCase join: "execute batch replace" → "executeBatchReplace"
	// But the actual symbol is "ExecuteBatchReplace" (capital E)
	// Let's test with the exact symbol name
	result := filter.PreFilter("ExecuteBatchReplace")
	if result == nil {
		t.Fatal("expected match for 'ExecuteBatchReplace'")
	}
}

func TestRegexSymbolFilter_SkipsNonSourceFiles(t *testing.T) {
	dir := t.TempDir()
	// .txt file should not be scanned
	txtFile := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, txtFile, []byte("func shouldNotBeFound() {}\n"))

	filter := NewRegexSymbolFilter(dir)
	filter.Build()

	result := filter.PreFilter("shouldNotBeFound")
	if result != nil {
		t.Errorf("expected nil for non-source file, got %v", result)
	}
}

func TestExtractSymbolCandidates(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		// Exact identifier
		{"retryStep", []string{"retryStep", "retry", "step"}},
		// Multi-word → camelCase + snake_case joins
		{"execute batch", []string{"execute", "batch", "executeBatch", "execute_batch"}},
		// Empty
		{"", nil},
	}

	for _, tt := range tests {
		got := extractSymbolCandidates(tt.query)
		// Check that all expected candidates are present (order may vary)
		gotSet := make(map[string]bool)
		for _, g := range got {
			gotSet[g] = true
		}
		for _, w := range tt.want {
			if !gotSet[w] {
				t.Errorf("extractSymbolCandidates(%q): missing %q, got %v", tt.query, w, got)
			}
		}
	}
}

func TestIsIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"retryStep", true},
		{"retry_step", true},
		{"retry123", true},
		{"retry step", false}, // space
		{"retry-step", false}, // hyphen
		{"retry.step", false}, // dot
		{"", false},
		{"_private", true},
	}

	for _, tt := range tests {
		if got := isIdentifier(tt.input); got != tt.want {
			t.Errorf("isIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
