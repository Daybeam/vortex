package core

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCookbookCacheDedup(t *testing.T) {
	tmpDir := t.TempDir()
	l := &ResourceLoader{
		cache:         make(map[string]string),
		cacheDir:      tmpDir,
		cacheEnabled:  true,
		githubAPIBase: "https://api.github.com",
		githubRawBase: "https://raw.githubusercontent.com",
	}

	content := "This is the same cookbook content regardless of task hint."

	// Save the same content under 3 different (source, taskHint) keys.
	pairs := []struct{ source, hint string }{
		{"github:foo/bar", "write-api"},
		{"github:foo/bar", "write-tests"},
		{"github:foo/bar", "refactor-code"},
	}
	for _, p := range pairs {
		if err := l.saveCookbookToDisk(p.source, p.hint, content); err != nil {
			t.Fatalf("saveCookbookToDisk(%q,%q): %v", p.source, p.hint, err)
		}
	}

	// Count .txt files — should be exactly 1 (content-addressed, deduped).
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	txtCount, ptrCount := 0, 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".txt" {
			txtCount++
		}
		if filepath.Ext(e.Name()) == ".ptr" {
			ptrCount++
		}
	}
	if txtCount != 1 {
		t.Errorf("expected 1 .txt file (deduped), got %d", txtCount)
	}
	if ptrCount != 3 {
		t.Errorf("expected 3 .ptr files (one per key), got %d", ptrCount)
	}

	// All 3 keys should load the same content.
	for _, p := range pairs {
		got, err := l.loadCookbookFromDisk(p.source, p.hint)
		if err != nil {
			t.Fatalf("loadCookbookFromDisk(%q,%q): %v", p.source, p.hint, err)
		}
		if got != content {
			t.Errorf("content mismatch for (%q,%q)", p.source, p.hint)
		}
	}
}

func TestCookbookCacheLegacyFallback(t *testing.T) {
	tmpDir := t.TempDir()
	l := &ResourceLoader{
		cache:         make(map[string]string),
		cacheDir:      tmpDir,
		cacheEnabled:  true,
		githubAPIBase: "https://api.github.com",
		githubRawBase: "https://raw.githubusercontent.com",
	}

	// Write a legacy-style direct key.txt file (old format, no .ptr).
	source, hint := "github:legacy/repo", "old-hint"
	key := fmtHash(source + "::" + hint)
	legacyContent := "legacy cached content"
	legacyPath := filepath.Join(tmpDir, key+".txt")
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0644); err != nil {
		t.Fatal(err)
	}

	// loadCookbookFromDisk should fall back to the legacy .txt file.
	got, err := l.loadCookbookFromDisk(source, hint)
	if err != nil {
		t.Fatalf("legacy fallback failed: %v", err)
	}
	if got != legacyContent {
		t.Errorf("expected legacy content, got %q", got)
	}
}

func TestCookbookCacheDifferentContent(t *testing.T) {
	tmpDir := t.TempDir()
	l := &ResourceLoader{
		cache:         make(map[string]string),
		cacheDir:      tmpDir,
		cacheEnabled:  true,
		githubAPIBase: "https://api.github.com",
		githubRawBase: "https://raw.githubusercontent.com",
	}

	// Two different contents under the same key — should produce 2 .txt files.
	if err := l.saveCookbookToDisk("github:a/b", "hint1", "content-A"); err != nil {
		t.Fatal(err)
	}
	if err := l.saveCookbookToDisk("github:a/b", "hint2", "content-B"); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(tmpDir)
	txtCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".txt" {
			txtCount++
		}
	}
	if txtCount != 2 {
		t.Errorf("expected 2 .txt files (different content), got %d", txtCount)
	}
}

// fmtHash is a test helper matching the sha256 hex format used in resources.go.
func fmtHash(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// TestResourceLoader_Fetch_DirectoryReturnsError is a regression test for
// audit fix D (shadowed err variable). os.Stat succeeds on a directory, but
// os.ReadFile fails. Before the fix, the shadowed `:=` meant the ReadFile
// error was lost and Fetch returned ("", nil) — silent data loss. After the
// fix, Fetch correctly propagates the ReadFile error.
func TestResourceLoader_Fetch_DirectoryReturnsError(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}

	l := NewResourceLoader()
	_, err := l.Fetch(t.Context(), inner)
	if err == nil {
		t.Fatal("expected error when Fetch is called on a directory (os.ReadFile fails), got nil — shadowed err bug (audit fix D)")
	}
}
