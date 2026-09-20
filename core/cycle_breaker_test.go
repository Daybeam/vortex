package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func newTestEngineForCycleBreaker(t *testing.T) *DirectedEngine {
	t.Helper()
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return &DirectedEngine{
		logger:     logger,
		outputBase: t.TempDir(),
		doneChans:  make(map[string]chan struct{}),
	}
}

func TestMakeCycleID(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		deps    []string
		want    string
	}{
		{"single_dep", "s3", []string{"s1"}, "s1:s3"},
		{"multi_dep_sorted", "s3", []string{"s1", "s2"}, "s1:s2:s3"},
		{"multi_dep_unsorted_input", "s1", []string{"s3", "s2"}, "s1:s2:s3"},
		{"no_deps", "s1", []string{}, "s1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := makeCycleID(tt.trigger, tt.deps)
			if got != tt.want {
				t.Fatalf("makeCycleID(%q, %v) = %q, want %q", tt.trigger, tt.deps, got, tt.want)
			}
		})
	}
}

func TestTryBacktrack_NotApplicable(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	tests := []struct {
		name      string
		missing   []string
		deps      []string
		maxRounds int
	}{
		{"no_missing_context", nil, []string{"s1"}, 3},
		{"no_deps", []string{"x"}, nil, 3},
		{"disabled", []string{"x"}, []string{"s1"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &schemas.Step{
				ID:            "s2",
				DependsOn:     tt.deps,
				MaxLoopRounds: tt.maxRounds,
				Status:        schemas.StepPartial,
			}
			graph := &schemas.TaskGraph{
				TaskID: "t1",
				Status: schemas.GraphRunning,
				Steps:  map[string]*schemas.Step{"s2": step},
			}
			output := &schemas.SubagentOutput{MissingContext: tt.missing}

			handled := engine.tryBacktrack(graph, step, output)
			if handled {
				t.Fatal("expected tryBacktrack to return false (not applicable)")
			}
		})
	}
}

func TestTryBacktrack_UnderLimit_ResetsUpstreamAndDownstream(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK, Task: "upstream task"}
	downstream := &schemas.Step{
		ID:            "s3",
		DependsOn:     []string{"s1", "s2"},
		MaxLoopRounds: 3,
		Status:        schemas.StepPartial,
		MissingCtx:    []string{"missing_data"},
		LastError:     "some error",
	}
	upstream2 := &schemas.Step{ID: "s2", Status: schemas.StepOK}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": upstream2, "s3": downstream},
	}
	output := &schemas.SubagentOutput{
		MissingContext: []string{"缺少营收数据"},
	}

	handled := engine.tryBacktrack(graph, downstream, output)
	if !handled {
		t.Fatal("expected tryBacktrack to return true")
	}

	if upstream.Status != schemas.StepPending {
		t.Fatalf("expected upstream s1 Status == StepPending, got %v", upstream.Status)
	}
	if upstream2.Status != schemas.StepPending {
		t.Fatalf("expected upstream s2 Status == StepPending, got %v", upstream2.Status)
	}
	if upstream.AdditionalPromptContext == "" {
		t.Fatal("expected upstream s1 AdditionalPromptContext to contain feedback")
	}
	if downstream.Status != schemas.StepPending {
		t.Fatalf("expected downstream s3 Status == StepPending, got %v", downstream.Status)
	}
	if downstream.MissingCtx != nil {
		t.Fatal("expected downstream MissingCtx to be cleared")
	}
	if downstream.LastError != "" {
		t.Fatal("expected downstream LastError to be cleared")
	}

	cycleID := makeCycleID("s3", []string{"s1", "s2"})
	if graph.CycleCounters[cycleID] != 1 {
		t.Fatalf("expected CycleCounters[%s] == 1, got %d", cycleID, graph.CycleCounters[cycleID])
	}
	if graph.Status != schemas.GraphRunning {
		t.Fatalf("expected graph still GraphRunning, got %v", graph.Status)
	}
}

func TestTryBacktrack_MultipleRounds_IncrementsCounter(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	cycleID := makeCycleID("s2", []string{"s1"})

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 3,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
	}
	engine.graphs = map[string]*schemas.TaskGraph{"t1": graph}

	output := &schemas.SubagentOutput{MissingContext: []string{"data"}}

	for round := 1; round <= 3; round++ {
		upstream.Status = schemas.StepOK
		downstream.Status = schemas.StepPartial

		handled := engine.tryBacktrack(graph, downstream, output)
		if !handled {
			t.Fatalf("round %d: expected handled", round)
		}
		if graph.CycleCounters[cycleID] != round {
			t.Fatalf("round %d: expected counter == %d, got %d", round, round, graph.CycleCounters[cycleID])
		}
		if downstream.Status != schemas.StepPending {
			t.Fatalf("round %d: expected downstream Pending, got %v", round, downstream.Status)
		}
	}
}

func TestTryBacktrack_Fuse_SuspendsAndBlocks(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 2,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID:        "t1",
		Status:        schemas.GraphRunning,
		Steps:         map[string]*schemas.Step{"s1": upstream, "s2": downstream},
		CycleCounters: map[string]int{makeCycleID("s2", []string{"s1"}): 2},
	}
	engine.graphs = map[string]*schemas.TaskGraph{"t1": graph}

	output := &schemas.SubagentOutput{MissingContext: []string{"final_missing"}}
	handled := engine.tryBacktrack(graph, downstream, output)
	if !handled {
		t.Fatal("expected tryBacktrack to return true on fuse")
	}

	if upstream.Status != schemas.StepSuspended {
		t.Fatalf("expected upstream StepSuspended, got %v", upstream.Status)
	}
	if downstream.Status != schemas.StepSuspended {
		t.Fatalf("expected downstream StepSuspended, got %v", downstream.Status)
	}
	if downstream.AdditionalPromptContext == "" {
		t.Fatal("expected degraded prompt injected")
	}
	if graph.Status != schemas.GraphBlocked {
		t.Fatalf("expected GraphBlocked, got %v", graph.Status)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}

	cycleID := makeCycleID("s2", []string{"s1"})
	if _, exists := graph.CycleCounters[cycleID]; exists {
		t.Fatal("expected cycle counter to be removed after fuse")
	}
}

func TestTryBacktrack_FuseDecisionOptions(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 1,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID:        "t1",
		Status:        schemas.GraphRunning,
		Steps:         map[string]*schemas.Step{"s1": upstream, "s2": downstream},
		CycleCounters: map[string]int{makeCycleID("s2", []string{"s1"}): 1},
	}
	engine.graphs = map[string]*schemas.TaskGraph{"t1": graph}

	output := &schemas.SubagentOutput{MissingContext: []string{"x"}}
	engine.tryBacktrack(graph, downstream, output)

	dec := graph.PendingDecisions[0]
	hasOption := func(opt string) bool {
		for _, o := range dec.Options {
			if o == opt {
				return true
			}
		}
		return false
	}
	for _, expected := range []string{"abort", "accept_degraded", "provide_context"} {
		if !hasOption(expected) {
			t.Fatalf("expected decision to offer %q, options: %v", expected, dec.Options)
		}
	}
}
