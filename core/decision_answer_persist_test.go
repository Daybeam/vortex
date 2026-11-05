package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression tests for O25 (docs/audit/unwired-code-register.md §O25).
//
// The choice switch in SubmitDecisionWithPayload applies skip|abort|retry|... to
// the in-memory graph and never called persistGraph — the only writer of
// outputs/<task>/manifest.json and of the durable task-state row. An answer was
// therefore durable only if some other path in the same process happened to
// persist afterwards. CancelTask did persist, which is why cancelling a task
// settled its row while aborting it left the row saying `running` even though
// submit_decision had returned {"ok":true} (measured: task_f31f1868, 14 such
// rows in orchestrator_runtime.db). Inside a live engine the run loop's next
// checkpoint hides the defect; what it bites is the short-lived process pattern
// — rehydrate a task, answer the decision, exit.
//
// These tests pin the answer itself to disk and to the registry, so a future
// refactor that moves the switch cannot silently make answers transient again.

// recordingTaskRegistry captures every state write so a test can assert what a
// later process would read back. The embedded nil interface makes any un-implemented
// method call panic loudly rather than silently return zero values.
type recordingTaskRegistry struct {
	store.TaskRegistry
	mu     sync.Mutex
	states []string
}

func (r *recordingTaskRegistry) SaveTask(_ context.Context, _, status string, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, status)
	return nil
}

func (r *recordingTaskRegistry) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return "<no write>"
	}
	return r.states[len(r.states)-1]
}

func newAnswerTestEngine(t *testing.T) (*DirectedEngine, string) {
	t.Helper()
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	t.Cleanup(lifecycleCancel)

	outputBase := t.TempDir()
	reg := &recordingTaskRegistry{}
	s := &DirectedEngine{
		logger:          mustNewLogger(t, t.TempDir(), nil),
		outputBase:      outputBase,
		graphs:          make(map[string]*schemas.TaskGraph),
		doneChans:       make(map[string]chan struct{}),
		bgWg:            sync.WaitGroup{},
		sieve:           sieve.NewSieve(1),
		spawner:         &Spawner{},
		notifyChan:      make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		TaskRegistry:    reg,
		registry:        &config.Registry{System: config.SystemSettings{}},
	}
	return s, outputBase
}

// newBlockedGraph is the state the engine leaves behind when a step fails and a
// decision is parked: graph running, one blocked step, one pending decision.
func newBlockedGraph(taskID string) *schemas.TaskGraph {
	return &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "appworld_agent", Task: "do the thing", Status: schemas.StepBlocked},
		},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec_o25",
			StepID:  "s1",
			Type:    schemas.DecisionStepFailed,
			Options: []string{"skip", "abort"},
		}},
	}
}

// readManifest unmarshals the durable manifest the way a rehydrating process would.
func readManifest(t *testing.T, outputBase, taskID string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(outputBase, taskID, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest.json not readable (that is O25 itself): %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest.json unparsable: %v", err)
	}
	return m
}

func TestSubmitDecision_AbortIsDurable(t *testing.T) {
	s, outputBase := newAnswerTestEngine(t)
	taskID := "o25_abort_durable"
	s.graphs[taskID] = newBlockedGraph(taskID)
	s.doneChans[taskID] = make(chan struct{})

	// The checkpoint that produced the stale `running` state in the field.
	s.persistGraph(s.graphs[taskID])
	// persistGraph debounces 200 ms per task (H-5); the answer normally arrives
	// seconds later, so give the debounce its real-time window instead of
	// clearing it behind the code's back.
	time.Sleep(210 * time.Millisecond)

	if err := s.SubmitDecisionWithPayload(taskID, "dec_o25", "abort", ""); err != nil {
		t.Fatalf("SubmitDecisionWithPayload(abort): %v", err)
	}

	m := readManifest(t, outputBase, taskID)
	if m["status"] != string(schemas.GraphFailed) {
		t.Fatalf("manifest status = %v, want %q — the answer was applied in memory "+
			"and lost when the process exited (O25)", m["status"], schemas.GraphFailed)
	}
	if got := s.TaskRegistry.(*recordingTaskRegistry).last(); got != string(schemas.GraphFailed) {
		t.Fatalf("task-state row last written as %q, want %q", got, schemas.GraphFailed)
	}
}

func TestSubmitDecision_AnswerRemovesThePendingDecisionDurably(t *testing.T) {
	s, outputBase := newAnswerTestEngine(t)
	taskID := "o25_skip_checkpointed"
	s.graphs[taskID] = newBlockedGraph(taskID)
	s.doneChans[taskID] = make(chan struct{})

	s.persistGraph(s.graphs[taskID])
	time.Sleep(210 * time.Millisecond)

	if err := s.SubmitDecisionWithPayload(taskID, "dec_o25", "skip", ""); err != nil {
		t.Fatalf("SubmitDecisionWithPayload(skip): %v", err)
	}

	// A non-terminal answer still has to be checkpointed: a process that re-reads
	// this task must not re-offer a decision that has already been answered.
	m := readManifest(t, outputBase, taskID)
	pending, ok := m["pending_decisions"].([]any)
	if !ok {
		t.Fatalf("manifest has no pending_decisions array: %#v", m["pending_decisions"])
	}
	if len(pending) != 0 {
		t.Fatalf("durable pending_decisions = %v, want empty — the answer was not checkpointed (O25)", pending)
	}
	steps, _ := m["steps"].(map[string]any)
	s1, _ := steps["s1"].(map[string]any)
	if s1["status"] != string(schemas.StepSkipped) {
		t.Fatalf("durable step status = %v, want %q", s1["status"], schemas.StepSkipped)
	}
}
