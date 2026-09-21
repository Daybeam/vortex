package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a test helper that creates a file with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// readFile is a test helper that reads a file's content.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExecuteBatchReplace_SingleFileSingleReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "handler.go")
	writeFile(t, path, "func oldName() {}\n")

	result, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "oldName", NewStr: "newName"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	got := readFile(t, path)
	want := "func newName() {}\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExecuteBatchReplace_SingleFileMultipleReplacements(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, "package old\n\nfunc oldFunc() {\n\toldCall()\n}\n")

	result, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "package old", NewStr: "package new"},
			{FilePath: path, OldStr: "oldFunc", NewStr: "newFunc"},
			{FilePath: path, OldStr: "oldCall()", NewStr: "newCall()"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	got := readFile(t, path)
	want := "package new\n\nfunc newFunc() {\n\tnewCall()\n}\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExecuteBatchReplace_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "a.go")
	path2 := filepath.Join(dir, "b.go")
	writeFile(t, path1, "alpha\n")
	writeFile(t, path2, "beta\n")

	result, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path1, OldStr: "alpha", NewStr: "ALPHA"},
			{FilePath: path2, OldStr: "beta", NewStr: "BETA"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ModifiedFiles) != 2 {
		t.Errorf("expected 2 modified files, got %d", len(result.ModifiedFiles))
	}
	if readFile(t, path1) != "ALPHA\n" {
		t.Error("file1 not modified correctly")
	}
	if readFile(t, path2) != "BETA\n" {
		t.Error("file2 not modified correctly")
	}
}

func TestExecuteBatchReplace_OldStrNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.go")
	original := "func keep() {}\n"
	writeFile(t, path, original)

	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "nonexistent", NewStr: "whatever"},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing old_str")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
	// File must be unchanged
	if got := readFile(t, path); got != original {
		t.Errorf("file was modified on failure: got %q, want %q", got, original)
	}
}

func TestExecuteBatchReplace_OldStrNotUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.go")
	original := "foo\nfoo\n"
	writeFile(t, path, original)

	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "foo", NewStr: "bar"},
		},
	})
	if err == nil {
		t.Fatal("expected error for non-unique old_str")
	}
	if !strings.Contains(err.Error(), "not unique") {
		t.Errorf("error should mention 'not unique', got: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Errorf("file was modified on failure: got %q, want %q", got, original)
	}
}

func TestExecuteBatchReplace_EmptyOldStr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.go")
	writeFile(t, path, "content\n")

	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "", NewStr: "x"},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty old_str")
	}
	if !strings.Contains(err.Error(), "empty old_str") {
		t.Errorf("error should mention 'empty old_str', got: %v", err)
	}
}

func TestExecuteBatchReplace_RelativePath(t *testing.T) {
	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: "relative/path.go", OldStr: "a", NewStr: "b"},
		},
	})
	if err == nil {
		t.Fatal("expected error for relative path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("error should mention 'must be absolute', got: %v", err)
	}
}

func TestExecuteBatchReplace_EmptyReplacements(t *testing.T) {
	_, err := ExecuteBatchReplace(BatchReplaceRequest{})
	if err == nil {
		t.Fatal("expected error for empty replacements")
	}
}

func TestExecuteBatchReplace_OverlappingReplacements(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlap.go")
	original := "abcdef\n"
	writeFile(t, path, original)

	// "bcd" at offset 1, "cde" at offset 2 — they overlap
	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "bcd", NewStr: "XYZ"},
			{FilePath: path, OldStr: "cde", NewStr: "UVW"},
		},
	})
	if err == nil {
		t.Fatal("expected error for overlapping replacements")
	}
	if !strings.Contains(err.Error(), "overlapping") {
		t.Errorf("error should mention 'overlapping', got: %v", err)
	}
	if got := readFile(t, path); got != original {
		t.Errorf("file was modified on failure: got %q, want %q", got, original)
	}
}

func TestExecuteBatchReplace_AtomicityFailureLeavesFilesUnchanged(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "first.go")
	path2 := filepath.Join(dir, "missing.go") // doesn't exist
	writeFile(t, path1, "original\n")

	// path2 doesn't exist → read fails → path1 must NOT be modified
	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path1, OldStr: "original", NewStr: "modified"},
			{FilePath: path2, OldStr: "x", NewStr: "y"},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	// path1 must still contain original content (atomicity)
	if got := readFile(t, path1); got != "original\n" {
		t.Errorf("atomicity violated: path1 was modified despite failure, got %q", got)
	}
}

func TestExecuteBatchReplace_NoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.go")
	writeFile(t, path, "hello\n")

	_, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: "hello", NewStr: "world"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check no .tmp files remain in the directory
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestExecuteBatchReplace_MultilineReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.go")
	original := `package main

func old() {
	line1
	line2
}
`
	writeFile(t, path, original)

	oldBlock := `func old() {
	line1
	line2
}`
	newBlock := `func new() {
	lineA
	lineB
}`

	result, err := ExecuteBatchReplace(BatchReplaceRequest{
		Replacements: []ReplacementItem{
			{FilePath: path, OldStr: oldBlock, NewStr: newBlock},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	got := readFile(t, path)
	if !strings.Contains(got, "func new()") || !strings.Contains(got, "lineA") {
		t.Errorf("multiline replacement failed, got:\n%s", got)
	}
}
