package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// failingScheduler is a stub OrchestrationEngine whose Submit always returns
// an error. All other methods are no-ops. Used to simulate a cron-triggered
// task submission failure without a real engine.
type failingScheduler struct{ err error }

func (f *failingScheduler) Submit(_ []schemas.StepInput) (string, error) {
	return "", f.err
}
func (f *failingScheduler) GetStatus(_ string, _ ...string) (map[string]any, bool) {
	return nil, false
}
func (f *failingScheduler) WaitTask(_ context.Context, _ string, _ time.Duration, _ ...string) (map[string]any, bool) {
	return nil, false
}
func (f *failingScheduler) SubmitDecision(_, _, _ string) error            { return nil }
func (f *failingScheduler) SubmitDecisionWithPayload(_, _, _, _ string) error { return nil }
func (f *failingScheduler) Clear(_ string) error                          { return nil }
func (f *failingScheduler) GetStepResult(_, _ string) *store.StepResult   { return nil }
func (f *failingScheduler) GetManifest(_ string) (map[string]any, bool)   { return nil, false }
func (f *failingScheduler) CancelTask(_ string) error                     { return nil }
func (f *failingScheduler) PatchWorkspace(_ string, _ map[string]any) error { return nil }
func (f *failingScheduler) Refresh() error                                { return nil }

// TestTrigger_FailureEmitsSubmissionFailedLog verifies that when a
// cron-triggered schedule submission fails, the ScheduleManager emits a
// "schedule_submission_failed" log event containing the error message and
// schedule ID.
//
// Regression sediment for audit M4 (2026-11-04): trigger() discards the
// return value of submitAndTrack (schedule_manager.go:192), but submitAndTrack
// logs the failure internally at :238. Before this test, that observability
// path was untested — a refactor removing the log would have gone unnoticed.
// This test pins the behavior so the silent-failure observability cannot
// regress.
func TestTrigger_FailureEmitsSubmissionFailedLog(t *testing.T) {
	tmpDir := t.TempDir()

	logger, err := obs.NewLogger(filepath.Join(tmpDir, "logs"), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	sub := logger.SubscribeEvents()
	defer logger.UnsubscribeEvents(sub)

	sm := &ScheduleManager{
		scheduler:     &failingScheduler{err: errors.New("simulated submit failure")},
		scheduleStore: store.NewScheduleStore(filepath.Join(tmpDir, "schedules.json"), nil),
		logger:        logger,
		stopChan:      make(chan struct{}),
	}
	defer sm.Stop()

	sched := &schemas.Schedule{
		ID:         "sched-fail-test",
		Name:       "failing-schedule",
		Type:       schemas.ScheduleInterval,
		Interval:   "1h",
		TaskInputs: []schemas.StepInput{{Task: "test task"}},
		Status:     schemas.ScheduleActive,
	}

	// trigger is the automatic cron path; it discards the submitAndTrack
	// return value. The error must still be observable via the logger.
	sm.trigger(sched)

	// The logger writer goroutine broadcasts to subscribers asynchronously,
	// so read from the subscription channel with a timeout.
	seenFailure := false
	deadline := time.After(3 * time.Second)
	for !seenFailure {
		select {
		case ev := <-sub:
			if string(ev.Event) != "schedule_submission_failed" {
				continue // skip the leading "schedule_triggered" event
			}
			seenFailure = true
			errMsg, _ := ev.Detail["error"].(string)
			if !strings.Contains(errMsg, "simulated submit failure") {
				t.Errorf("expected error detail to contain 'simulated submit failure', got %q", errMsg)
			}
			if sid, _ := ev.Detail["schedule_id"].(string); sid != "sched-fail-test" {
				t.Errorf("expected schedule_id 'sched-fail-test', got %q", sid)
			}
		case <-deadline:
			t.Fatal("timeout: schedule_submission_failed event not received within 3s — observability regressed")
		}
	}

	// The failure counter must be incremented for adaptive recovery.
	if sched.ConsecutiveFailures != 1 {
		t.Errorf("expected ConsecutiveFailures=1, got %d", sched.ConsecutiveFailures)
	}
}
