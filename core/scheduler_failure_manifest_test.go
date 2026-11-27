package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// F7 regression: ENGINE_EVAL_VALIDATION_PLAN-2026-11-27.md — answering the
// failure decision used to remove the only failure_class/root_cause record.
func TestF7_FailureClassificationSurvivesFinalManifest(t *testing.T) {
	s := newFailEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-f7-manifest",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning, RoleID: "r1", Task: "exchange order"}
	graph.Steps["s1"] = step
	injectTestGraph(t, s, graph)
	for i := 0; i < toolFailCircuitBreakerThreshold; i++ {
		incrTaskToolFailForTest(t, s.spawner, graph.TaskID, "exchange_order", 1)
	}

	s.handleStepFailure(graph, step, &SpawnResult{Output: schemas.SubagentOutput{
		Status: schemas.StatusPartial, Confidence: 0.3,
	}})
	want := string(FailureClassDeterministicToolRefusal)
	if graph.FailureClass != want || graph.RootCause != want {
		t.Fatalf("graph failure=(%q,%q), want (%q,%q)", graph.FailureClass, graph.RootCause, want, want)
	}
	status := graph.ToStatusDict()
	if status["failure_class"] != want || status["root_cause"] != want {
		t.Fatalf("status failure=(%v,%v), want %q", status["failure_class"], status["root_cause"], want)
	}

	s.Mu.Lock()
	graph.PendingDecisions = nil
	s.Mu.Unlock()
	s.finalize(graph)

	data, err := os.ReadFile(filepath.Join(s.outputBase, graph.TaskID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["failure_class"] != want || manifest["root_cause"] != want {
		t.Fatalf("manifest lost failure classification: %s", data)
	}
}

func TestF7_SmartRouteTerminalEventRetainsFailure(t *testing.T) {
	s := newFailEngine(t)
	defer s.Stop()
	events := make(chan obs.AgentEvent, 1)
	unsub := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskFailed), func(e obs.AgentEvent) error {
		select {
		case events <- e:
		default:
		}
		return nil
	})
	defer unsub()

	graph := &schemas.TaskGraph{
		TaskID: "task-f7-smart-event", Steps: make(map[string]*schemas.Step),
		Status: schemas.GraphRunning, IsSmartRouted: true,
		FailureClass: "generative_uncertainty", RootCause: "generative_uncertainty",
	}
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepFailed}
	_, cancel := injectRunningTestGraph(t, s, graph)
	defer cancel()
	s.Mu.Lock()
	unlocked := false
	finalized := s.finalizeSmartRoutedAfterDecision(graph, graph.TaskID, "s1", &unlocked)
	if !finalized || !unlocked {
		t.Fatal("smart-routed graph was not finalized")
	}
	select {
	case event := <-events:
		if event.Payload["failure_class"] != graph.FailureClass || event.Payload["root_cause"] != graph.RootCause {
			t.Fatalf("terminal event lost failure classification: %+v", event.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal failure event not published")
	}
}

func TestF7_ApprovalDecisionDoesNotBecomeFailure(t *testing.T) {
	s := newFailEngine(t)
	defer s.Stop()
	graph := &schemas.TaskGraph{TaskID: "task-f7-approval", Steps: make(map[string]*schemas.Step), Status: schemas.GraphRunning}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step
	injectTestGraph(t, s, graph)

	s.emitMountApprovalDecision(graph, step, []string{"browser"}, nil)
	if graph.FailureClass != "" || graph.RootCause != "" {
		t.Fatalf("approval polluted terminal failure fields: (%q,%q)", graph.FailureClass, graph.RootCause)
	}
}
