package core

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daybeam/vortex/config"
)

type EventType string

const (
	EventTaskSubmitted           EventType = "task_submitted"
	EventStepStarted             EventType = "step_started"
	EventStepCompleted           EventType = "step_completed"
	EventStepPartial             EventType = "step_partial"
	EventStepFailed              EventType = "step_failed"
	EventStepRetrying            EventType = "step_retrying"
	EventRoleMissing             EventType = "role_missing"
	EventRateLimited             EventType = "rate_limited"
	EventDecisionRequired        EventType = "decision_required"
	EventDecisionSubmitted       EventType = "decision_submitted"
	EventTaskCompleted           EventType = "task_completed"
	EventTaskFailed              EventType = "task_failed"
	EventProviderCallStarted     EventType = "provider_call_started"
	EventProviderFallback        EventType = "provider_fallback"
	EventSwarmWatchdogTriggered  EventType = "swarm_watchdog_triggered"
	EventAdminFileDeployed       EventType = "admin_file_deployed"
	EventRoleSynthesisSuppressed EventType = "RoleSynthesisSuppressed"
	EventUISuspendRequested      EventType = "ui_suspend_requested"
	EventProviderCooldownSkip    EventType = "provider_cooldown_skip"
	EventRateLimitWaitStarted    EventType = "rate_limit_wait_started"
	EventRateLimitWaitCompleted  EventType = "rate_limit_wait_completed"
	EventRateLimitWaitFailed     EventType = "rate_limit_wait_failed"
	EventToolCall                EventType = "tool_call"
	EventToolPromoted            EventType = "tool_promoted"
	EventHumanApprovalAttached   EventType = "human_approval_attached"
	EventFallbackActivated       EventType = "fallback_activated"
	EventMCPDegraded             EventType = "mcp_degraded"
	EventStepDeprecated          EventType = "step_deprecated"          // ADDED (2026-09-07): DAG surgery
	EventStepInjected            EventType = "step_injected"            // ADDED (2026-09-07): DAG surgery
	EventAutonomousStepSpawned   EventType = "autonomous_step_spawned"  // ASAE (ADDED 2026-09-09)
	EventDynamicRubricGen        EventType = "dynamic_rubric_gen"       // Phase 2 (ADDED 2026-09-09)
	EventCacheInefficiency       EventType = "cache_inefficiency"       // Cost governance (ADDED 2026-09-14)
	EventBudgetPredictExceed     EventType = "budget_predict_exceed"    // Cost governance (ADDED 2026-09-14)
	EventBehaviorDeviation       EventType = "behavior_deviation"       // Cost governance (ADDED 2026-09-14)
	EventDebateStarted           EventType = "debate_started"           // Cross-family debate (ADDED 2026-09-14)
	EventDebateCritiqueReceived  EventType = "debate_critique_received" // Cross-family debate (ADDED 2026-09-14)
	EventDebateConcluded         EventType = "debate_concluded"         // Cross-family debate (ADDED 2026-09-14)
)

type LogEvent struct {
	Event      EventType      `json:"event"`
	TaskID     string         `json:"task_id"`
	StepID     string         `json:"step_id,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	SpanID     string         `json:"span_id,omitempty"`
	Confidence *float64       `json:"confidence,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	TS         float64        `json:"ts"`
}

type Logger struct {
	logDir   string
	ch       chan LogEvent
	Mu       sync.Mutex
	wg       sync.WaitGroup
	closed   bool
	closedMu sync.RWMutex
	System   *config.SystemSettings
	// droppedEvents counts log events dropped because the channel was full
	// (audit finding H6 — non-blocking send prevents deadlock under backpressure).
	droppedEvents uint64
	// SSE subscribers (ADDED 2026-09-14): each subscriber gets a copy of events.
	// See docs/architecture/TIME_TRAVEL_AND_OBSERVABILITY_DESIGN.md
	subMu    sync.RWMutex
	subChans []chan LogEvent
	// dirCache tracks directories already created by MkdirAll, avoiding a
	// stat syscall per log event. Only accessed from the writer goroutine.
	dirCache map[string]bool
	// fileSizeCache tracks approximate file sizes for rotation check,
	// avoiding a Stat syscall per log event. Only accessed from the writer goroutine.
	// Capped at maxFileSizeCacheEntries to prevent unbounded growth on long-running
	// servers (one entry per unique task ID). When exceeded, the cache is cleared
	// (safe — it is only an optimization; cleared entries trigger a one-time Stat).
	fileSizeCache           map[string]int64
	maxFileSizeCacheEntries int
}

