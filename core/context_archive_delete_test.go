package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDeleteByScope_RemovesMatchingTask verifies that DeleteByScope with a
// TaskID filter removes only items matching that TaskID and rewrites the file.
func TestDeleteByScope_RemovesMatchingTask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.jsonl")
	a := NewContextArchive(path)

	// Append items with different TaskIDs.
	items := []MemoryItem{
		{TaskID: "taskA", NodeID: "n1", Intent: "alpha", Timestamp: time.Now()},
		{TaskID: "taskB", NodeID: "n2", Intent: "beta", Timestamp: time.Now()},
		{TaskID: "taskA", NodeID: "n3", Intent: "gamma", Timestamp: time.Now()},
	}
	for _, item := range items {
		if err := a.Append(item); err != nil {
			t.Fatalf("Append failed: %v", err)
		}
	}

	// Delete taskA entries.
	deleted, err := a.DeleteByScope(ArchiveScope{TaskID: "taskA"})
	if err != nil {
		t.Fatalf("DeleteByScope failed: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}

	// Verify remaining items: only taskB should survive.
	a2 := NewContextArchive(path)
	if err := a2.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(a2.items) != 1 {
		t.Fatalf("expected 1 remaining item, got %d", len(a2.items))
	}
	if a2.items[0].TaskID != "taskB" {
		t.Errorf("expected remaining item TaskID=taskB, got %s", a2.items[0].TaskID)
	}
}

// TestDeleteByScope_NoMatch returns 0 and does not rewrite the file.
func TestDeleteByScope_NoMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.jsonl")
	a := NewContextArchive(path)

	if err := a.Append(MemoryItem{TaskID: "taskA", NodeID: "n1", Intent: "x", Timestamp: time.Now()}); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	deleted, err := a.DeleteByScope(ArchiveScope{TaskID: "nonexistent"})
	if err != nil {
		t.Fatalf("DeleteByScope failed: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
	if len(a.items) != 1 {
		t.Errorf("item count should be unchanged, got %d", len(a.items))
	}
}

// TestDeleteByScope_EmptyScope returns an error.
func TestDeleteByScope_EmptyScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.jsonl")
	a := NewContextArchive(path)

	_, err := a.DeleteByScope(ArchiveScope{})
	if err == nil {
		t.Error("expected error for empty scope, got nil")
	}
}

// TestDeleteByScope_EmptyArchive returns 0, nil.
func TestDeleteByScope_EmptyArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.jsonl")
	a := NewContextArchive(path)

	deleted, err := a.DeleteByScope(ArchiveScope{TaskID: "taskA"})
	if err != nil {
		t.Fatalf("DeleteByScope failed: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted from empty archive, got %d", deleted)
	}
}

// TestDeleteByScope_FileRewritten verifies the JSONL file is rewritten after
// deletion (not just the in-memory slice).
func TestDeleteByScope_FileRewritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.jsonl")
	a := NewContextArchive(path)

	_ = a.Append(MemoryItem{TaskID: "taskA", NodeID: "n1", Intent: "keep", Timestamp: time.Now()})
	_ = a.Append(MemoryItem{TaskID: "taskB", NodeID: "n2", Intent: "delete", Timestamp: time.Now()})

	_, err := a.DeleteByScope(ArchiveScope{TaskID: "taskB"})
	if err != nil {
		t.Fatalf("DeleteByScope failed: %v", err)
	}

	// Read the raw file and verify it contains only "keep".
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "keep") {
		t.Error("file should contain 'keep' item")
	}
	if strings.Contains(s, "delete") {
		t.Error("file should NOT contain 'delete' item")
	}
}

// TestDeleteByScope_IndexDirty verifies the BM25 index is marked dirty after
// deletion so the next search rebuilds it.
func TestDeleteByScope_IndexDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.jsonl")
	a := NewContextArchive(path)

	_ = a.Append(MemoryItem{TaskID: "taskA", NodeID: "n1", Intent: "x", Timestamp: time.Now()})
	_ = a.Append(MemoryItem{TaskID: "taskB", NodeID: "n2", Intent: "y", Timestamp: time.Now()})

	// Build the index so it's clean.
	a.BuildIndex()
	if a.indexDirty {
		t.Fatal("index should be clean after BuildIndex")
	}

	// Delete → index should be dirty.
	_, err := a.DeleteByScope(ArchiveScope{TaskID: "taskA"})
	if err != nil {
		t.Fatalf("DeleteByScope failed: %v", err)
	}
	if !a.indexDirty {
		t.Error("index should be dirty after DeleteByScope")
	}
}
