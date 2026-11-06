package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// resumePausedSchedule is the body of the `schedule.resume` action (register
// O5, decision Q4-1). It lives in its own file because registerScheduleSubsystem
// was 152 lines with pause/delete/reload already in it — the Q6-1 rule is that
// new behavior goes into a helper, not into the growing function.
//
// Resume means "skip what the pause swallowed", never "catch up": a paused row
// keeps a NextRun in the past, and scan fires on `now.After(NextRun)`, so a
// plain status flip would burn a burst of runs the moment the row woke up.
// resumeAction is the `schedule.resume` entry of the schedule subsystem. It is
// built here rather than inline because registerScheduleSubsystem sat at 145 of
// its 150-line budget before this action existed — the Q6-1 rule is that new
// behavior goes into a helper, not into the growing function.
func resumeAction() Action {
	return Action{
		Name:        "resume",
		Description: "Resume a paused schedule, skipping the ticks it missed: NextRun is recomputed to the first window after now, so nothing fires immediately and no backlog is replayed.",
		Parameters:  map[string]any{"schedule_id": "string"},
		Handler:     resumePausedSchedule,
	}
}

func resumePausedSchedule(_ context.Context, app *App, args map[string]any) (any, error) {
	id := strArg(args, "schedule_id")
	sch := app.SStore.Get(id)
	if sch == nil {
		return nil, fmt.Errorf("not found")
	}
	prev := sch.NextRun
	next, err := core.ScheduleNextRunAfter(sch, time.Now())
	if err != nil {
		// Refuse instead of half-applying: an active row with a stale or zero
		// NextRun is the failure this action exists to prevent.
		return nil, fmt.Errorf("cannot compute the next window for %s: %w", id, err)
	}
	if next.IsZero() {
		return nil, fmt.Errorf("schedule %s is a `once` whose run_at has already passed (%s): resuming it would leave a row that can never fire. Create it again with a future run_at", id, prev.Format(time.RFC3339))
	}
	// Write a copy, not the row in place: ScheduleStore.Get hands back the same
	// *schemas.Schedule the 30s scan loop is reading (register O6 obstacle 2),
	// and a torn NextRun read mid-scan is what pausing must not leave behind.
	// Set swaps the map entry instead of mutating it.
	updated := *sch
	updated.Status = schemas.ScheduleActive
	updated.NextRun = next
	app.SStore.Set(&updated)

	var prevRun any
	if !prev.IsZero() {
		prevRun = prev.Format(time.RFC3339)
	}
	return map[string]any{
		"ok":                true,
		"schedule_id":       id,
		"previous_next_run": prevRun,
		"next_run":          next.Format(time.RFC3339),
		"note":              "the window(s) between previous_next_run and next_run are skipped, not replayed",
	}, nil
}
