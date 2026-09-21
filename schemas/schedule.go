package schemas

import (
	"time"
)

type ScheduleType string

const (
	ScheduleOnce     ScheduleType = "once"
	ScheduleInterval ScheduleType = "interval"
	ScheduleCron     ScheduleType = "cron"
)

type ScheduleStatus string

const (
	ScheduleActive    ScheduleStatus = "active"
	SchedulePaused    ScheduleStatus = "paused"
	ScheduleCompleted ScheduleStatus = "completed"
)

type OnFailurePolicy string

const (
	FailureIgnore          OnFailurePolicy = "ignore"
	FailureRetryNextWindow OnFailurePolicy = "retry_next_window"
	FailureAlert           OnFailurePolicy = "alert"
)

// Schedule defines a recurring or delayed task execution.
type Schedule struct {
	ID   string       `json:"id"`
	Name string       `json:"name"`
	Type ScheduleType `json:"type"` // "once" | "interval" | "cron"

	// Configuration based on Type
	RunAt    *time.Time `json:"run_at,omitempty"`    // For "once"
	Interval string     `json:"interval,omitempty"`  // For "interval", e.g. "6h", "30m"
	CronExpr string     `json:"cron_expr,omitempty"` // For "cron", e.g. "0 9 * * MON-FRI"

	TaskInputs []StepInput `json:"task_inputs"` // Re-using StepInput from Scheduler

	// Runtime state
	Status   ScheduleStatus `json:"status"`
	MaxRuns  int            `json:"max_runs"` // 0 for unlimited
	RunCount int            `json:"run_count"`
	LastRun  *time.Time     `json:"last_run,omitempty"`
	NextRun  time.Time      `json:"next_run"`

	// Error handling
	OnFailure           OnFailurePolicy `json:"on_failure"` // ignore | retry_next_window | alert
	ConsecutiveFailures int             `json:"consecutive_failures"`

	CreatedAt time.Time `json:"created_at"`
}

// StepInput is copied from Scheduler to avoid circular dependency if needed,
// or moved to schemas if it's shared. In this project, Scheduler.StepInput
// is the primary entry point.
// Note: We should probably move StepInput to schemas/task.go if not already there.
// Checking core/scheduler.go again... it's defined there. Let's move it to schemas.
