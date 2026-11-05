package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Regression tests for Q5-5 (answered 2026-11-06): 「step_failed 等人裁决的 24 小时
// 判"已陈旧"，… 到期只新增一个信号，在 /tasks 和 get_task_status 里显示，不自动
// abort」.
//
// The delegation half (30 minutes) is pinned by delegation_answer_timeout_test.go.
// These tests cover the second window: a failed step whose decision nobody has
// answered for a day. The same O22 constraint applies — a report may never
// resolve, cancel, or drop anything.

func staleTimeoutPtr(n int) *int { return &n }

// newFailedDecisionGraph is the state a failed step leaves behind: graph still
// running, step blocked, one step_failed decision parked waitingFor ago.
func newFailedDecisionGraph(taskID string, waitingFor time.Duration) *schemas.TaskGraph {
	return &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "worker", Task: "do the thing", Status: schemas.StepBlocked, LastError: "boom"},
		},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec_failed",
			StepID:  "s1",
			Type:    schemas.DecisionStepFailed,
			Context: map[string]any{"error": "boom"},
			Created: time.Now().Add(-waitingFor),
		}},
	}
}

func staleReport(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	raw, ok := status["stale_decisions"]
	if !ok {
		return nil
	}
	report, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("stale_decisions is %T, want map", raw)
	}
	return report
}

// TestStaleDecisionReport_ReportsPastDefaultWindow is the operator's shape: a
// step_failed decision parked 25 hours must be named instead of leaving the
// task looking merely "running" in get_task_status.
func TestStaleDecisionReport_ReportsPastDefaultWindow(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_default_window"
	graph := newFailedDecisionGraph(taskID, 25*time.Hour)
	s.graphs[taskID] = graph

	report := staleReport(t, mustStatus(t, s, taskID))
	if report == nil {
		t.Fatal("25-hour unanswered step_failed decision reported nothing")
	}
	if report["stale"] != true {
		t.Errorf("stale = %v, want true", report["stale"])
	}
	if got := report["timeout_seconds"]; got != 86400 {
		t.Errorf("timeout_seconds = %v, want 86400 (24h default)", got)
	}
	if got, _ := report["oldest_wait_seconds"].(int); got < 25*3600 {
		t.Errorf("oldest_wait_seconds = %v, want at least 90000", report["oldest_wait_seconds"])
	}
	errText, _ := report["error"].(string)
	for _, want := range []string{"dec_failed", "s1", "not an abort", "decision_stale_timeout_seconds", "submit_decision"} {
		if !strings.Contains(errText, want) {
			t.Errorf("error %q does not contain %q", errText, want)
		}
	}
	if status := mustStatus(t, s, taskID); status["next_action"] != "submit_decision" {
		t.Errorf("next_action = %v, want submit_decision", status["next_action"])
	}

	// The no-auto-abort pin: reading status must not resolve the failure.
	if graph.Status != schemas.GraphRunning {
		t.Errorf("graph status = %s, a report may not change it", graph.Status)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("pending decisions = %d, the report forfeited the answer", len(graph.PendingDecisions))
	}
	if graph.Steps["s1"].Status != schemas.StepBlocked {
		t.Errorf("step s1 = %s, still must be parked", graph.Steps["s1"].Status)
	}
}

// TestStaleDecisionReport_SilentInsideWindow: one day minus one hour is still a
// legitimate "left it for the day" window, not a stale decision.
func TestStaleDecisionReport_SilentInsideWindow(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_inside_window"
	s.graphs[taskID] = newFailedDecisionGraph(taskID, 23*time.Hour)

	if report := staleReport(t, mustStatus(t, s, taskID)); report != nil {
		t.Errorf("23-hour step_failed reported as stale: %+v", report)
	}
}

