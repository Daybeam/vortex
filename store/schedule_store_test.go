package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

type scheduleTestBackend struct {
	rows      map[string]*schemas.Schedule
	saveErr   error
	deleteErr error
}

func (b *scheduleTestBackend) Save(_ context.Context, s *schemas.Schedule) error {
	if b.saveErr != nil {
		return b.saveErr
	}
	b.rows[s.ID] = s
	return nil
}

func (b *scheduleTestBackend) LoadAll(context.Context) ([]*schemas.Schedule, error) {
	out := make([]*schemas.Schedule, 0, len(b.rows))
	for _, s := range b.rows {
		out = append(out, s)
	}
	return out, nil
}

func (b *scheduleTestBackend) Delete(_ context.Context, id string) error {
	if b.deleteErr != nil {
		return b.deleteErr
	}
	delete(b.rows, id)
	return nil
}

// Regression (SCHEDULE_DELIVERY_TARGETS_DESIGN §4/P0): schedule persistence
// errors must reach the caller, and reload must replace rather than merge the
// authoritative backend snapshot so deleted schedules cannot fire again.
func TestScheduleStore_PersistenceErrorsAndAuthoritativeReload(t *testing.T) {
	backend := &scheduleTestBackend{rows: make(map[string]*schemas.Schedule)}
	ss := NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), backend)
	s := &schemas.Schedule{ID: "sched_1", Name: "report", Deliver: schemas.DeliverTarget{Mode: "webhook", WebhookURL: "https://192.0.2.1/hook"}}
	if err := ss.Set(s); err != nil {
		t.Fatalf("positive control Set: %v", err)
	}
	backend.saveErr = errors.New("disk full")
	updated := *s
	updated.Name = "changed"
	if err := ss.Set(&updated); err == nil {
		t.Fatal("Set swallowed backend persistence error")
	}
	if got := ss.Get(s.ID); got == nil || got.Name != "report" {
		t.Fatalf("failed Set changed in-memory schedule: %+v", got)
	}
	backend.saveErr = nil
	if err := ss.Save(); err != nil {
		t.Fatalf("Save after recovery: %v", err)
	}
	if err := ss.Delete(s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ss.Get(s.ID) != nil {
		t.Fatal("Delete left the schedule in memory")
	}
	ss.Schedules[s.ID] = s
	ss.Reload()
	if ss.Get(s.ID) != nil {
		t.Fatal("Reload merged a stale schedule deleted from the backend")
	}
}
