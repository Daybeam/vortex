package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Regression tests for Q4-6 (register O20, docs/plan/O_BATCH_REMEDIATION_2026-11-04.md).
//
// O20 measured the silence this closes: a delegation envelope was handed to a
// host, the host's second submission never came back within 12 minutes, and
// get_task_status answered only "running" the whole time. There is no deadline
// on the sync-envelope wait and no progress signal in the status, so "slow
// delegation" and "my answer was lost" were the same observable.
//
// The operator's answer: 「把"分钟级无回传"显式化为错误，超时值做成可配置，默认给宽」.
// These tests pin the three properties that answer depends on — that the report
// appears past the window, that the window is configurable, and that the report
// is a report: O22 forbids a timer from resolving a delegated step, so nothing
// here may abort, cancel, or drop the parked decision.

func delegationTimeoutPtr(n int) *int { return &n }

// newDelegationGraph is the O20 state: one step parked behind a delegation
// decision created waitingFor ago.
func newDelegationGraph(taskID string, waitingFor time.Duration) *schemas.TaskGraph {
	return &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "appworld_agent", Task: "do the thing", Status: schemas.StepBlocked},
		},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec_deleg",
			StepID:  "s1",
			Type:    schemas.DecisionDelegationRequired,
			Context: map[string]any{"envelope": "compiled"},
			Created: time.Now().Add(-waitingFor),
		}},
	}
}

func waitReport(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	raw, ok := status["delegation_wait"]
	if !ok {
		return nil
	}
	report, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("delegation_wait is %T, want map", raw)
	}
	return report
}

// TestDelegationAnswerTimeout_ReportsPastDefaultWindow is the O20 shape: a
// delegation parked 31 minutes at the wide default must be named in the status
// instead of leaving "running" as the only signal.
func TestDelegationAnswerTimeout_ReportsPastDefaultWindow(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q46_default_window"
	graph := newDelegationGraph(taskID, 31*time.Minute)
	s.graphs[taskID] = graph

	status, ok := s.GetStatus(taskID)
	if !ok {
		t.Fatal("GetStatus said not found")
	}
	report := waitReport(t, status)
	if report == nil {
		t.Fatalf("31-minute unanswered delegation reported no wait: %+v", status)
	}
	if report["stale"] != true {
		t.Errorf("stale = %v, want true", report["stale"])
	}
	if got := report["timeout_seconds"]; got != 1800 {
		t.Errorf("timeout_seconds = %v, want 1800 (the wide default)", got)
	}
	errText, _ := report["error"].(string)
	for _, want := range []string{"dec_deleg", "s1", "submit_decision"} {
		if !strings.Contains(errText, want) {
			t.Errorf("error %q does not name %q", errText, want)
		}
	}
	if !strings.Contains(errText, "not an abort") {
		t.Errorf("error %q must say it is not an abort (O22)", errText)
	}
	if status["next_action"] != "submit_decision" {
		t.Errorf("next_action = %v, want submit_decision", status["next_action"])
	}

	// The no-auto-abort pin: reading status must not resolve anything.
	if graph.Status != schemas.GraphRunning {
		t.Errorf("graph status = %s, a report may not change it", graph.Status)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("pending decisions = %d, the report forfeited the answer", len(graph.PendingDecisions))
	}
	if graph.Steps["s1"].Status != schemas.StepBlocked {
		t.Errorf("step s1 = %s, still must be parked", graph.Steps["s1"].Status)
	}

	// Idempotent: a second poll reports the same thing and changes nothing.
	if again := waitReport(t, mustStatus(t, s, taskID)); again == nil {
		t.Error("second poll lost the report")
	}
}

func mustStatus(t *testing.T, s *DirectedEngine, taskID string) map[string]any {
	t.Helper()
	status, ok := s.GetStatus(taskID)
	if !ok {
		t.Fatalf("GetStatus(%s) not found", taskID)
	}
	return status
}

// TestDelegationAnswerTimeout_SilentInsideWindow pins the other half of "wide":
// a delegation that is merely minutes old — the legitimate case, where the host
// is running a whole sub-agent — must not be called an error.
func TestDelegationAnswerTimeout_SilentInsideWindow(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q46_inside_window"
	s.graphs[taskID] = newDelegationGraph(taskID, time.Minute)

	if report := waitReport(t, mustStatus(t, s, taskID)); report != nil {
		t.Errorf("1-minute delegation reported as stale: %+v", report)
	}
}

