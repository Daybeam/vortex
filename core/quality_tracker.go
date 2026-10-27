package core

// quality_tracker.go
//
// Sliding-window quality trend tracking per role/task-type.
// Addresses observability gap D/E (design §2.4 方案 D, 2026-10-23):
// the existing monitors (CacheSentinel, BudgetSentinel, ConvergenceMonitor,
// ProviderTelemetry, Metrics) only capture instantaneous snapshots — none
// record a time series of success rates. This tracker fills that gap by
// subscribing to step/task completion events and maintaining a bounded
// sliding window (cap=200) of recent outcomes per role.
//
// Design constraints:
//   - Stdlib only (sync, time) — no new dependencies.
//   - Sliding window, not unbounded accumulation (audit F2 lesson).
//   - Nil-safe — disabled gracefully when nil.
//   - Follows the BudgetGate Wire/Unwire pattern (EventBus subscription).

import (
	"sync"
	"time"
)

const qualityWindowCap = 200

// QualityTracker records step/task outcomes in per-role sliding windows
// so callers can answer "is quality improving or declining over time?"
type QualityTracker struct {
	mu           sync.RWMutex
	windows      map[string]*QualityWindow // key = roleID (or "global")
	unsubscribes []func()
}

// QualityWindow is a bounded ring of recent quality samples.
type QualityWindow struct {
	samples []QualitySample
}

// QualitySample is one observed outcome at a point in time.
type QualitySample struct {
	Timestamp  time.Time
	TaskID     string
	Outcome    string  // "ok" | "partial" | "fail"
	Confidence float64 // 0.0-1.0 from the step/task
	AuditLevel int     // 1=L1 pass, 2=L2 pass, 3=L3 pass, 0=fail/no-audit
}

// NewQualityTracker creates a QualityTracker.
func NewQualityTracker() *QualityTracker {
	return &QualityTracker{
		windows: make(map[string]*QualityWindow),
	}
}

// Wire subscribes to step/task completion+failure events on the global EventBus.
// Call once at startup; defer the returned func to tear down.
func (q *QualityTracker) Wire() func() {
	if q == nil {
		return func() {}
	}
	q.unsubscribes = append(q.unsubscribes,
		DefaultBus.SubscribeWithCancel(string(EventStepCompleted), q.onStepCompleted),
		DefaultBus.SubscribeWithCancel(string(EventStepFailed), q.onStepFailed),
		DefaultBus.SubscribeWithCancel(string(EventTaskCompleted), q.onTaskCompleted),
		DefaultBus.SubscribeWithCancel(string(EventTaskFailed), q.onTaskFailed),
	)
	return q.Unwire
}

// Unwire tears down all EventBus subscriptions. Safe to call multiple times.
func (q *QualityTracker) Unwire() {
	if q == nil || len(q.unsubscribes) == 0 {
		return
	}
	for _, unsub := range q.unsubscribes {
		unsub()
	}
	q.unsubscribes = nil
}

// Record adds a quality sample to the window for the given key (roleID).
func (q *QualityTracker) Record(key string, sample QualitySample) {
	if q == nil || key == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	w, ok := q.windows[key]
	if !ok {
		w = &QualityWindow{}
		q.windows[key] = w
	}
	if len(w.samples) >= qualityWindowCap {
		w.samples = w.samples[1:] // slide: drop oldest
	}
	w.samples = append(w.samples, sample)
}

// QualityTrend is the aggregated result for a time-bounded query.
type QualityTrend struct {
	Key         string  `json:"key"`
	Total       int     `json:"total"`
	Successes   int     `json:"successes"`
	SuccessRate float64 `json:"success_rate"`
	AvgConf     float64 `json:"avg_confidence"`
	Since       string  `json:"since"`
}

// GetTrend returns the success rate for the given key (roleID) over the
// last `window` duration. Returns zero-value if no samples.
func (q *QualityTracker) GetTrend(key string, window time.Duration) QualityTrend {
	if q == nil || key == "" {
		return QualityTrend{Key: key}
	}
	cutoff := time.Now().Add(-window)
	q.mu.RLock()
	defer q.mu.RUnlock()
	w, ok := q.windows[key]
	if !ok {
		return QualityTrend{Key: key, Since: cutoff.Format(time.RFC3339)}
	}
	var total, successes int
	var confSum float64
	for _, s := range w.samples {
		if s.Timestamp.Before(cutoff) {
			continue
		}
		total++
		confSum += s.Confidence
		if s.Outcome == "ok" {
			successes++
		}
	}
	rate := 0.0
	if total > 0 {
		rate = float64(successes) / float64(total)
	}
	avgConf := 0.0
	if total > 0 {
		avgConf = confSum / float64(total)
	}
	return QualityTrend{
		Key: key, Total: total, Successes: successes,
		SuccessRate: rate, AvgConf: avgConf,
		Since: cutoff.Format(time.RFC3339),
	}
}

// onStepCompleted records an "ok" sample from EventStepCompleted payload.
func (q *QualityTracker) onStepCompleted(ev AgentEvent) error {
	if q == nil || ev.TaskID == "" {
		return nil
	}
	conf, _ := ev.Payload["confidence"].(float64)
	roleID, _ := ev.Payload["role_id"].(string)
	if roleID == "" {
		roleID = "global"
	}
	q.Record(roleID, QualitySample{
		Timestamp:  time.Now(),
		TaskID:     ev.TaskID,
		Outcome:    "ok",
		Confidence: conf,
	})
	return nil
}

// onStepFailed records a "fail" sample.
func (q *QualityTracker) onStepFailed(ev AgentEvent) error {
	if q == nil || ev.TaskID == "" {
		return nil
	}
	roleID, _ := ev.Payload["role_id"].(string)
	if roleID == "" {
		roleID = "global"
	}
	q.Record(roleID, QualitySample{
		Timestamp: time.Now(),
		TaskID:    ev.TaskID,
		Outcome:   "fail",
	})
	return nil
}

// onTaskCompleted records a task-level "ok" sample under "task" key.
func (q *QualityTracker) onTaskCompleted(ev AgentEvent) error {
	if q == nil || ev.TaskID == "" {
		return nil
	}
	conf, _ := ev.Payload["confidence"].(float64)
	q.Record("task", QualitySample{
		Timestamp:  time.Now(),
		TaskID:     ev.TaskID,
		Outcome:    "ok",
		Confidence: conf,
	})
	return nil
}

// onTaskFailed records a task-level "fail" sample under "task" key.
func (q *QualityTracker) onTaskFailed(ev AgentEvent) error {
	if q == nil || ev.TaskID == "" {
		return nil
	}
	q.Record("task", QualitySample{
		Timestamp: time.Now(),
		TaskID:    ev.TaskID,
		Outcome:   "fail",
	})
	return nil
}
