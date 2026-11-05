package core

import (
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Regression test for register O5, decision Q4-1 (2026-11-05).
//
// Bug shape: nothing on the MCP surface could set a row back to
// ScheduleActive, and even if one had, the stored NextRun of a paused schedule
// is in the past, so scan's `now.After(s.NextRun)` would fire a catch-up run
// the moment the row went active. The operator's ruling: skip the missed ticks
// — a scheduled run usually spends tokens, and a burst on resume is the harm
// the pause was meant to prevent.
//
// So the rule under test is "the next occurrence strictly after now", which is
// what schedule.resume writes. Past ticks are never replayed.

func TestScheduleNextRunAfter_IntervalSkipsEveryMissedTick(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-3 * time.Hour) // five windows already gone by
	s := &schemas.Schedule{
		ID:       "sch_interval",
		Type:     schemas.ScheduleInterval,
		Interval: "1h",
		Status:   schemas.SchedulePaused,
		NextRun:  stale,
	}

	next, err := ScheduleNextRunAfter(s, now)
	if err != nil {
		t.Fatalf("ScheduleNextRunAfter: %v", err)
	}
	if !next.Equal(now.Add(time.Hour)) {
		t.Errorf("next = %s, want %s (now + one interval, no catch-up)", next, now.Add(time.Hour))
	}
	if !next.After(now) {
		t.Errorf("resume must not leave a fire-on-next-scan NextRun: got %s, now %s", next, now)
	}
}

func TestScheduleNextRunAfter_CronJumpsToTheNextRealWindow(t *testing.T) {
	// 10:00, so today's 09:00 window is already missed.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	s := &schemas.Schedule{ID: "sch_cron", Type: schemas.ScheduleCron, CronExpr: "0 9 * * *"}

	next, err := ScheduleNextRunAfter(s, now)
	if err != nil {
		t.Fatalf("ScheduleNextRunAfter: %v", err)
	}
	if want := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %s, want %s — a resume must not fire today's missed 09:00", next, want)
	}
}

func TestScheduleNextRunAfter_Once(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	future := now.Add(2 * time.Hour)
	s := &schemas.Schedule{ID: "once_future", Type: schemas.ScheduleOnce, RunAt: &future}
	next, err := ScheduleNextRunAfter(s, now)
	if err != nil {
		t.Fatalf("future once: %v", err)
	}
	if !next.Equal(future) {
		t.Errorf("future once next = %s, want %s", next, future)
	}

	// A `once` whose window passed while paused has no future occurrence: zero,
	// and no error. resume surfaces this rather than activating a dead row.
	past := now.Add(-2 * time.Hour)
	s = &schemas.Schedule{ID: "once_past", Type: schemas.ScheduleOnce, RunAt: &past}
	next, err = ScheduleNextRunAfter(s, now)
	if err != nil {
		t.Fatalf("past once: %v", err)
	}
	if !next.IsZero() {
		t.Errorf("past once must report no future occurrence, got %s", next)
	}
}

func TestScheduleNextRunAfter_BadConfigIsAnErrorNotASilentZero(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	if _, err := ScheduleNextRunAfter(&schemas.Schedule{ID: "s", Type: schemas.ScheduleInterval, Interval: "not-a-duration"}, now); err == nil ||
		!strings.Contains(err.Error(), "invalid interval: not-a-duration") {
		t.Errorf("invalid interval: want the message calculateNextRun logs, got %v", err)
	}
	if _, err := ScheduleNextRunAfter(&schemas.Schedule{ID: "s", Type: schemas.ScheduleCron, CronExpr: "99 99 * * *"}, now); err == nil ||
		!strings.HasPrefix(err.Error(), "invalid cron: 99 99 * * *") {
		t.Errorf("invalid cron: want \"invalid cron: <expr> (<cause>)\", got %v", err)
	}
	if _, err := ScheduleNextRunAfter(&schemas.Schedule{ID: "s", Type: schemas.ScheduleType("weird")}, now); err == nil {
		t.Error("unknown schedule type must be an error, not a zero NextRun that scan silently ignores")
	}
}
