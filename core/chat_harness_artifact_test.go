package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeToolName verifies that non-alphanumeric characters are replaced
// with underscores and alphanumeric characters are preserved.
// Mutation target: changing the condition or the replacement char → test fails.
func TestSanitizeToolName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"read_file", "read_file"},
		{"write-file", "write_file"},
		{"execute code", "execute_code"},
		{"tool.name@v2", "tool_name_v2"},
		{"abc123", "abc123"},
		{"", ""},
	}
	for _, c := range cases {
		got := sanitizeToolName(c.input)
		if got != c.want {
			t.Errorf("sanitizeToolName(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestDetectAndSkeletonize_HTML verifies that HTML content is routed to
// tagSkeleton (structure-preserving, not head/tail).
// Mutation target: removing the `<` prefix check → HTML falls through to
// SummarizeOutput, which doesn't produce "[Tag Skeleton:" header → test fails.
func TestDetectAndSkeletonize_HTML(t *testing.T) {
	html := []byte(`<html><head><title>Test</title></head><body><div class="main">Hello</div></body></html>`)
	result := detectAndSkeletonize(html, "write_file")
	// tagSkeleton produces a "[Tag Skeleton: ...]" header — distinct from
	// SummarizeOutput which produces head/tail without this prefix.
	if !strings.Contains(result, "[Tag Skeleton:") {
		t.Errorf("detectAndSkeletonize(HTML) = %q, missing \"[Tag Skeleton:\" header (not routed to tagSkeleton)", result)
	}
}

// TestDetectAndSkeletonize_SVG verifies that SVG content is routed to
// tagSkeleton (SVG starts with <svg, which is caught by the `<` prefix check).
func TestDetectAndSkeletonize_SVG(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="100" height="100"><rect x="10" y="10" width="80" height="80"/></svg>`)
	result := detectAndSkeletonize(svg, "write_file")
	if !strings.Contains(result, "[Tag Skeleton:") {
		t.Errorf("detectAndSkeletonize(SVG) = %q, missing \"[Tag Skeleton:\" header", result)
	}
}

// TestDetectAndSkeletonize_JSON verifies that valid JSON is routed to
// jsonSkeleton (key-preserving, not head/tail).
// Mutation target: removing the JSON validity check or the prefix check →
// test fails because output lacks "[JSON Skeleton:" header.
func TestDetectAndSkeletonize_JSON(t *testing.T) {
	jsonData := []byte(`{"name":"test","value":42,"items":["a","b","c"]}`)
	result := detectAndSkeletonize(jsonData, "read_file")
	// jsonSkeleton produces a "[JSON Skeleton: ...]" header.
	if !strings.Contains(result, "[JSON Skeleton:") {
		t.Errorf("detectAndSkeletonize(JSON) = %q, missing \"[JSON Skeleton:\" header (not routed to jsonSkeleton)", result)
	}
}

// TestDetectAndSkeletonize_Empty verifies that empty content returns "[empty]".
// Mutation target: removing the empty check → test fails (panics on trimmed[0]
// or returns different output).
func TestDetectAndSkeletonize_Empty(t *testing.T) {
	result := detectAndSkeletonize([]byte(""), "write_file")
	if result != "[empty]" {
		t.Errorf("detectAndSkeletonize(empty) = %q, want \"[empty]\"", result)
	}
	// Whitespace-only should also be empty.
	result = detectAndSkeletonize([]byte("   \n\t  "), "write_file")
	if result != "[empty]" {
		t.Errorf("detectAndSkeletonize(whitespace) = %q, want \"[empty]\"", result)
	}
}

// TestDetectAndSkeletonize_Fallback verifies that unrecognised content falls
// through to SummarizeOutput (head/tail + middle sampling).
// Mutation target: removing the fallback return → test fails (empty or
// different output).
func TestDetectAndSkeletonize_Fallback(t *testing.T) {
	// Plain text that doesn't start with < or { or [
	data := []byte("This is plain text content that is not HTML or JSON.\nLine 2.\nLine 3.")
	result := detectAndSkeletonize(data, "unknown_tool")
	if result == "" {
		t.Error("detectAndSkeletonize(plaintext) = empty, want non-empty fallback")
	}
	if result == "[empty]" {
		t.Error("detectAndSkeletonize(plaintext) = \"[empty]\", want fallback content")
	}
}

// TestExternalizeArtifact_WritesFile verifies that externalizeArtifact writes
// the full content to a file and returns a skeleton + path reference.
// Mutation target: skipping the file write → test fails (file doesn't exist
// or result doesn't contain the path).
func TestExternalizeArtifact_WritesFile(t *testing.T) {
	tmpDir := t.TempDir()
	h := &ChatHarness{OutputBase: tmpDir}

	// Create content above the threshold.
	content := strings.Repeat("line of content\n", 200) // ~3000 bytes > 2KB
	result := h.externalizeArtifact(content, "read_file", "task-123")

	// Result should contain the externalization header.
	if !strings.Contains(result, "[Artifact externalized:") {
		t.Errorf("result missing externalization header: %q", result[:100])
	}
	// Result should contain the file path.
	if !strings.Contains(result, tmpDir) {
		t.Errorf("result missing file path: %q", result[:200])
	}
	// Result should contain "Use read_file".
	if !strings.Contains(result, "Use read_file") {
		t.Errorf("result missing read_file hint: %q", result[:200])
	}

	// The artifact file should exist on disk.
	artDir := filepath.Join(tmpDir, "task-123", "artifacts")
	entries, err := os.ReadDir(artDir)
	if err != nil {
		t.Fatalf("ReadDir(artDir): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	// File content should match the original.
	fileContent, err := os.ReadFile(filepath.Join(artDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(fileContent) != content {
		t.Error("file content does not match original result")
	}
}

// TestExternalizeArtifact_NoOutputBase verifies graceful degradation when
// OutputBase is empty — the original result is returned unchanged.
// Mutation target: removing the OutputBase check in the Run loop or the
// mkdirAll guard → test fails (result is modified or panics).
func TestExternalizeArtifact_NoOutputBase(t *testing.T) {
	h := &ChatHarness{OutputBase: ""} // no output base
	content := strings.Repeat("content\n", 500)
	result := h.externalizeArtifact(content, "read_file", "task-123")
	// Should return the original content unchanged (mkdirAll on "" returns error).
	if result != content {
		t.Errorf("result should be unchanged when OutputBase is empty")
	}
}

// TestExternalizeArtifact_ArchiveIndex verifies that the artifact is indexed
// in the ContextArchive with the correct Intent, SourceKeys, and TaskID.
// Mutation target: skipping the Archive.Append call → test fails (archive
// is empty).
func TestExternalizeArtifact_ArchiveIndex(t *testing.T) {
	tmpDir := t.TempDir()
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	h := &ChatHarness{OutputBase: tmpDir, Archive: archive}

	content := strings.Repeat("indexed content\n", 200)
	h.externalizeArtifact(content, "write_file", "task-idx")

	// Load the archive and check the artifact was indexed.
	if err := archive.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.items) == 0 {
		t.Fatal("archive.items is empty, expected 1 artifact")
	}
	item := archive.items[0]
	if item.TaskID != "task-idx" {
		t.Errorf("item.TaskID = %q, want \"task-idx\"", item.TaskID)
	}
	if !strings.Contains(item.Intent, "artifact") {
		t.Errorf("item.Intent = %q, want to contain \"artifact\"", item.Intent)
	}
	if !strings.Contains(item.Intent, "write_file") {
		t.Errorf("item.Intent = %q, want to contain \"write_file\"", item.Intent)
	}
	if len(item.SourceKeys) != 1 {
		t.Fatalf("len(item.SourceKeys) = %d, want 1", len(item.SourceKeys))
	}
	if !strings.Contains(item.SourceKeys[0], "artifacts") {
		t.Errorf("item.SourceKeys[0] = %q, want to contain \"artifacts\"", item.SourceKeys[0])
	}
}

// TestExternalizeArtifact_NilArchive verifies that externalizeArtifact works
// correctly when Archive is nil (no indexing, but file is still written).
func TestExternalizeArtifact_NilArchive(t *testing.T) {
	tmpDir := t.TempDir()
	h := &ChatHarness{OutputBase: tmpDir, Archive: nil}

	content := strings.Repeat("no archive\n", 300)
	result := h.externalizeArtifact(content, "read_file", "task-noarch")

	// Should still externalize (file write + skeleton), just no archive index.
	if !strings.Contains(result, "[Artifact externalized:") {
		t.Errorf("result missing externalization header with nil Archive")
	}
	// File should still exist.
	artDir := filepath.Join(tmpDir, "task-noarch", "artifacts")
	entries, err := os.ReadDir(artDir)
	if err != nil || len(entries) != 1 {
		t.Errorf("expected 1 artifact file, got err=%v, entries=%d", err, len(entries))
	}
}

// TestArtifactThreshold verifies the threshold constant is 2KB.
// This is a guard against accidental changes to the threshold.
func TestArtifactThreshold(t *testing.T) {
	if artifactThreshold != 2*1024 {
		t.Errorf("artifactThreshold = %d, want %d", artifactThreshold, 2*1024)
	}
}
