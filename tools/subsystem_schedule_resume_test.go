package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression test for register O5, decision Q4-1 (2026-11-05).
//
// Bug shape: `schedule` could pause a row through MCP and nothing on that
// surface could resume it. Even had a caller flipped the status by hand, the
// paused row keeps a NextRun that is already in the past, so
// ScheduleManager.scan (`!NextRun.IsZero() && now.After(NextRun)`) fires a
// catch-up run the instant the row goes active — one run the operator paused
// precisely to avoid.
//
// The user's ruling: resume means *skip*, not catch up. Scheduled runs usually
// spend tokens, so a burst on resume is the harm, not the feature.

func newResumeTestApp(t *testing.T) *App {
	t.Helper()
	app := &App{Tier: TierAdmin, SStore: store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)}
	registerScheduleSubsystem(app)
	return app
}

func putPausedSchedule(t *testing.T, app *App, sch *schemas.Schedule) {
	t.Helper()
	app.SStore.Set(sch)
	got := app.SStore.Get(sch.ID)
	if got == nil {
		t.Fatalf("setup: schedule %q did not persist", sch.ID)
	}
	if got.Status != schemas.SchedulePaused {
		t.Fatalf("setup: schedule %q status = %q, want paused", sch.ID, got.Status)
	}
}

// TestScheduleResume_ActivatesWithoutFiring is the whole point of the answer:
// the row comes back, and its next fire is a window in the future.
func TestScheduleResume_ActivatesWithoutFiring(t *testing.T) {
	app := newResumeTestApp(t)
	stale := time.Now().Add(-5 * time.Hour) // six 1h windows already missed
	putPausedSchedule(t, app, &schemas.Schedule{
		ID:       "sch_interval",
		Name:     "hourly",
		Type:     schemas.ScheduleInterval,
		Interval: "1h",
		Status:   schemas.SchedulePaused,
		NextRun:  stale,
	})

	before := time.Now()
	res, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{"schedule_id": "sch_interval"})
	if err != nil {
		t.Fatalf("resume: %v (result=%v)", err, res)
	}
	got := app.SStore.Get("sch_interval")
	if got.Status != schemas.ScheduleActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if !got.NextRun.After(before) {
		t.Errorf("O5/Q4-1: resume left NextRun=%s at or before now=%s — scan would fire a catch-up run", got.NextRun, before)
	}
	if got.NextRun.After(before.Add(90 * time.Minute)) {
		t.Errorf("NextRun=%s is more than one 1h window ahead of now=%s", got.NextRun, before)
	}
	if !got.NextRun.After(stale) {
		t.Errorf("NextRun must move forward past the missed windows, got %s (was %s)", got.NextRun, stale)
	}
}

// TestScheduleResume_CronSkipsToTheNextWindow pins the same rule for cron rows,
// which are the ones that can accumulate a backlog.
func TestScheduleResume_CronSkipsToTheNextWindow(t *testing.T) {
	app := newResumeTestApp(t)
	stale := time.Now().Add(-26 * time.Hour)
	putPausedSchedule(t, app, &schemas.Schedule{
		ID:       "sch_cron",
		Type:     schemas.ScheduleCron,
		CronExpr: "0 * * * *",
		Status:   schemas.SchedulePaused,
		NextRun:  stale,
	})

	if _, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{"schedule_id": "sch_cron"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := app.SStore.Get("sch_cron")
	if !got.NextRun.After(time.Now()) {
		t.Errorf("a 26-missed-hour cron row came back due immediately: NextRun=%s", got.NextRun)
	}
	if d := time.Until(got.NextRun); d > time.Hour {
		t.Errorf("NextRun=%s is further out than the next hourly window", got.NextRun)
	}
}

// TestScheduleResume_PastOnceIsRefusedAndStaysPaused: a `once` whose window
// passed while paused has nothing to skip *to*. Activating it would create a row
// the scan can never fire, so the action refuses and leaves the row as found.
func TestScheduleResume_PastOnceIsRefusedAndStaysPaused(t *testing.T) {
	app := newResumeTestApp(t)
	past := time.Now().Add(-2 * time.Hour)
	putPausedSchedule(t, app, &schemas.Schedule{
		ID:      "sch_once",
		Type:    schemas.ScheduleOnce,
		RunAt:   &past,
		Status:  schemas.SchedulePaused,
		NextRun: past,
	})

	res, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{"schedule_id": "sch_once"})
	if err == nil {
		t.Fatalf("resuming an already-expired `once` must not report ok: %v", res)
	}
	if !strings.Contains(err.Error(), "never fire") {
		t.Errorf("error must say why there is no window to skip to, got: %v", err)
	}
	if got := app.SStore.Get("sch_once"); got.Status != schemas.SchedulePaused {
		t.Errorf("refused resume must leave the row paused, got %q", got.Status)
	}
}

