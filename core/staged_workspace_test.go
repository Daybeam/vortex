package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStagedWorkspace_ScanAndHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}

	sw := NewStagedWorkspace(dir)
	files, err := sw.scanWorkspace()
	if err != nil {
		t.Fatalf("scanWorkspace: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}
	a, ok := files["a.txt"]
	if !ok {
		t.Fatalf("missing a.txt in scan result")
	}
	if a.SHA256 != hashBytes([]byte("hello")) {
		t.Errorf("wrong hash for a.txt")
	}
	if a.Size != 5 {
		t.Errorf("wrong size for a.txt: got %d", a.Size)
	}
	if _, ok := files["nested/b.txt"]; !ok {
		t.Errorf("missing nested/b.txt in scan result (nested dirs should be walked)")
	}
}

func TestStagedWorkspace_ExcludesManifestArtifactsAndStagingDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "artifacts.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "real_output.md"), []byte("content"), 0644)

	sw := NewStagedWorkspace(dir) // creates .staging/snapshots as a side effect
	// Write a decoy file directly under .staging to confirm it's never scanned.
	os.WriteFile(filepath.Join(dir, ".staging", "decoy.txt"), []byte("x"), 0644)

	files, err := sw.scanWorkspace()
	if err != nil {
		t.Fatalf("scanWorkspace: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 tracked file (real_output.md), got %d: %+v", len(files), files)
	}
	if _, ok := files["real_output.md"]; !ok {
		t.Errorf("real_output.md should be tracked")
	}
}

func TestStagedWorkspace_SnapshotAndDiff(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644)
	pre, err := sw.TakePreStepSnapshot("step1")
	if err != nil {
		t.Fatalf("TakePreStepSnapshot: %v", err)
	}
	if len(pre.Files) != 1 {
		t.Fatalf("expected 1 file in pre snapshot, got %d", len(pre.Files))
	}

	// Modify a.txt, add b.txt.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new"), 0644)

	entry, _, err := sw.RecordPostStepSnapshot("task1", "step1", "completed", pre)
	if err != nil {
		t.Fatalf("RecordPostStepSnapshot: %v", err)
	}
	if len(entry.Created) != 1 || entry.Created[0].Path != "b.txt" {
		t.Errorf("expected b.txt as the sole Created entry, got %+v", entry.Created)
	}
	if len(entry.Modified) != 1 || entry.Modified[0].Path != "a.txt" {
		t.Errorf("expected a.txt as the sole Modified entry, got %+v", entry.Modified)
	}
	if len(entry.Deleted) != 0 {
		t.Errorf("expected no deletions, got %+v", entry.Deleted)
	}
	if entry.Status != "completed" {
		t.Errorf("expected status completed, got %s", entry.Status)
	}
}

func TestStagedWorkspace_Diff_DetectsDeletion(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("keep"), 0644)
	pre, _ := sw.TakePreStepSnapshot("step1")

	os.Remove(filepath.Join(dir, "a.txt"))

	entry, _, err := sw.RecordPostStepSnapshot("task1", "step1", "completed", pre)
	if err != nil {
		t.Fatalf("RecordPostStepSnapshot: %v", err)
	}
	if len(entry.Deleted) != 1 || entry.Deleted[0].Path != "a.txt" {
		t.Errorf("expected a.txt as the sole Deleted entry, got %+v", entry.Deleted)
	}
	if len(entry.Created) != 0 || len(entry.Modified) != 0 {
		t.Errorf("expected no created/modified, got created=%+v modified=%+v", entry.Created, entry.Modified)
	}
}

func TestStagedWorkspace_Rollback_RestoresDeletedAndRemovesCreated(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("original-a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("original-b"), 0644)
	pre, err := sw.TakePreStepSnapshot("step1")
	if err != nil {
		t.Fatalf("TakePreStepSnapshot: %v", err)
	}

	// Simulate a bad step: delete a.txt, leave b.txt untouched, create c.txt.
	os.Remove(filepath.Join(dir, "a.txt"))
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("should not survive rollback"), 0644)

	if err := sw.Rollback(pre); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	aData, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("a.txt should have been restored: %v", err)
	}
	if string(aData) != "original-a" {
		t.Errorf("a.txt restored with wrong content: %q", aData)
	}

	bData, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil {
		t.Fatalf("b.txt should still exist: %v", err)
	}
	if string(bData) != "original-b" {
		t.Errorf("b.txt should be unchanged: %q", bData)
	}

	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) {
		t.Errorf("c.txt should have been removed by rollback, stat err = %v", err)
	}
}

func TestStagedWorkspace_Rollback_NilPreReturnsError(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)
	if err := sw.Rollback(nil); err == nil {
		t.Errorf("expected an error when rolling back with a nil pre-snapshot")
	}
}

