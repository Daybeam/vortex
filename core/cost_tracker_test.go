package core

import (
	"path/filepath"
	"testing"
)

func TestCostTracker_RecordAndGetSpend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cost.db")
	ct, err := NewCostTracker(path)
	if err != nil {
		t.Fatalf("NewCostTracker: %v", err)
	}
	defer ct.Close()

	// Record several LLM calls across two models.
	ct.Record("gpt-4", "task1", "step1", 1000, 500)
	ct.Record("gpt-4", "task1", "step2", 800, 300)
	ct.Record("claude-3", "task2", "step1", 2000, 1000)

	spend, err := ct.GetTotalSpend()
	if err != nil {
		t.Fatalf("GetTotalSpend: %v", err)
	}
	if spend["gpt-4"] != 2600 {
		t.Errorf("gpt-4 total = %d, want 2600 (1000+500+800+300)", spend["gpt-4"])
	}
	if spend["claude-3"] != 3000 {
		t.Errorf("claude-3 total = %d, want 3000 (2000+1000)", spend["claude-3"])
	}
}

func TestCostTracker_NilSafe(t *testing.T) {
	var ct *CostTracker
	// Should not panic.
	if err := ct.Record("model", "task", "step", 100, 50); err != nil {
		t.Errorf("nil CostTracker.Record should return nil, got %v", err)
	}
	spend, err := ct.GetTotalSpend()
	if err != nil {
		t.Errorf("nil CostTracker.GetTotalSpend should return nil error, got %v", err)
	}
	if spend != nil {
		t.Errorf("nil CostTracker.GetTotalSpend should return nil map, got %v", spend)
	}
	ct.Close() // should not panic
}

func TestCostTracker_EmptyPathReturnsNil(t *testing.T) {
	ct, err := NewCostTracker("")
	if err != nil {
		t.Fatalf("NewCostTracker(\"\"): %v", err)
	}
	if ct != nil {
		t.Error("NewCostTracker(\"\") should return nil CostTracker")
	}
}

func TestCostTracker_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cost.db")

	ct1, err := NewCostTracker(path)
	if err != nil {
		t.Fatalf("NewCostTracker 1: %v", err)
	}
	ct1.Record("gpt-4", "task1", "step1", 1000, 500)
	ct1.Close()

	// Reopen and verify data persisted.
	ct2, err := NewCostTracker(path)
	if err != nil {
		t.Fatalf("NewCostTracker 2: %v", err)
	}
	defer ct2.Close()
	spend, err := ct2.GetTotalSpend()
	if err != nil {
		t.Fatalf("GetTotalSpend: %v", err)
	}
	if spend["gpt-4"] != 1500 {
		t.Errorf("after reopen: gpt-4 total = %d, want 1500", spend["gpt-4"])
	}
}