// TestScheduleResume_FutureOnceKeepsItsWindow is the other half: pausing before
// a `once` fires must not lose the scheduled run.
func TestScheduleResume_FutureOnceKeepsItsWindow(t *testing.T) {
	app := newResumeTestApp(t)
	future := time.Now().Add(3 * time.Hour)
	putPausedSchedule(t, app, &schemas.Schedule{
		ID:      "sch_once_future",
		Type:    schemas.ScheduleOnce,
		RunAt:   &future,
		Status:  schemas.SchedulePaused,
		NextRun: future,
	})

	if _, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{"schedule_id": "sch_once_future"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := app.SStore.Get("sch_once_future"); !got.NextRun.Equal(future) {
		t.Errorf("NextRun = %s, want the still-future run_at %s", got.NextRun, future)
	}
}

// TestScheduleResume_BadConfigIsNotHalfApplied: an unparseable interval must
// fail before the status flip, not leave an active row with a zero NextRun.
func TestScheduleResume_BadConfigIsNotHalfApplied(t *testing.T) {
	app := newResumeTestApp(t)
	putPausedSchedule(t, app, &schemas.Schedule{
		ID:       "sch_broken",
		Type:     schemas.ScheduleInterval,
		Interval: "every-hour",
		Status:   schemas.SchedulePaused,
		NextRun:  time.Now().Add(-time.Hour),
	})

	res, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{"schedule_id": "sch_broken"})
	if err == nil {
		t.Fatalf("resume accepted an unparseable interval: %v", res)
	}
	if got := app.SStore.Get("sch_broken"); got.Status != schemas.SchedulePaused {
		t.Errorf("a refused resume must not activate the row, status = %q", got.Status)
	}
}

func TestScheduleResume_MissingAndUnknownID(t *testing.T) {
	app := newResumeTestApp(t)
	if _, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{"schedule_id": "sch_nope"}); err == nil {
		t.Error("resume of an unknown schedule must fail")
	}
	if _, err := registry.Invoke(adminCtx(), app, "schedule", "resume", map[string]any{}); err == nil {
		t.Error("resume without schedule_id must fail rather than resume a random row")
	}
}

// TestScheduleResume_IsAdminOnly mirrors schedule.pause: only `list` is Tier1.
func TestScheduleResume_IsAdminOnly(t *testing.T) {
	app := newResumeTestApp(t)
	putPausedSchedule(t, app, &schemas.Schedule{
		ID:       "sch_public_try",
		Type:     schemas.ScheduleInterval,
		Interval: "1h",
		Status:   schemas.SchedulePaused,
		NextRun:  time.Now().Add(-time.Hour),
	})

	ctx := context.WithValue(context.Background(), AuthTierKey, TierPublic)
	if _, err := registry.Invoke(ctx, app, "schedule", "resume", map[string]any{"schedule_id": "sch_public_try"}); err == nil {
		t.Error("resume must require Tier2 (Admin), like pause")
	}
	if got := app.SStore.Get("sch_public_try"); got != nil && got.Status == schemas.ScheduleActive {
		t.Error("the tier gate must reject before the handler runs — the row was activated")
	}
}
