package store

import (
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — ownerless
// rows are local-only, and callers must not mutate the scanner's live pointers.
func TestScheduleStore_VisibleScopeAndSnapshots(t *testing.T) {
	ss := NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)
	for _, sch := range []*schemas.Schedule{
		{ID: "alice", OwnerID: "alice", TaskInputs: []schemas.StepInput{{Task: "alice"}}},
		{ID: "bob", OwnerID: "bob"},
		{ID: "legacy"},
	} {
		if err := ss.Set(sch); err != nil {
			t.Fatal(err)
		}
	}
	if got := ss.ListVisible("alice", false); len(got) != 1 || got[0].ID != "alice" {
		t.Fatalf("alice visible rows = %+v", got)
	}
	if ss.GetVisible("bob", "alice", false) != nil || ss.GetVisible("legacy", "alice", false) != nil {
		t.Fatal("foreign or ownerless schedule was visible in multi-user scope")
	}
	if got := ss.ListVisible("", false); len(got) != 0 {
		t.Fatalf("anonymous strict scope saw ownerless/foreign rows: %+v", got)
	}
	if got := ss.ListVisible("alice", true); len(got) != 3 {
		t.Fatalf("admin/local positive control returned %d rows, want 3", len(got))
	}

	got := ss.Get("alice")
	got.Name = "mutated"
	got.TaskInputs[0].Task = "mutated"
	if stored := ss.Get("alice"); stored.Name != "" || stored.TaskInputs[0].Task != "alice" {
		t.Fatalf("Get exposed live schedule state: %+v", stored)
	}
}
