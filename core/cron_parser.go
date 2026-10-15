package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronField represents a parsed cron field with acceptable values.
// Supports: *, exact values, ranges (a-b), steps (*/n), lists (a,b,c).
type cronField struct {
	values map[int]bool // set of matching values
}

// cronSchedule holds 5 parsed fields for next-run calculation.
type cronSchedule struct {
	minute  cronField // 0-59
	hour    cronField // 0-23
	day     cronField // 1-31
	month   cronField // 1-12
	weekday cronField // 0-6 (0=Sunday)
	// audit L-MED-1: standard cron semantics — when both day and weekday are
	// restricted (not "*"), match if EITHER matches (OR). When at least one is
	// "*", match only if BOTH match (AND).
	dayRestricted     bool
	weekdayRestricted bool
}

var (
	cronFieldRanges = []struct{ min, max int }{
		{0, 59}, // minute
		{0, 23}, // hour
		{1, 31}, // day
		{1, 12}, // month
		{0, 6},  // weekday
	}
)

// parseCronField parses a single cron field expression.
// Supports: *, n, a-b, a-b/n, */n, a,b,c (combinations of the above).
func parseCronField(expr string, fieldIdx int) (cronField, error) {
	r := cronFieldRanges[fieldIdx]
	cf := cronField{values: make(map[int]bool)}

	if expr == "*" {
		for v := r.min; v <= r.max; v++ {
			cf.values[v] = true
		}
		return cf, nil
	}

	for _, part := range strings.Split(expr, ",") {
		if err := parseCronPart(part, fieldIdx, r.min, r.max, &cf); err != nil {
			return cronField{}, err
		}
	}
	return cf, nil
}

func parseCronPart(part string, fieldIdx, min, max int, cf *cronField) error {
	// Handle step: */n or a-b/n
	step := 1
	rangePart := part
	if idx := strings.Index(part, "/"); idx >= 0 {
		var err error
		step, err = strconv.Atoi(part[idx+1:])
		if err != nil || step <= 0 {
			return fmt.Errorf("invalid step in %q", part)
		}
		rangePart = part[:idx]
	}

	lo, hi := min, max
	if rangePart == "*" {
		// */n — all values in range with step
	} else if strings.Contains(rangePart, "-") {
		dashParts := strings.SplitN(rangePart, "-", 2)
		var err error
		lo, err = strconv.Atoi(dashParts[0])
		if err != nil || lo < min || lo > max {
			return fmt.Errorf("invalid range start in %q", part)
		}
		hi, err = strconv.Atoi(dashParts[1])
		if err != nil || hi < min || hi > max || hi < lo {
			return fmt.Errorf("invalid range end in %q", part)
		}
	} else {
		val, err := strconv.Atoi(rangePart)
		if err != nil || val < min || val > max {
			return fmt.Errorf("invalid value %q in field (range %d-%d)", rangePart, min, max)
		}
		lo = val
		hi = val
	}

	for v := lo; v <= hi; v += step {
		cf.values[v] = true
	}
	return nil
}

// parseCronSchedule parses a full 5-field cron expression.
func parseCronSchedule(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields, got %d", len(fields))
	}

	sched := &cronSchedule{}
	var err error

	sched.minute, err = parseCronField(fields[0], 0)
	if err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	sched.hour, err = parseCronField(fields[1], 1)
	if err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	sched.day, err = parseCronField(fields[2], 2)
	if err != nil {
		return nil, fmt.Errorf("day field: %w", err)
	}
	sched.month, err = parseCronField(fields[3], 3)
	if err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	sched.weekday, err = parseCronField(fields[4], 4)
	if err != nil {
		return nil, fmt.Errorf("weekday field: %w", err)
	}

	// audit L-MED-1: track whether day/weekday are restricted (not "*") for OR semantics.
	sched.dayRestricted = fields[2] != "*"
	sched.weekdayRestricted = fields[4] != "*"

	return sched, nil
}

// nextRun calculates the next time >= from that matches the cron schedule.
// Iterates minute-by-minute up to 366 days (handles yearly schedules).
func (s *cronSchedule) nextRun(from time.Time) time.Time {
	// Start from the next minute boundary, truncated to minute.
	t := from.Add(time.Minute).Truncate(time.Minute)
	maxIter := 366 * 24 * 60 // ~527k iterations max (366 days)

	for i := 0; i < maxIter; i++ {
		if s.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	// No match within 366 days — return zero value (caller handles fallback).
	return time.Time{}
}

func (s *cronSchedule) matches(t time.Time) bool {
	_, month, day := t.Date()
	weekday := int(t.Weekday()) // Sunday=0, Monday=1, ..., Saturday=6
	hour := t.Hour()
	minute := t.Minute()

	if !s.minute.values[minute] || !s.hour.values[hour] || !s.month.values[int(month)] {
		return false
	}
	// audit L-MED-1: standard cron semantics — when both day-of-month and
	// day-of-week are restricted (not "*"), match if EITHER matches (OR).
	// When at least one is "*", match only if BOTH match (AND).
	if s.dayRestricted && s.weekdayRestricted {
		return s.day.values[day] || s.weekday.values[weekday]
	}
	return s.day.values[day] && s.weekday.values[weekday]
}
