package core

import (
	"testing"
	"time"
)

func TestCronParser_EveryMinute(t *testing.T) {
	sched, err := parseCronSchedule("* * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 1, 15, 10, 30, 45, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 10, 31, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("every-minute: got %v, want %v", next, want)
	}
}

func TestCronParser_EveryHour(t *testing.T) {
	sched, err := parseCronSchedule("0 * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("every-hour: got %v, want %v", next, want)
	}
}

func TestCronParser_WeekdayMorning(t *testing.T) {
	// "0 9 * * 1-5" — 9am on weekdays (Mon-Fri)
	sched, err := parseCronSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Wednesday 3pm → next is Thursday 9am
	from := time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC) // Thursday
	// Actually 2026-01-15 is a Thursday. Let's use a Wednesday.
	from = time.Date(2026, 1, 14, 15, 0, 0, 0, time.UTC) // Wednesday
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC) // Thursday 9am
	if !next.Equal(want) {
		t.Errorf("weekday-morning: got %v, want %v", next, want)
	}
}

func TestCronParser_FridayToMonday(t *testing.T) {
	// "0 9 * * 1-5" — 9am on weekdays
	// Friday 10am → next is Monday 9am (skip weekend)
	sched, err := parseCronSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2026-01-16 is Friday
	from := time.Date(2026, 1, 16, 10, 0, 0, 0, time.UTC)
	next := sched.nextRun(from)
	// 2026-01-19 is Monday
	want := time.Date(2026, 1, 19, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("fri-to-mon: got %v, want %v", next, want)
	}
}

func TestCronParser_SpecificTime(t *testing.T) {
	// "30 14 * * *" — 2:30pm every day
	sched, err := parseCronSchedule("30 14 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 14, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("specific-time: got %v, want %v", next, want)
	}
}

func TestCronParser_StepEvery5Minutes(t *testing.T) {
	// "*/5 * * * *" — every 5 minutes
	sched, err := parseCronSchedule("*/5 * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 1, 15, 10, 3, 0, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 10, 5, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("step-5min: got %v, want %v", next, want)
	}
}

func TestCronParser_ListValues(t *testing.T) {
	// "0,30 * * * *" — at minute 0 and 30 of every hour
	sched, err := parseCronSchedule("0,30 * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 1, 15, 10, 15, 0, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("list-values: got %v, want %v", next, want)
	}
}

func TestCronParser_MonthSpecific(t *testing.T) {
	// "0 0 1 1,7 *" — midnight on Jan 1 and Jul 1
	sched, err := parseCronSchedule("0 0 1 1,7 *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 2, 15, 10, 0, 0, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("month-specific: got %v, want %v", next, want)
	}
}

func TestCronParser_RangeWithStep(t *testing.T) {
	// "0 9-17/2 * * 1-5" — every 2 hours from 9am to 5pm on weekdays
	sched, err := parseCronSchedule("0 9-17/2 * * 1-5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Wednesday 10am → next is 11am (9, 11, 13, 15, 17)
	from := time.Date(2026, 1, 14, 10, 0, 0, 0, time.UTC) // Wednesday
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 14, 11, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("range-step: got %v, want %v", next, want)
	}
}

func TestCronParser_InvalidField(t *testing.T) {
	_, err := parseCronSchedule("60 * * * *")
	if err == nil {
		t.Error("should reject minute=60")
	}
	_, err = parseCronSchedule("* 25 * * *")
	if err == nil {
		t.Error("should reject hour=25")
	}
	_, err = parseCronSchedule("* * * 13 *")
	if err == nil {
		t.Error("should reject month=13")
	}
}

func TestCronParser_FourFields(t *testing.T) {
	_, err := parseCronSchedule("* * * *")
	if err == nil {
		t.Error("should reject 4-field expression")
	}
}

func TestCronParser_SundayWeekday(t *testing.T) {
	// "0 10 * * 0" — 10am every Sunday
	sched, err := parseCronSchedule("0 10 * * 0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2026-01-16 is Friday → next Sunday is 2026-01-18
	from := time.Date(2026, 1, 16, 11, 0, 0, 0, time.UTC)
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 18, 10, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("sunday: got %v, want %v", next, want)
	}
}

// TestCronParser_DayAndWeekdayORSemantics verifies standard cron behavior:
// when both day-of-month and day-of-week are restricted (not "*"), the match
// is OR-based (fire if EITHER matches). audit L-MED-1 regression test.
func TestCronParser_DayAndWeekdayORSemantics(t *testing.T) {
	// "0 0 15 * 1" — midnight on the 15th OR every Monday.
	// 2026-01-14 is Wednesday. Next match should be 2026-01-15 (Thursday, the 15th),
	// NOT the next Monday (2026-01-19), because the 15th comes first.
	sched, err := parseCronSchedule("0 0 15 * 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC) // Wednesday noon
	next := sched.nextRun(from)
	want := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) // 15th at midnight
	if !next.Equal(want) {
		t.Errorf("day-weekday OR: got %v, want %v (15th should fire before next Monday)", next, want)
	}

	// Also verify a Monday fires when the 15th has passed.
	// 2026-01-16 is Friday. Next match should be 2026-01-19 (Monday),
	// NOT 2026-02-15 (next 15th), because Monday comes first.
	from2 := time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC) // Friday noon
	next2 := sched.nextRun(from2)
	want2 := time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC) // Monday at midnight
	if !next2.Equal(want2) {
		t.Errorf("day-weekday OR (monday): got %v, want %v (Monday should fire before next 15th)", next2, want2)
	}
}
