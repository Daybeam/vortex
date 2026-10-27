package core

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrLogSinkClosed is returned by the default sink adapters once the
// underlying writer has been closed.
var ErrLogSinkClosed = errors.New("log sink closed")

// loggerSink adapts *Logger to the LogSink extension point (A17). *Logger is
// the documented "default implementation" (JSONL files); this adapter exposes
// it through the interface so alternative backends (batched SQLite, Loki,
// ClickHouse) can be swapped at the composition root without touching Log call
// sites. Kept intentionally thin — no behavior change vs Logger.Log.
type loggerSink struct{ l *Logger }

var _ LogSink = (*loggerSink)(nil)

// NewLogSink returns the default LogSink backed by a JSONL *Logger.
func NewLogSink(l *Logger) LogSink { return &loggerSink{l: l} }

// Write enqueues a pre-built LogEvent. Best-effort, matching Logger.Log: a full
// queue drops the event (counted) and returns nil; a closed logger returns
// ErrLogSinkClosed.
func (s *loggerSink) Write(_ context.Context, ev LogEvent) error {
	if s.l == nil {
		return ErrLogSinkClosed
	}
	s.l.closedMu.RLock()
	defer s.l.closedMu.RUnlock()
	if s.l.closed {
		return ErrLogSinkClosed
	}
	select {
	case s.l.ch <- ev:
	default:
		atomic.AddUint64(&s.l.droppedEvents, 1)
	}
	return nil
}

// ReadTaskLogs delegates to the logger's JSONL reader. limit <= 0 means "no limit".
func (s *loggerSink) ReadTaskLogs(_ context.Context, taskID string, limit int) ([]map[string]any, error) {
	if s.l == nil {
		return nil, ErrLogSinkClosed
	}
	if limit > 0 {
		return s.l.ReadTaskLogs(taskID, limit)
	}
	return s.l.ReadTaskLogs(taskID)
}

// Close closes the underlying logger.
func (s *loggerSink) Close() error {
	if s.l != nil {
		s.l.Close()
	}
	return nil
}

// eventSink adapts *GlobalEventLogger to the EventSink extension point (A18).
// GlobalEventLogger is the documented default (global_trajectory.jsonl).
type eventSink struct{ l *GlobalEventLogger }

var _ EventSink = (*eventSink)(nil)

// NewEventSink returns the default EventSink backed by the JSONL GlobalEventLogger.
func NewEventSink(l *GlobalEventLogger) EventSink { return &eventSink{l: l} }

// Log queues an event for the global trajectory log.
func (s *eventSink) Log(_ context.Context, event AgentEvent) error {
	if s.l == nil {
		return ErrLogSinkClosed
	}
	return s.l.Log(event)
}

// Close closes the underlying global event logger.
func (s *eventSink) Close() error {
	if s.l != nil {
		s.l.Close()
	}
	return nil
}