// TestStaleDecisionReport_Configurable pins "两个值都做成配置项": the override
// sets the window, 0/negative turns it off, and nil keeps 24h.
func TestStaleDecisionReport_Configurable(t *testing.T) {
	cases := []struct {
		name      string
		setting   *int
		waiting   time.Duration
		wantOK    bool
		wantLimit int
	}{
		{"override_60s_reports", staleTimeoutPtr(60), 2 * time.Minute, true, 60},
		{"override_60s_silent_below", staleTimeoutPtr(60), 10 * time.Second, false, 0},
		{"zero_disables", staleTimeoutPtr(0), 30 * 24 * time.Hour, false, 0},
		{"negative_disables", staleTimeoutPtr(-1), 30 * 24 * time.Hour, false, 0},
		{"nil_uses_24h_default", nil, 25 * time.Hour, true, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerTestEngine(t)
			s.registry.System.DecisionStaleTimeoutSeconds = tc.setting
			taskID := "q55_" + tc.name
			s.graphs[taskID] = newFailedDecisionGraph(taskID, tc.waiting)

			report := staleReport(t, mustStatus(t, s, taskID))
			if !tc.wantOK {
				if report != nil {
					t.Fatalf("want no report, got %+v", report)
				}
				return
			}
			if report == nil {
				t.Fatal("want a report, got none")
			}
			if got := report["timeout_seconds"]; got != tc.wantLimit {
				t.Errorf("timeout_seconds = %v, want %d", got, tc.wantLimit)
			}
		})
	}
}

// TestStaleDecisionReport_OnlyStepFailuresAreReported: delegation decisions have
// their own 30-minute report; reporting them under the 24-hour window too would
// make the two numbers indistinguishable at the status surface.
func TestStaleDecisionReport_OnlyStepFailuresAreReported(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_other_types"
	graph := newFailedDecisionGraph(taskID, time.Minute)
	graph.PendingDecisions = []*schemas.Decision{
		{ID: "dec_deleg", StepID: "s1", Type: schemas.DecisionDelegationRequired, Created: time.Now().Add(-3 * 24 * time.Hour)},
		{ID: "dec_cap", StepID: "s1", Type: schemas.DecisionCapabilityRequired, Created: time.Now().Add(-3 * 24 * time.Hour)},
	}
	s.graphs[taskID] = graph

	status := mustStatus(t, s, taskID)
	if report := staleReport(t, status); report != nil {
		t.Errorf("non-step_failed decisions reported as stale: %+v", report)
	}
	// The delegation half must still fire for its own type.
	if report := waitReport(t, status); report == nil {
		t.Error("the delegation report did not fire on the same graph")
	}
}

// TestStaleDecisionReport_SkipsZeroCreated mirrors the delegation report: a
// decision restored from a manifest written before Created existed has no
// timestamp to be late against.
func TestStaleDecisionReport_SkipsZeroCreated(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_zero_created"
	graph := newFailedDecisionGraph(taskID, time.Minute)
	graph.PendingDecisions[0].Created = time.Time{}
	s.graphs[taskID] = graph

	if report := staleReport(t, mustStatus(t, s, taskID)); report != nil {
		t.Errorf("zero Created reported as stale: %+v", report)
	}
}

// TestStaleDecisionReport_NamesTheOldestOfMany: with several failed steps the
// human needs to know which one has been waiting longest.
func TestStaleDecisionReport_NamesTheOldestOfMany(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_many"
	graph := newFailedDecisionGraph(taskID, 26*time.Hour)
	graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
		ID: "dec_newer", StepID: "s2", Type: schemas.DecisionStepFailed,
		Created: time.Now().Add(-25 * time.Hour),
	})
	s.graphs[taskID] = graph

	report := staleReport(t, mustStatus(t, s, taskID))
	if report == nil {
		t.Fatal("two stale failures reported nothing")
	}
	stale, ok := report["decisions"].([]StaleDecision)
	if !ok || len(stale) != 2 {
		t.Fatalf("decisions = %#v, want 2 entries", report["decisions"])
	}
	errText, _ := report["error"].(string)
	if !strings.Contains(errText, "2 failed steps") || !strings.Contains(errText, `oldest "s1"`) {
		t.Errorf("error should lead with the count and the oldest step, got %q", errText)
	}
}

