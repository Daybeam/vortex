package core

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type recordingScheduleScheduler struct {
	failingScheduler
	inputs []schemas.StepInput
}

func (r *recordingScheduleScheduler) Submit(inputs []schemas.StepInput) (string, error) {
	r.inputs = append([]schemas.StepInput(nil), inputs...)
	return "", errors.New("stop before monitor")
}

func (r *recordingScheduleScheduler) GetStatus(string, ...string) (map[string]any, bool) {
	return map[string]any{"status": schemas.GraphCompleted}, true
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — the scanner
// used to mark every scheduled step admin, escalating ordinary users' schedules.
func TestScheduleSubmission_PersistedCreatorTier(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		admin, want bool
	}{
		{name: "ordinary owner", owner: "alice", want: false},
		{name: "named admin", owner: "bob", admin: true, want: true},
		{name: "legacy ownerless", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := &recordingScheduleScheduler{failingScheduler: failingScheduler{err: errors.New("submit failed")}}
			ss := store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)
			sm := &ScheduleManager{scheduler: scheduler, scheduleStore: ss, logger: newTestLogger(t), stopChan: make(chan struct{})}
			sch := &schemas.Schedule{ID: "sched", OwnerID: tc.owner, SubmitterIsAdmin: tc.admin,
				TaskInputs: []schemas.StepInput{{Task: "work", SubmitterIsAdmin: !tc.want}}}
			if err := ss.Set(sch); err != nil {
				t.Fatal(err)
			}
			if _, err := sm.RunNowForOwner(sch.ID, tc.owner, tc.owner == ""); err == nil {
				t.Fatal("expected controlled submission failure")
			}
			if len(scheduler.inputs) != 1 || scheduler.inputs[0].SubmitterIsAdmin != tc.want {
				t.Fatalf("submitted tier = %+v, want admin=%v", scheduler.inputs, tc.want)
			}
		})
	}
}

func TestScheduleRunNowForOwner_RejectsBeforeSubmission(t *testing.T) {
	scheduler := &recordingScheduleScheduler{}
	ss := store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)
	if err := ss.Set(&schemas.Schedule{ID: "bob", OwnerID: "bob", TaskInputs: []schemas.StepInput{{Task: "work"}}}); err != nil {
		t.Fatal(err)
	}
	sm := &ScheduleManager{scheduler: scheduler, scheduleStore: ss, logger: newTestLogger(t), stopChan: make(chan struct{})}
	defer sm.Stop()
	if _, err := sm.RunNowForOwner("bob", "alice", false); err == nil {
		t.Fatal("cross-owner run succeeded")
	}
	if len(scheduler.inputs) != 0 {
		t.Fatal("cross-owner run reached the scheduler")
	}
	if _, err := sm.RunNowForOwner("bob", "bob", false); err == nil || err.Error() != "stop before monitor" {
		t.Fatalf("owner positive control did not reach scheduler: %v", err)
	}
	if len(scheduler.inputs) != 1 {
		t.Fatal("owner positive control did not reach scheduler")
	}
}