const defaultMaxFileSizeCacheEntries = 10000

func NewLogger(logDir string, sys *config.SystemSettings) (*Logger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}
	l := &Logger{
		logDir:                  logDir,
		ch:                      make(chan LogEvent, 512),
		System:                  sys,
		dirCache:                make(map[string]bool),
		fileSizeCache:           make(map[string]int64),
		maxFileSizeCacheEntries: defaultMaxFileSizeCacheEntries,
	}

	// Purge old logs on startup (keep last 7 days)
	l.PurgeLogs(7)

	l.wg.Add(1)
	go l.writer()
	return l, nil
}

func (l *Logger) Log(event EventType, taskID, stepID string, detail map[string]any) {
	l.closedMu.RLock()
	defer l.closedMu.RUnlock()
	if l.closed {
		return
	}
	select {
	case l.ch <- LogEvent{
		Event:  event,
		TaskID: taskID,
		StepID: stepID,
		Detail: detail,
		TS:     float64(time.Now().UnixNano()) / 1e9,
	}:
	default:
		atomic.AddUint64(&l.droppedEvents, 1)
	}
}

func (l *Logger) LogCtx(ctx context.Context, event EventType, taskID, stepID string, detail map[string]any) {
	l.closedMu.RLock()
	defer l.closedMu.RUnlock()
	if l.closed {
		return
	}
	select {
	case l.ch <- LogEvent{
		Event:   event,
		TaskID:  taskID,
		StepID:  stepID,
		TraceID: GetTraceID(ctx),
		SpanID:  GetSpanID(ctx),
		Detail:  detail,
		TS:      float64(time.Now().UnixNano()) / 1e9,
	}:
	default:
		atomic.AddUint64(&l.droppedEvents, 1)
	}
}

func (l *Logger) LogWithConfidence(event EventType, taskID, stepID string, confidence float64, detail map[string]any) {
	l.closedMu.RLock()
	defer l.closedMu.RUnlock()
	if l.closed {
		return
	}
	c := confidence
	ev := LogEvent{
		Event:      event,
		TaskID:     taskID,
		StepID:     stepID,
		Confidence: &c,
		Detail:     detail,
		TS:         float64(time.Now().UnixNano()) / 1e9,
	}
	select {
	case l.ch <- ev:
	default:
		atomic.AddUint64(&l.droppedEvents, 1)
	}
}

// DroppedEvents returns the number of log events dropped due to a full channel.
func (l *Logger) DroppedEvents() uint64 {
	return atomic.LoadUint64(&l.droppedEvents)
}

func (l *Logger) Close() {
	l.closedMu.Lock()
	if l.closed {
		l.closedMu.Unlock()
		return
	}
	l.closed = true
	close(l.ch)
	l.closedMu.Unlock()
	l.wg.Wait()
}

// SubscribeEvents returns a channel that receives a copy of every log event.
// Used by the SSE /api/events endpoint for real-time observability.
// The caller must call UnsubscribeEvents when done to avoid leaks.
func (l *Logger) SubscribeEvents() <-chan LogEvent {
	ch := make(chan LogEvent, 128)
	l.subMu.Lock()
	l.subChans = append(l.subChans, ch)
	l.subMu.Unlock()
	return ch
}

// UnsubscribeEvents removes a subscriber channel and closes it.
func (l *Logger) UnsubscribeEvents(ch <-chan LogEvent) {
	l.subMu.Lock()
	for i, sub := range l.subChans {
		if sub == ch {
			l.subChans = append(l.subChans[:i], l.subChans[i+1:]...)
			close(sub)
			break
		}
	}
	l.subMu.Unlock()
}

func (l *Logger) writer() {
	defer l.wg.Done()
	for ev := range l.ch {
		l.write(ev)
	}
}

