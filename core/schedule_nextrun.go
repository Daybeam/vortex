package core

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/core/cron"
	"github.com/daybeam/vortex/schemas"
)

// ScheduleNextRunAfter returns the next occurrence of s strictly after now.
//
// It is the computation ScheduleManager.calculateNextRun performs after every
// fire, lifted out of the manager because register O5 / decision Q4-1 need it
// from the tool layer: a paused row keeps a NextRun that is already in the
// past, so an honest resume is "recompute", not "flip the status flag" —
// flipping alone makes the next scan fire a catch-up run the operator paused
// precisely to avoid.
//
// Pure — no receiver, no shared state — so a tool handler goroutine can call it
// while the 30s scan ticker runs without touching ScheduleManager.Mu.
//
// A zero time with a nil error means "no future occurrence exists" (a `once`
// schedule whose run_at has passed). Callers must surface that instead of
// activating a row that the scan will never fire.
//
// calculateNextRun is the one caller that overrides the `once` branch, because
// it runs *after* a fire: see the comment there.
func ScheduleNextRunAfter(s *schemas.Schedule, now time.Time) (time.Time, error) {
	switch s.Type {
	case schemas.ScheduleOnce:
		if s.RunAt != nil && s.RunAt.After(now) {
			return *s.RunAt, nil
		}
		return time.Time{}, nil
	case schemas.ScheduleInterval:
		d, err := time.ParseDuration(s.Interval)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid interval: %s", s.Interval)
		}
		return now.Add(d), nil
	case schemas.ScheduleCron:
		next, err := cron.NextRun(s.CronExpr, now)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron: %s (%v)", s.CronExpr, err)
		}
		return next, nil
	}
	return time.Time{}, fmt.Errorf("unknown schedule type: %s", s.Type)
}