func TestStagedWorkspace_Journal_AppendAndLoad(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)

	e1 := &StagingJournalEntry{StepID: "s1", TaskID: "t1", Status: "completed", Timestamp: time.Now()}
	e2 := &StagingJournalEntry{StepID: "s2", TaskID: "t1", Status: "failed", Timestamp: time.Now()}

	if err := sw.AppendJournalEntry(e1); err != nil {
		t.Fatalf("AppendJournalEntry e1: %v", err)
	}
	if err := sw.AppendJournalEntry(e2); err != nil {
		t.Fatalf("AppendJournalEntry e2: %v", err)
	}

	entries, err := sw.LoadJournal()
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 journal entries, got %d", len(entries))
	}
	if entries[0].StepID != "s1" || entries[1].StepID != "s2" {
		t.Errorf("journal entries out of order: %+v", entries)
	}
}

func TestStagedWorkspace_MarkRolledBack(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)

	sw.AppendJournalEntry(&StagingJournalEntry{StepID: "s1", Status: "failed"})

	if err := sw.MarkRolledBack("s1"); err != nil {
		t.Fatalf("MarkRolledBack: %v", err)
	}
	entries, _ := sw.LoadJournal()
	if len(entries) != 1 || !entries[0].RolledBack {
		t.Fatalf("expected entry s1 to be marked RolledBack, got %+v", entries)
	}
}

func TestStagedWorkspace_LoadJournal_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)
	entries, err := sw.LoadJournal()
	if err != nil {
		t.Fatalf("LoadJournal on a fresh workspace should not error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %+v", entries)
	}
}

func TestStagedWorkspace_SnapshotContent_DedupesIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("same"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("same"), 0644)

	snap, err := sw.TakePreStepSnapshot("step1")
	if err != nil {
		t.Fatalf("TakePreStepSnapshot: %v", err)
	}
	if snap.Files["a.txt"].SHA256 != snap.Files["b.txt"].SHA256 {
		t.Fatalf("expected identical content to hash identically")
	}

	entries, err := os.ReadDir(sw.SnapshotsDir)
	if err != nil {
		t.Fatalf("ReadDir snapshots: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly 1 stored blob for deduped identical content, got %d", len(entries))
	}
}

func TestStagedWorkspace_CleanupStaging_RespectsTTL(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644)
	sw.TakePreStepSnapshot("step1")
	sw.AppendJournalEntry(&StagingJournalEntry{StepID: "step1", Status: "completed"})

	// Not old enough: nothing should be removed.
	if err := sw.CleanupStaging(24*time.Hour, false); err != nil {
		t.Fatalf("CleanupStaging (not expired): %v", err)
	}
	if _, err := os.Stat(sw.JournalPath); err != nil {
		t.Fatalf("journal.json should still exist before TTL expiry: %v", err)
	}

	// Force the task dir to look old.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := sw.CleanupStaging(24*time.Hour, true); err != nil {
		t.Fatalf("CleanupStaging (keepJournal): %v", err)
	}
	if _, err := os.Stat(sw.SnapshotsDir); !os.IsNotExist(err) {
		t.Errorf("snapshots dir should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(sw.JournalPath); err != nil {
		t.Errorf("journal.json should be retained when keepJournal=true: %v", err)
	}
}

func TestStagedWorkspace_CleanupStaging_RemovesEverythingWhenNotKeepingJournal(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644)
	sw.TakePreStepSnapshot("step1")
	sw.AppendJournalEntry(&StagingJournalEntry{StepID: "step1", Status: "completed"})

	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(dir, old, old)

	if err := sw.CleanupStaging(24*time.Hour, false); err != nil {
		t.Fatalf("CleanupStaging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".staging")); !os.IsNotExist(err) {
		t.Errorf(".staging directory should be fully removed, stat err = %v", err)
	}
}

func TestStagedWorkspace_RecordPostStepSnapshot_NilPreTreatsEverythingAsCreated(t *testing.T) {
	dir := t.TempDir()
	sw := NewStagedWorkspace(dir)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644)

	entry, _, err := sw.RecordPostStepSnapshot("task1", "step1", "completed", nil)
	if err != nil {
		t.Fatalf("RecordPostStepSnapshot: %v", err)
	}
	if len(entry.Created) != 1 || entry.Created[0].Path != "a.txt" {
		t.Errorf("expected a.txt to be treated as created when pre=nil, got %+v", entry.Created)
	}
	if entry.SnapshotBefore != "" {
		t.Errorf("expected empty SnapshotBefore when pre=nil, got %q", entry.SnapshotBefore)
	}
}