// TestStaleDecisionReport_BothWindowsAreIndependent: one graph can hold a 31m
// delegation and a 25h failure at once. Both signals attach, each with its own
// configured window, and they do not overwrite each other.
func TestStaleDecisionReport_BothWindowsAreIndependent(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_both"
	graph := newFailedDecisionGraph(taskID, 25*time.Hour)
	graph.PendingDecisions = append(graph.PendingDecisions, &schemas.Decision{
		ID: "dec_deleg", StepID: "s2", Type: schemas.DecisionDelegationRequired,
		Created: time.Now().Add(-31 * time.Minute),
	})
	s.graphs[taskID] = graph

	status := mustStatus(t, s, taskID)
	wait := waitReport(t, status)
	stale := staleReport(t, status)
	if wait == nil || stale == nil {
		t.Fatalf("want both signals, got delegation_wait=%+v stale_decisions=%+v", wait, stale)
	}
	if wait["timeout_seconds"] != 1800 {
		t.Errorf("delegation timeout_seconds = %v, want 1800", wait["timeout_seconds"])
	}
	if stale["timeout_seconds"] != 86400 {
		t.Errorf("stale timeout_seconds = %v, want 86400", stale["timeout_seconds"])
	}
	if status["next_action"] != "submit_decision" {
		t.Errorf("next_action = %v, want submit_decision", status["next_action"])
	}
}

// TestStaleDecisionReport_WaitTaskCarriesIt: wait_task is where a host parks
// itself, so the stale-failure report must be on that path too.
func TestStaleDecisionReport_WaitTaskCarriesIt(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "q55_wait_task"
	s.graphs[taskID] = newFailedDecisionGraph(taskID, 25*time.Hour)
	s.doneChans[taskID] = make(chan struct{})

	status, ok := s.WaitTask(context.Background(), taskID, time.Millisecond)
	if !ok {
		t.Fatal("WaitTask said not found")
	}
	if report := staleReport(t, status); report == nil {
		t.Errorf("wait_task status carried no stale_decisions: %+v", status)
	}
}

// TestListGraphsCarriesStalenessSignals: /tasks renders ListGraphs, not
// GetStatus. Both the in-memory and the disk-restored halves must carry the two
// signals, or the task list stays silent about parked decisions.
func TestListGraphsCarriesStalenessSignals(t *testing.T) {
	s, outputBase := newAnswerTestEngine(t)

	staleID := "q55_list_stale"
	s.graphs[staleID] = newFailedDecisionGraph(staleID, 25*time.Hour)
	s.graphs[staleID].PendingDecisions = append(s.graphs[staleID].PendingDecisions, &schemas.Decision{
		ID: "dec_deleg", StepID: "s2", Type: schemas.DecisionDelegationRequired,
		Created: time.Now().Add(-31 * time.Minute),
	})

	freshID := "q55_list_fresh"
	s.graphs[freshID] = newFailedDecisionGraph(freshID, time.Minute)

	// The disk half: a manifest not in s.graphs, still carrying a stale failure.
	diskID := "q55_list_disk"
	diskGraph := newFailedDecisionGraph(diskID, 26*time.Hour)
	data, err := json.Marshal(diskGraph)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outputBase, diskID), 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputBase, diskID, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	rows := make(map[string]map[string]any, len(s.graphs)+1)
	for _, row := range s.ListGraphs() {
		id, _ := row["task_id"].(string)
		rows[id] = row
	}

	staleRow := rows[staleID]
	if staleRow == nil {
		t.Fatalf("in-memory task missing from ListGraphs: %v", rows)
	}
	if report := staleReport(t, staleRow); report == nil {
		t.Errorf("in-memory ListGraphs row has no stale_decisions: %+v", staleRow)
	}
	if report := waitReport(t, staleRow); report == nil {
		t.Errorf("in-memory ListGraphs row has no delegation_wait: %+v", staleRow)
	}

	if report := staleReport(t, rows[freshID]); report != nil {
		t.Errorf("fresh failure reported stale in ListGraphs: %+v", report)
	}

	diskRow := rows[diskID]
	if diskRow == nil {
		t.Fatalf("disk task missing from ListGraphs: %v", rows)
	}
	if diskRow["status"] != string(schemas.GraphOrphaned) {
		// Pre-existing override; asserted here so the marker is not attributed
		// to a still-running disk task.
		t.Logf("disk row status = %v (expected orphaned)", diskRow["status"])
	}
	if report := staleReport(t, diskRow); report == nil {
		t.Errorf("disk ListGraphs row has no stale_decisions: %+v", diskRow)
	}
}