func (l *Logger) write(ev LogEvent) {
	// Fan-out to SSE subscribers (non-blocking)
	l.subMu.RLock()
	for _, ch := range l.subChans {
		select {
		case ch <- ev:
		default: // skip if subscriber is slow
		}
	}
	l.subMu.RUnlock()

	date := time.Now().UTC().Format("2006-01-02")
	dir := filepath.Join(l.logDir, date)
	if !l.dirCache[dir] {
		_ = os.MkdirAll(dir, 0755)
		l.dirCache[dir] = true
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.jsonl", ev.TaskID))

	// LMP: Size-based rotation (MaxLogSizeMB threshold)
	maxLogSize := int64(100 * 1024 * 1024)
	if l.System != nil && l.System.MaxLogSizeMB > 0 {
		maxLogSize = int64(l.System.MaxLogSizeMB) * 1024 * 1024
	}
	// Use in-memory size tracking instead of os.Stat per log event.
	// Only call Stat on the first write to a file (to sync with any
	// pre-existing content) or when the in-memory size exceeds the threshold.
	cachedSize, hasSize := l.fileSizeCache[path]
	if !hasSize {
		// Evict entire cache if it has grown too large (unbounded growth
		// protection for long-running servers). Safe because the cache is
		// only an optimization — cleared entries trigger a one-time Stat.
		if len(l.fileSizeCache) >= l.maxFileSizeCacheEntries {
			l.fileSizeCache = make(map[string]int64)
		}
		if info, err := os.Stat(path); err == nil {
			cachedSize = info.Size()
		}
		l.fileSizeCache[path] = cachedSize
	}
	if cachedSize > maxLogSize {
		l.rotate(path)
		l.fileSizeCache[path] = 0
	}

	// Redact sensitive info before writing (ADDED 2026-08-28)
	ev.Detail = RedactMap(ev.Detail)

	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	data = append(data, '\n')

	l.Mu.Lock()
	defer l.Mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	n, _ := f.Write(data)
	// Update in-memory file size cache for rotation check.
	l.fileSizeCache[path] += int64(n)
}

func (l *Logger) rotate(path string) {
	l.Mu.Lock()
	defer l.Mu.Unlock()

	// Reset file size cache for the rotated path.
	l.fileSizeCache[path] = 0

	// 1. Try atomic rename with multiple attempts for Windows compatibility
	tmpPath := path + "." + time.Now().Format("150405.000") + ".tmp"
	var renamed bool
	for i := 0; i < 5; i++ {
		if err := os.Rename(path, tmpPath); err == nil {
			renamed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !renamed {
		fmt.Fprintf(os.Stderr, "Logger: Failed to rotate log %s (file may be locked)\n", path)
		return
	}

	// 2. async compression to avoid blocking writer goroutine
	// audit M6: track with l.wg so Close() waits for compression to finish
	l.wg.Add(1)
	go func(src, dest string) {
		defer l.wg.Done()
		// use timestamp to prevent overwrite
		timestamp := time.Now().Format("20060102-150405")
		gzPath := fmt.Sprintf("%s.%s.gz", dest, timestamp)

		if err := l.compressFile(src, gzPath); err == nil {
			_ = os.Remove(src)
		} else {
			fmt.Fprintf(os.Stderr, "Logger: Failed to compress rotated log %s: %v\n", src, err)
		}
	}(tmpPath, path)
}

func (l *Logger) compressFile(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gzFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer gzFile.Close()

	zw := gzip.NewWriter(gzFile)
	defer zw.Close()

	if _, err := io.Copy(zw, f); err != nil {
		return err
	}
	return nil
}

// ReadTaskLogs returns all log lines for a given task (today's log).
// Supports optional pagination: ReadTaskLogs(taskID, [limit, [offset]])
func (l *Logger) ReadTaskLogs(taskID string, args ...int) ([]map[string]any, error) {
	limit := 0
	offset := 0
	if len(args) > 0 {
		limit = args[0]
	}
	if len(args) > 1 {
		offset = args[1]
	}

	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(l.logDir, date, fmt.Sprintf("%s.jsonl", taskID))

	data, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}, nil
	}

	var events []map[string]any
	lines := splitLines(data)

	// Apply offset
	if offset > 0 {
		if offset >= len(lines) {
			return []map[string]any{}, nil
		}
		lines = lines[offset:]
	}

	// Apply limit
	if limit > 0 && len(lines) > limit {
		lines = lines[:limit]
	}

	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events, nil
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// PurgeLogs removes log directories older than daysToKeep.
func (l *Logger) PurgeLogs(daysToKeep int) {
	entries, err := os.ReadDir(l.logDir)
	if err != nil {
		return
	}

	now := time.Now()
	threshold := now.AddDate(0, 0, -daysToKeep)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Parse folder name (expecting YYYY-MM-DD)
		t, err := time.Parse("2006-01-02", entry.Name())
		if err != nil {
			continue
		}

		if t.Before(threshold) {
			dirPath := filepath.Join(l.logDir, entry.Name())
			_ = os.RemoveAll(dirPath)
		}
	}
}
