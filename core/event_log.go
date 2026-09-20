package core

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// AgentEvent represents a single atomic entry in the unified event stream.
type AgentEvent struct {
	EventID   string         `json:"event_id"`
	TaskID    string         `json:"task_id"`
	StepID    string         `json:"step_id,omitempty"`
	EventType string         `json:"event_type"` // step_start, tool_call, tool_result, decision_required, etc.
	Payload   map[string]any `json:"payload"`
	Timestamp int64          `json:"timestamp"` // Unix nano
}

// NewAgentEvent creates a new event with a generated ID and timestamp.
func NewAgentEvent(taskID, stepID, eventType string, payload map[string]any) AgentEvent {
	return AgentEvent{
		EventID:   uuid.New().String(),
		TaskID:    taskID,
		StepID:    stepID,
		EventType: eventType,
		Payload:   payload,
		Timestamp: time.Now().UnixNano(),
	}
}

// GlobalEventLogger writes events to a central trajectory log file.
type GlobalEventLogger struct {
	path    string
	ch      chan AgentEvent
	wg      sync.WaitGroup
	done    chan struct{}
	closed  uint32 // atomic flag: 1 = closed (audit finding M3)
	dropped uint64 // atomic counter for dropped events (audit finding M2)
}

// NewGlobalEventLogger initializes the logger and starts the background writer.
func NewGlobalEventLogger(logDir string) (*GlobalEventLogger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(logDir, "global_trajectory.jsonl")
	l := &GlobalEventLogger{
		path: path,
		ch:   make(chan AgentEvent, 1024),
		done: make(chan struct{}),
	}
	l.wg.Add(1)
	go l.writer()
	return l, nil
}

// Log queues an event for writing. Returns an error if the channel is full
// or the logger has been closed.
func (l *GlobalEventLogger) Log(event AgentEvent) error {
	if atomic.LoadUint32(&l.closed) == 1 {
		return fmt.Errorf("global event logger closed")
	}
	select {
	case l.ch <- event:
		return nil
	default:
		atomic.AddUint64(&l.dropped, 1)
		return fmt.Errorf("global event logger channel full")
	}
}

// DroppedEvents returns the number of events dropped due to a full channel.
func (l *GlobalEventLogger) DroppedEvents() uint64 {
	return atomic.LoadUint64(&l.dropped)
}

// Close flushes the channel and stops the writer. Safe to call concurrently
// with Log — the atomic closed flag prevents send-on-closed-channel panics
// (audit finding M3).
func (l *GlobalEventLogger) Close() {
	if !atomic.CompareAndSwapUint32(&l.closed, 0, 1) {
		return
	}
	close(l.ch)
	l.wg.Wait()
}

func (l *GlobalEventLogger) writer() {
	defer l.wg.Done()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GlobalEventLogger: Failed to open %s: %v\n", l.path, err)
		return
	}
	defer f.Close()

	for ev := range l.ch {
		data, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		data = append(data, '\n')
		if _, werr := f.Write(data); werr != nil {
			log.Printf("WARN: GlobalEventLogger: failed to write event: %v", werr)
		}
	}
}

// HookEventBus attaches the global logger to an EventBus.
func (l *GlobalEventLogger) HookEventBus(bus *EventBus) {
	bus.Subscribe("*", func(event AgentEvent) error {
		return l.Log(event)
	})
}
