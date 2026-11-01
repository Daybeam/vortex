package obs

import "context"

// LogSink is the extension point for log event persistence (A17).
// The default implementation writes JSONL files (obs/logger.go).
// Future implementations: batched SQLite, Loki, ClickHouse, etc.
// High-volume append-only — do NOT route through the main SQLite DB
// (SetMaxOpenConns(1) would serialize writers).
type LogSink interface {
	Write(ctx context.Context, ev LogEvent) error
	ReadTaskLogs(ctx context.Context, taskID string, limit int) ([]map[string]any, error)
	Close() error
}

// EventSink is the extension point for global event trajectory persistence (A18).
// The default implementation appends to global_trajectory.jsonl (obs/event_log.go).
// Future implementations: SQLite batched, Kafka, event streaming platforms.
// High-volume append-only — same concern as LogSink.
type EventSink interface {
	Log(ctx context.Context, event AgentEvent) error
	Close() error
}