// TestDelegationAnswerTimeout_Configurable covers the "超时值做成可配置" half:
// system.delegation_answer_timeout_seconds sets the window, 0 or less turns the
// report off, and an explicit value does not inherit the default.
func TestDelegationAnswerTimeout_Configurable(t *testing.T) {
	cases := []struct {
		name      string
		setting   *int
		waiting   time.Duration
		wantOK    bool
		wantLimit int
	}{
		{"override_60s_reports", delegationTimeoutPtr(60), 2 * time.Minute, true, 60},
		{"override_60s_silent_below", delegationTimeoutPtr(60), 10 * time.Second, false, 0},
		{"zero_disables", delegationTimeoutPtr(0), 10 * time.Hour, false, 0},
		{"negative_disables", delegationTimeoutPtr(-1), 10 * time.Hour, false, 0},
		{"nil_uses_wide_default", nil, 31 * time.Minute, true, 1800},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerTestEngine(t)
			s.registry.System.DelegationAnswerTimeoutSeconds = tc.setting
			taskID := "q46_" + tc.name
			s.graphs[taskID] = newDelegationGraph(taskID, tc.waiting)

			report := waitReport(t, mustStatus(t, s, taskID))
			if !tc.wantOK {
				if report != nil {
					t.Fatalf("want no report, got %+v", report)
				}
				return
			}
			if report == nil {
				t.Fatalf("want a report, got none")
			}
			if got := report["timeout_seconds"]; got != tc.wantLimit {
				t.Errorf("timeout_seconds = %v, want %d", got, tc.wantLimit)
			}
		})
	}
}

// TestDelegationAnswerTimeout_OnlyDelegationsAreReported: Q4-6 is about the
// delegation handover. A step_failed decision parked for days is a different
// question (Q5-5 is unanswered and has no number yet), so it must not be
// preempted by this report.
func TestDelegationAnswerTimeout_OnlyDelegationsAreReported(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q46_other_types"
	graph := newDelegationGraph(taskID, 3*24*time.Hour)
	graph.PendingDecisions = []*schemas.Decision{
		{ID: "dec_step_failed", StepID: "s1", Type: schemas.DecisionStepFailed, Created: time.Now().Add(-3 * 24 * time.Hour)},
		{ID: "dec_approval", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired, Created: time.Now().Add(-3 * 24 * time.Hour)},
	}
	s.graphs[taskID] = graph

	if report := waitReport(t, mustStatus(t, s, taskID)); report != nil {
		t.Errorf("non-delegation decisions reported as a delegation wait: %+v", report)
	}
}

// TestDelegationAnswerTimeout_SkipsZeroCreated: a graph rehydrated from a
// manifest written before Decision.Created existed has no timestamp to be late
// against. Treating zero as "infinitely old" would flag every legacy task.
func TestDelegationAnswerTimeout_SkipsZeroCreated(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q46_zero_created"
	graph := newDelegationGraph(taskID, time.Minute)
	graph.PendingDecisions[0].Created = time.Time{}
	s.graphs[taskID] = graph

	if report := waitReport(t, mustStatus(t, s, taskID)); report != nil {
		t.Errorf("zero Created reported as stale: %+v", report)
	}
}

// TestDelegationAnswerTimeout_NamesTheOldestOfMany: with several parked
// delegations the host needs to know which one to chase first.
func TestDelegationAnswerTimeout_NamesTheOldestOfMany(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q46_many"
	graph := newDelegationGraph(taskID, 45*time.Minute)
	graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
		ID: "dec_deleg_newer", StepID: "s2", Type: schemas.DecisionDelegationRequired,
		Created: time.Now().Add(-35 * time.Minute),
	})
	s.graphs[taskID] = graph

	report := waitReport(t, mustStatus(t, s, taskID))
	if report == nil {
		t.Fatal("two parked delegations reported nothing")
	}
	stale, ok := report["decisions"].([]StaleDecision)
	if !ok || len(stale) != 2 {
		t.Fatalf("decisions = %#v, want 2 entries", report["decisions"])
	}
	errText, _ := report["error"].(string)
	if !strings.Contains(errText, "2 parked delegations") || !strings.Contains(errText, `oldest "dec_deleg"`) {
		t.Errorf("error should lead with the count and the oldest id, got %q", errText)
	}
}

// TestDelegationAnswerTimeout_WaitTaskCarriesIt: wait_task is where a host
// parks itself, which is exactly the O20 position. The report has to be on the
// path a blocked host actually reads, not only on get_task_status.
func TestDelegationAnswerTimeout_WaitTaskCarriesIt(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q46_wait_task"
	graph := newDelegationGraph(taskID, 31*time.Minute)
	s.graphs[taskID] = graph
	s.doneChans[taskID] = make(chan struct{})

	// The fast path (pending decisions > 0) returns without waiting.
	status, ok := s.WaitTask(context.Background(), taskID, time.Millisecond)
	if !ok {
		t.Fatal("WaitTask said not found")
	}
	if report := waitReport(t, status); report == nil {
		t.Errorf("wait_task status carried no delegation wait: %+v", status)
	} else if !strings.Contains(fmt.Sprint(report["error"]), "not an abort") {
		t.Errorf("wait_task error text %v must read as a report", report["error"])
	}
}
