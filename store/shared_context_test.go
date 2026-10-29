package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSharedContext_WriteAndRead(t *testing.T) {
	dir := t.TempDir()
	sc, err := NewSharedContext(dir, "task_1")
	if err != nil {
		t.Fatalf("NewSharedContext: %v", err)
	}

	entry := &SharedContextEntry{
		Type:    ContextTypeFAIL,
		Content: "Approach X fails: constraint Y",
		Author:  "step_a",
	}
	written, err := sc.WriteEntry(entry)
	if err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	if written.ID == "" {
		t.Error("expected non-empty ID")
	}
	if written.TaskID != "task_1" {
		t.Errorf("expected TaskID=task_1, got %s", written.TaskID)
	}
	if written.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}

	entries, err := sc.ReadRecent(10, "")
	if err != nil {
		t.Fatalf("ReadRecent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Content != "Approach X fails: constraint Y" {
		t.Errorf("unexpected content: %s", entries[0].Content)
	}
}

func TestSharedContext_ReadRecent_OrderAndLimit(t *testing.T) {
	dir := t.TempDir()
	sc, _ := NewSharedContext(dir, "task_2")

	for i := 0; i < 5; i++ {
		sc.WriteEntry(&SharedContextEntry{
			Type:    ContextTypeFACT,
			Content: string(rune('A' + i)),
			Author:  "step_a",
		})
	}

	// Read last 3 — should be E, D, C (most recent first).
	entries, _ := sc.ReadRecent(3, "")
	if len(entries) != 3 {
		t.Fatalf("expected 3, got %d", len(entries))
	}
	if entries[0].Content != "E" {
		t.Errorf("expected E first, got %s", entries[0].Content)
	}
	if entries[2].Content != "C" {
		t.Errorf("expected C last, got %s", entries[2].Content)
	}
}

func TestSharedContext_TypeFilter(t *testing.T) {
	dir := t.TempDir()
	sc, _ := NewSharedContext(dir, "task_3")

	sc.WriteEntry(&SharedContextEntry{Type: ContextTypeFAIL, Content: "fail1", Author: "a"})
	sc.WriteEntry(&SharedContextEntry{Type: ContextTypeFACT, Content: "fact1", Author: "b"})
	sc.WriteEntry(&SharedContextEntry{Type: ContextTypeFAIL, Content: "fail2", Author: "c"})

	fails, _ := sc.ReadRecent(10, ContextTypeFAIL)
	if len(fails) != 2 {
		t.Fatalf("expected 2 FAILs, got %d", len(fails))
	}
	if fails[0].Content != "fail2" || fails[1].Content != "fail1" {
		t.Errorf("unexpected FAIL order: %s, %s", fails[0].Content, fails[1].Content)
	}
}

func TestSharedContext_PersistenceReload(t *testing.T) {
	dir := t.TempDir()
	sc1, _ := NewSharedContext(dir, "task_4")
	sc1.WriteEntry(&SharedContextEntry{Type: ContextTypeFACT, Content: "persisted", Author: "a"})

	// Create a new instance pointing to the same file.
	sc2, _ := NewSharedContext(dir, "task_4")
	entries, _ := sc2.ReadRecent(10, "")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after reload, got %d", len(entries))
	}
	if entries[0].Content != "persisted" {
		t.Errorf("expected 'persisted', got %s", entries[0].Content)
	}
}

func TestSharedContext_ConcurrentWrite(t *testing.T) {
	dir := t.TempDir()
	sc, _ := NewSharedContext(dir, "task_5")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sc.WriteEntry(&SharedContextEntry{
				Type:    ContextTypeOBSERVED,
				Content: "concurrent",
				Author:  "step_a",
			})
		}(i)
	}
	wg.Wait()

	entries, _ := sc.ReadAll()
	if len(entries) != 20 {
		t.Errorf("expected 20 entries, got %d", len(entries))
	}

	// Verify all entries are valid (no corruption from concurrent writes).
	for _, e := range entries {
		if e.Content != "concurrent" {
			t.Errorf("corrupted entry: %s", e.Content)
		}
	}
}

func TestSharedContext_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	sc, _ := NewSharedContext(dir, "task_6")

	entries, err := sc.ReadRecent(10, "")
	if err != nil {
		t.Fatalf("ReadRecent on empty: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestSharedContext_FilePath(t *testing.T) {
	dir := t.TempDir()
	sc, _ := NewSharedContext(dir, "task_7")

	sc.WriteEntry(&SharedContextEntry{Type: ContextTypeFACT, Content: "x", Author: "a"})

	expected := filepath.Join(dir, "task_7.jsonl")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected file %s to exist: %v", expected, err)
	}
}
