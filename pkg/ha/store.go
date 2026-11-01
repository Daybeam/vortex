package ha

import (
	"context"
	"encoding/json"
	"time"
)

// GraphStore externalises task graphs so a standby can adopt running work.
// Today the only persistence is outputs/<task>/manifest.json written by
// core/scheduler.go persistGraph and re-read by loadGraphs; the HA
// implementation stores the same JSON document in a fenced table keyed by
// task_id.
type GraphStore interface {
	// Save persists the full graph under fence. Implementations must reject a
	// fence older than the highest one accepted for taskID (ErrStaleFence).
	Save(ctx context.Context, taskID string, graph json.RawMessage, fence Fence) error
	Load(ctx context.Context, taskID string) (json.RawMessage, error)
	// ListRunning returns task ids whose graph status is running, so a new
	// leader can rehydrate them the way loadGraphs does today.
	ListRunning(ctx context.Context) ([]string, error)
}

// EventBus carries task lifecycle events across hub replicas. In-process
// doneChans stay as the fast path; the bus exists so any replica can observe a
// task and feed SSE streams after a failover.
type EventBus interface {
	Publish(ctx context.Context, ev TaskEvent) error
	// Subscribe delivers events for one task until cancel is called or ctx ends.
	Subscribe(ctx context.Context, taskID string) (<-chan TaskEvent, func(), error)
}

// TaskEvent is the transport shape of one task or step transition. Kind uses
// the telemetry event names (task_submitted, step_started, step_completed,
// task_completed, ...) so replay and SSE keep one vocabulary.
type TaskEvent struct {
	TaskID  string          `json:"task_id"`
	StepID  string          `json:"step_id,omitempty"`
	Kind    string          `json:"kind"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SignalStore backs the signal field with shared intensities. Decay is computed
// lazily on read (value * decay^(elapsed/interval)), so a standby never needs
// to run the in-memory decay ticker that main_web.go starts today.
type SignalStore interface {
	Set(ctx context.Context, layer, taskID string, value float64, fence Fence) error
	// Top returns the strongest task ids in layer after decay.
	Top(ctx context.Context, layer string, limit int) ([]SignalReading, error)
}

// SignalReading is one decayed intensity sample.
type SignalReading struct {
	TaskID    string    `json:"task_id"`
	Intensity float64   `json:"intensity"`
	UpdatedAt time.Time `json:"updated_at"`
}
