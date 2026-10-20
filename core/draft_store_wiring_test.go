package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// C4 regression: validation failures (too-simple task, workspace scope
// violation, guard rejection) must NOT save a draft. Before C4, any
// error stored a draft, persisting potentially malicious content.
// Also: taskID must be empty on error (was draftID before C4).
//
// Note (2026-10-20): the "too-simple" rejection is now opt-in via
// System.RejectSimpleTasks. We opt in here because the test exercises the
// validation-failure → no-draft path, not the FastPath default-off path.
func TestDraftStore_ValidationFailureNoDraftNoTaskID(t *testing.T) {
	s := &DirectedEngine{
		drafts: NewDraftStore(),
		registry: &config.Registry{
			DefaultProvider: "local",
			System:          config.SystemSettings{RejectSimpleTasks: true},
		},
	}

	inputs := []schemas.StepInput{
		{ID: "step1", Task: "do something simple"},
	}

	taskID, err := s.SubmitWithSessionIR(inputs, nil, nil, nil, "", "", "", 0, 0, "")
	if err == nil {
		t.Fatal("expected submission to fail for too-simple local task (RejectSimpleTasks=true)")
	}

	// C4: taskID must be empty on error (not draftID)
	if taskID != "" {
		t.Fatalf("C4: taskID must be empty on error, got %q", taskID)
	}

	// C4: no draft saved for validation failures
	drafts := s.ListDrafts()
	if len(drafts) != 0 {
		t.Fatalf("C4: validation failure must not save draft, got %d drafts", len(drafts))
	}
}

func TestDraftStore_ListAndRemove(t *testing.T) {
	s := &DirectedEngine{
		drafts: NewDraftStore(),
	}

	// C4: drafts need CreatedAt set (TTL eviction removes zero-time drafts)
	now := time.Now()
	s.drafts.Save(&TaskDraft{DraftID: "draft_test1", Steps: []any{}, CreatedAt: now})
	s.drafts.Save(&TaskDraft{DraftID: "draft_test2", Steps: []any{}, CreatedAt: now})

	list := s.ListDrafts()
	if len(list) != 2 {
		t.Errorf("expected 2 drafts, got %d", len(list))
	}

	s.RemoveDraft("draft_test1")
	list = s.ListDrafts()
	if len(list) != 1 {
		t.Errorf("expected 1 draft after remove, got %d", len(list))
	}
	if list[0].DraftID != "draft_test2" {
		t.Errorf("expected draft_test2, got %q", list[0].DraftID)
	}
}

func TestDraftStore_NilSafe(t *testing.T) {
	s := &DirectedEngine{}

	if _, ok := s.GetDraft("anything"); ok {
		t.Error("expected GetDraft to return false when draftsStore is nil")
	}
	if list := s.ListDrafts(); list != nil {
		t.Error("expected ListDrafts to return nil when draftStore is nil")
	}
	s.RemoveDraft("anything")
}

// C4 regression: DraftStore TTL evicts expired drafts on Save.
func TestDraftStore_TTLEviction(t *testing.T) {
	ds := NewDraftStore()
	now := time.Now()

	// Save a draft that's already expired
	ds.drafts["expired"] = &TaskDraft{
		DraftID:   "expired",
		CreatedAt: now.Add(-2 * ds.ttl), // well past TTL
	}

	// Save a new draft — should evict the expired one
	ds.Save(&TaskDraft{DraftID: "fresh", Steps: []any{}, CreatedAt: now})

	list := ds.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 draft after TTL eviction, got %d", len(list))
	}
	if list[0].DraftID != "fresh" {
		t.Errorf("expected fresh draft, got %q", list[0].DraftID)
	}
}

// C4 regression: DraftStore capacity limit evicts oldest on Save.
func TestDraftStore_CapacityEviction(t *testing.T) {
	ds := NewDraftStore()
	now := time.Now()

	// Fill to capacity
	for i := 0; i < ds.maxCapacity; i++ {
		ds.Save(&TaskDraft{
			DraftID:   fmt.Sprintf("draft_%d", i),
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
	}

	// Save one more — should evict the oldest (draft_0)
	ds.Save(&TaskDraft{
		DraftID:   "draft_overflow",
		CreatedAt: now.Add(time.Duration(ds.maxCapacity) * time.Second),
	})

	list := ds.List()
	if len(list) > ds.maxCapacity {
		t.Fatalf("capacity not enforced: %d > %d", len(list), ds.maxCapacity)
	}
	found := false
	for _, d := range list {
		if d.DraftID == "draft_0" {
			found = true
		}
	}
	if found {
		t.Error("oldest draft (draft_0) should have been evicted")
	}
}
