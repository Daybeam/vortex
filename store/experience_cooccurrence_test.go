package store

import (
	"context"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCooccurrenceTracking_SQLite(t *testing.T) {
	dbFile := "test_cooccurrence.db"
	defer os.Remove(dbFile)

	db, err := InitDB(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := NewSQLiteExperienceBackend(db)
	ctx := context.Background()

	c1 := &CooccurrenceEntry{
		ToolA:        "research",
		ToolB:        "visualization",
		CoCount:      1,
		SuccessCount: 1,
		LastUpdated:  time.Now(),
	}
	if err := backend.SaveCooccurrence(ctx, c1); err != nil {
		t.Fatalf("SaveCooccurrence #1: %v", err)
	}

	c2 := &CooccurrenceEntry{
		ToolA:        "research",
		ToolB:        "visualization",
		CoCount:      1,
		SuccessCount: 0,
		LastUpdated:  time.Now(),
	}
	if err := backend.SaveCooccurrence(ctx, c2); err != nil {
		t.Fatalf("SaveCooccurrence #2: %v", err)
	}

	c3 := &CooccurrenceEntry{
		ToolA:        "research",
		ToolB:        "visualization",
		CoCount:      1,
		SuccessCount: 1,
		LastUpdated:  time.Now(),
	}
	if err := backend.SaveCooccurrence(ctx, c3); err != nil {
		t.Fatalf("SaveCooccurrence #3: %v", err)
	}

	candidates, err := backend.GetCompoundCandidates(ctx, 1, 0.0)
	if err != nil {
		t.Fatalf("GetCompoundCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	got := candidates[0]
	if got.ToolA != "research" || got.ToolB != "visualization" {
		t.Errorf("tool pair = (%s, %s), want (research, visualization)", got.ToolA, got.ToolB)
	}
	if got.CoCount != 3 {
		t.Errorf("CoCount = %d, want 3", got.CoCount)
	}
	if got.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2", got.SuccessCount)
	}
	if rate := got.SuccessRate(); rate != 2.0/3.0 {
		t.Errorf("SuccessRate = %.2f, want %.2f", rate, 2.0/3.0)
	}
}

func TestCompoundCandidates_ThresholdFilter(t *testing.T) {
	dbFile := "test_compound_threshold.db"
	defer os.Remove(dbFile)

	db, err := InitDB(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := NewSQLiteExperienceBackend(db)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		backend.SaveCooccurrence(ctx, &CooccurrenceEntry{
			ToolA: "alpha", ToolB: "beta",
			CoCount: 1, SuccessCount: 1, LastUpdated: time.Now(),
		})
	}

	for i := 0; i < 3; i++ {
		backend.SaveCooccurrence(ctx, &CooccurrenceEntry{
			ToolA: "gamma", ToolB: "delta",
			CoCount: 1, SuccessCount: 0, LastUpdated: time.Now(),
		})
	}

	candidates, err := backend.GetCompoundCandidates(ctx, 5, 0.85)
	if err != nil {
		t.Fatalf("GetCompoundCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (alpha→beta), got %d", len(candidates))
	}
	if candidates[0].ToolA != "alpha" || candidates[0].ToolB != "beta" {
		t.Errorf("expected alpha→beta, got %s→%s", candidates[0].ToolA, candidates[0].ToolB)
	}
}

func TestRecordTaskCompletion_TracksCooccurrence(t *testing.T) {
	dbFile := "test_cooccurrence_record.db"
	defer os.Remove(dbFile)

	db, err := InitDB(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	backend := NewSQLiteExperienceBackend(db)
	ctx := context.Background()

	dir := t.TempDir()
	ts := NewTaskStore(NewFileTaskBackend(dir))
	es, err := NewExperienceStore(dir, ts, nil, backend, nil)
	if err != nil {
		t.Fatal(err)
	}

	records := []StepRecord{
		{StepID: "s1", Capability: "research", Status: "ok", Timestamp: time.Now()},
		{StepID: "s2", Capability: "visualization", Status: "ok", Timestamp: time.Now()},
		{StepID: "s3", Capability: "reporting", Status: "ok", Timestamp: time.Now()},
	}

	if err := es.RecordTaskCompletion(ctx, "task-1", records, 0.9, nil, true, false); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	candidates, err := es.GetCompoundCandidates(ctx, 1, 0.0)
	if err != nil {
		t.Fatalf("GetCompoundCandidates: %v", err)
	}

	pairs := make(map[string]bool)
	for _, c := range candidates {
		pairs[c.ToolA+"→"+c.ToolB] = true
	}
	if !pairs["research→visualization"] {
		t.Error("expected research→visualization co-occurrence")
	}
	if !pairs["visualization→reporting"] {
		t.Error("expected visualization→reporting co-occurrence")
	}
}
