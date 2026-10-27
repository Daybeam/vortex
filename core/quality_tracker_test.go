package core

import (
	"testing"
	"time"
)

// TestQualityTracker_RecordsAndTrends verifies that the QualityTracker
// correctly records step outcomes and computes success-rate trends.
// (design §2.4 方案 D, 2026-10-23: observability gap D/E)
func TestQualityTracker_RecordsAndTrends(t *testing.T) {
	q := NewQualityTracker()
	defer q.Wire()()

	// Record 3 successes and 1 failure for role "worker".
	q.Record("worker", QualitySample{Timestamp: time.Now(), TaskID: "t1", Outcome: "ok", Confidence: 0.9})
	q.Record("worker", QualitySample{Timestamp: time.Now(), TaskID: "t2", Outcome: "ok", Confidence: 0.8})
	q.Record("worker", QualitySample{Timestamp: time.Now(), TaskID: "t3", Outcome: "fail"})
	q.Record("worker", QualitySample{Timestamp: time.Now(), TaskID: "t4", Outcome: "ok", Confidence: 0.7})

	trend := q.GetTrend("worker", 1*time.Hour)
	if trend.Total != 4 {
		t.Fatalf("expected 4 total, got %d", trend.Total)
	}
	if trend.Successes != 3 {
		t.Fatalf("expected 3 successes, got %d", trend.Successes)
	}
	if trend.SuccessRate != 0.75 {
		t.Fatalf("expected 0.75 success rate, got %f", trend.SuccessRate)
	}
	// avg conf = (0.9 + 0.8 + 0.0 + 0.7) / 4 = 0.6
	if trend.AvgConf < 0.59 || trend.AvgConf > 0.61 {
		t.Fatalf("expected ~0.6 avg confidence, got %f", trend.AvgConf)
	}
}

// TestQualityTracker_SlidingWindowCap verifies the window evicts old samples
// when exceeding the cap (200), preventing unbounded growth.
func TestQualityTracker_SlidingWindowCap(t *testing.T) {
	q := NewQualityTracker()
	defer q.Wire()()

	// Record 250 samples — only the last 200 should be retained.
	for i := 0; i < 250; i++ {
		q.Record("cap-test", QualitySample{Timestamp: time.Now(), TaskID: "t", Outcome: "ok"})
	}
	trend := q.GetTrend("cap-test", 1*time.Hour)
	if trend.Total != qualityWindowCap {
		t.Fatalf("expected %d (capped), got %d", qualityWindowCap, trend.Total)
	}
}

// TestQualityTracker_EventBusWiring verifies that publishing step events
// on the EventBus populates the tracker via the Wire() subscription.
func TestQualityTracker_EventBusWiring(t *testing.T) {
	q := NewQualityTracker()
	defer q.Wire()()

	// Publish a step-completed event with role_id + confidence.
	DefaultBus.Publish(NewAgentEvent("task-qt", "s1", string(EventStepCompleted), map[string]any{
		"confidence": 0.85,
		"role_id":    "analyst",
	}))
	DefaultBus.Publish(NewAgentEvent("task-qt", "s2", string(EventStepFailed), map[string]any{
		"role_id": "analyst",
	}))

	trend := q.GetTrend("analyst", 1*time.Hour)
	if trend.Total != 2 {
		t.Fatalf("expected 2 samples from EventBus, got %d", trend.Total)
	}
	if trend.Successes != 1 {
		t.Fatalf("expected 1 success, got %d", trend.Successes)
	}
}

// TestQualityTracker_NilSafe verifies nil receiver doesn't panic.
func TestQualityTracker_NilSafe(t *testing.T) {
	var q *QualityTracker
	q.Record("x", QualitySample{})        // must not panic
	_ = q.GetTrend("x", time.Hour)        // must not panic
	q.Unwire()                             // must not panic
}
