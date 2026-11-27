package schemas

import (
	"fmt"
	"net/url"
	"strings"
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

	Deliver          DeliverTarget `json:"deliver,omitempty"`
	OwnerID          string        `json:"owner_id,omitempty"`
	SubmitterIsAdmin bool          `json:"submitter_is_admin,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// ScheduleDelivery is the immutable per-run delivery description carried by a
// task. It lets completion recovery enqueue the result without the Schedule row.
type ScheduleDelivery struct {
	ScheduleID string        `json:"schedule_id"`
	Name       string        `json:"name,omitempty"`
	Target     DeliverTarget `json:"target"`
	OwnerID    string        `json:"owner_id,omitempty"`
}

// DeliverTarget specifies where a scheduled task's result should be delivered.
// Resolved at trigger time, not at creation time — sessions may be archived or
// deleted between schedule creation and firing.
type DeliverTarget struct {
	Mode       string `json:"mode"` // session_current | session_new | webhook
	SessionID  string `json:"session_id,omitempty"`
	WebhookURL string `json:"webhook_url,omitempty"`
	OnMissing  string `json:"on_missing"`       // webhook | inbox (drop is not accepted)
	Prompt     string `json:"prompt,omitempty"` // session_new first-turn prompt
}

func ValidateDeliverTarget(target DeliverTarget) error {
	if target.OnMissing != "" && target.OnMissing != "webhook" && target.OnMissing != "inbox" {
		return fmt.Errorf("deliver.on_missing must be webhook or inbox")
	}
	if target.OnMissing == "webhook" {
		if err := validateWebhookTarget(target.WebhookURL); err != nil {
			return fmt.Errorf("deliver.on_missing=webhook: %w", err)
		}
	}
	switch target.Mode {
	case "":
		return nil
	case "session_current":
		if strings.TrimSpace(target.SessionID) == "" {
			return fmt.Errorf("deliver.session_id is required for session_current")
		}
		return nil
	case "webhook":
		return validateWebhookTarget(target.WebhookURL)
	case "session_new":
		return fmt.Errorf("deliver.mode=session_new is not implemented")
	default:
		return fmt.Errorf("unsupported deliver.mode %q", target.Mode)
	}
}

func validateWebhookTarget(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("webhook_url must be an https URL without credentials")
	}
	return nil
}

// StepInput is copied from Scheduler to avoid circular dependency if needed,
// or moved to schemas if it's shared. In this project, Scheduler.StepInput
// is the primary entry point.
// Note: We should probably move StepInput to schemas/task.go if not already there.
// Checking core/scheduler.go again... it's defined there. Let's move it to schemas.
