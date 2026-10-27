package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_intent_shift_test.go — Context Tree Intent-Shift Branching (B11).
// See docs/plan/CONTEXT_TREE_INTENT_SHIFT_BRANCHING_DESIGN.md §4.
//
// Pattern note: we call engine.transitionContextTree directly on a
// hand-built graph (same approach as TestDirectedEngine_ContextTreeBranching)
// instead of driving the full DAG — the branching decision is the unit under
// test. Engine directories use mkdirTemp (Windows t.TempDir cleanup race,
// see delegation_mode_test.go). mockTaskStore comes from
// scheduler_tree_test.go.

// intentShiftTestEngine builds a DirectedEngine wired to mockTaskStore with
// the given SystemSettings. The returned logger is closed by t.Cleanup.
func intentShiftTestEngine(t *testing.T, sys *config.SystemSettings) (*DirectedEngine, *config.Registry) {
	t.Helper()
	reg := &config.Registry{
		Roles:  make(map[string]*config.Role),
		Skills: make(map[string]*config.Skill),
		MCPs:   make(map[string]*config.MCPDef),
	}
	reg.System = *sys
	reg.Roles["auditor"] = &config.Role{ID: "auditor", BaseCapability: "audit"}

	dir := mkdirTemp(t)
	logger := mustNewLogger(t, dir, sys)
	engine := NewDirectedEngine(reg, &mockTaskStore{}, nil, nil, logger, nil, dir, dir, nil)
	t.Cleanup(engine.Stop)
	return engine, reg
}

// intentShiftGraph builds a minimal single-node graph whose root node and
// step carry the given embeddings (nil = cold cache).
func intentShiftGraph(nodeEmb, stepEmb []float32, assumptions []string) (*schemas.TaskGraph, *schemas.Step, *SpawnResult) {
	step := &schemas.Step{
		ID:        "s1",
		Task:      "audit the authentication module",
		Status:    schemas.StepOK,
		Embedding: stepEmb,
	}
	graph := &schemas.TaskGraph{
		TaskID: "task-intent-shift",
		Steps:  map[string]*schemas.Step{"s1": step},
		ContextTree: map[string]*schemas.ContextNode{
			"root": {
				ID:       "root",
				Intent:   "build a REST API",
				Status:   schemas.NodeActive,
				Metadata: map[string]any{"project": "demo", "tier": "public"},
				Embedding: nodeEmb,
			},
		},
		CurrentNodeID: "root",
	}
	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:      schemas.StatusOK,
			Confidence:  0.9,
			Result:      map[string]any{"data": "done"},
			Assumptions: assumptions,
		},
	}
	return graph, step, result
}

// waitForBranchEvent drains the subscription until the intent_shift_branch
// event arrives (other engine events — asset sentry etc. — are skipped).
func waitForBranchEvent(t *testing.T, ch <-chan LogEvent) LogEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event subscription closed before intent_shift_branch arrived")
			}
			if ev.Event == EventIntentShiftBranch {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for intent_shift_branch event")
		}
	}
}

// assertNoBranchEvent fails if an intent_shift_branch event shows up within
// the drain window. The decision is synchronous inside transitionContextTree,
// so a correct no-branch path never emits the event at all; the window only
// guards against the async writer-goroutine fan-out lag.
func assertNoBranchEvent(t *testing.T, ch <-chan LogEvent) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Event == EventIntentShiftBranch {
				t.Fatalf("unexpected intent_shift_branch event: %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}

func strPtr(b bool) *bool { return &b }

// ── Case 1: semantic drift branches ───────────────────────────────────────

// Feature: Context Tree
// Scenario: Semantic intent shift — cosine(node, step) below threshold
//
//	creates ContextNode_B with inherited metadata and cleared LocalSymbolIndex
//
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 上下文树
// -----------------------------------------------------------------------------
func TestTransitionContextTree_SemanticDrift_Branches(t *testing.T) {
	engine, _ := intentShiftTestEngine(t, &config.SystemSettings{})

	// Orthogonal vectors ⇒ cosine 0 < default threshold 0.20 ⇒ drift.
	graph, step, result := intentShiftGraph(
		[]float32{1, 0, 0},
		[]float32{0, 1, 0},
		nil,
	)

	events := engine.logger.SubscribeEvents()
	defer engine.logger.UnsubscribeEvents(events)

	engine.transitionContextTree(graph, step, result)

	if len(graph.ContextTree) != 2 {
		t.Fatalf("expected 2 context nodes after semantic branch, got %d", len(graph.ContextTree))
	}
	newNode := graph.ContextTree[graph.CurrentNodeID]
	if newNode == nil {
		t.Fatalf("CurrentNodeID %q not found in ContextTree", graph.CurrentNodeID)
	}
	if newNode.ParentID != "root" {
		t.Errorf("expected new node ParentID=root, got %q", newNode.ParentID)
	}
	if newNode.LocalSymbolIndex != nil {
		t.Error("expected LocalSymbolIndex to be nil (interference isolation)")
	}
	if newNode.Metadata["project"] != "demo" || newNode.Metadata["tier"] != "public" {
		t.Errorf("expected root metadata inherited, got %v", newNode.Metadata)
	}
	if newNode.Intent != step.Task {
		t.Errorf("expected new node Intent to be the drifted task text %q, got %q", step.Task, newNode.Intent)
	}
	// Parent keeps its StepIDs entry.
	root := graph.ContextTree["root"]
	if len(root.StepIDs) != 1 || root.StepIDs[0] != "s1" {
		t.Errorf("expected root.StepIDs=[s1], got %v", root.StepIDs)
	}

	ev := waitForBranchEvent(t, events)
	if ev.Detail["mode"] != intentShiftModeSemantic {
		t.Errorf("expected telemetry mode=semantic, got %v", ev.Detail["mode"])
	}
	if score, _ := ev.Detail["drift_score"].(float64); score >= defaultIntentShiftThreshold {
		t.Errorf("expected telemetry drift_score < %v, got %v", defaultIntentShiftThreshold, ev.Detail["drift_score"])
	}
	if thr, _ := ev.Detail["threshold"].(float64); thr != defaultIntentShiftThreshold {
		t.Errorf("expected telemetry threshold=%v, got %v", defaultIntentShiftThreshold, ev.Detail["threshold"])
	}
}

// ── Case 2: aligned vectors do not branch ─────────────────────────────────

// Feature: Context Tree
// Scenario: Aligned intent — cosine(node, step) above threshold must NOT
//
//	branch (no fragmentation on similar embeddings)
//
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 上下文树
// -----------------------------------------------------------------------------
func TestTransitionContextTree_Aligned_NoBranch(t *testing.T) {
	engine, _ := intentShiftTestEngine(t, &config.SystemSettings{})

	// Identical vectors ⇒ cosine 1 > 0.20 ⇒ no drift, no branch.
	graph, step, result := intentShiftGraph(
		[]float32{1, 0, 0},
		[]float32{1, 0, 0},
		nil,
	)

	events := engine.logger.SubscribeEvents()
	defer engine.logger.UnsubscribeEvents(events)

	engine.transitionContextTree(graph, step, result)

	if len(graph.ContextTree) != 1 {
		t.Errorf("expected no branch (1 node), got %d", len(graph.ContextTree))
	}
	if graph.CurrentNodeID != "root" {
		t.Errorf("expected CurrentNodeID to stay root, got %q", graph.CurrentNodeID)
	}
	assertNoBranchEvent(t, events)
}

// ── Case 3: cold cache falls back to the heuristic ────────────────────────

// Feature: Context Tree
// Scenario: Cold cache — no cached embeddings, model self-reports "new task"
//
//	⇒ heuristic fallback branch (pre-2026-10-25 behavior preserved)
//
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 上下文树
// -----------------------------------------------------------------------------
func TestTransitionContextTree_ColdCache_HeuristicFallback(t *testing.T) {
	engine, _ := intentShiftTestEngine(t, &config.SystemSettings{})

	// No embeddings on either side (cold cache) but the model self-reports
	// a new task ⇒ heuristic branch (pre-2026-10-25 behavior preserved).
	graph, step, result := intentShiftGraph(nil, nil, []string{"I will start new task now"})

	events := engine.logger.SubscribeEvents()
	defer engine.logger.UnsubscribeEvents(events)

	engine.transitionContextTree(graph, step, result)

	if len(graph.ContextTree) != 2 {
		t.Fatalf("expected heuristic branch (2 nodes), got %d", len(graph.ContextTree))
	}
	ev := waitForBranchEvent(t, events)
	if ev.Detail["mode"] != intentShiftModeHeuristic {
		t.Errorf("expected telemetry mode=heuristic, got %v", ev.Detail["mode"])
	}
	if score, _ := ev.Detail["drift_score"].(float64); score != -1 {
		t.Errorf("expected telemetry drift_score=-1 for heuristic branch, got %v", ev.Detail["drift_score"])
	}
}

// ── Case 4: disabled ⇒ heuristic only ─────────────────────────────────────

// Feature: Context Tree
// Scenario: Branching disabled — intent_shift_branching_enabled=false keeps
//
//	heuristic-only branching (zero behavior change vs pre-wiring)
//
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 上下文树
// -----------------------------------------------------------------------------
func TestTransitionContextTree_Disabled(t *testing.T) {
	engine, _ := intentShiftTestEngine(t, &config.SystemSettings{
		IntentShiftBranchingEnabled: strPtr(false),
	})

	// (a) Semantic trigger suppressed: orthogonal vectors, no keyword.
	graph, step, result := intentShiftGraph(
		[]float32{1, 0, 0},
		[]float32{0, 1, 0},
		nil,
	)
	events := engine.logger.SubscribeEvents()
	engine.transitionContextTree(graph, step, result)
	if len(graph.ContextTree) != 1 {
		t.Errorf("disabled: expected no semantic branch, got %d nodes", len(graph.ContextTree))
	}
	assertNoBranchEvent(t, events)

	// (b) Heuristic still works when disabled (equals pre-wiring behavior).
	graph2, step2, result2 := intentShiftGraph(nil, nil, []string{"starting new task"})
	engine.transitionContextTree(graph2, step2, result2)
	if len(graph2.ContextTree) != 2 {
		t.Errorf("disabled: expected heuristic branch to still work, got %d nodes", len(graph2.ContextTree))
	}
	waitForBranchEvent(t, events) // mode=heuristic
	engine.logger.UnsubscribeEvents(events)
}

// ── Case 5: no embedding, no heuristic ⇒ no branch (no false positive) ────

// Feature: Context Tree
// Scenario: No signal — no cached embeddings and no heuristic keyword must
//
//	NOT branch (guard against false-positive fragmentation)
//
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 上下文树
// -----------------------------------------------------------------------------
func TestTransitionContextTree_NoEmbedNoHeuristic(t *testing.T) {
	engine, _ := intentShiftTestEngine(t, &config.SystemSettings{})

	graph, step, result := intentShiftGraph(nil, nil, []string{"all checks passed"})

	events := engine.logger.SubscribeEvents()
	defer engine.logger.UnsubscribeEvents(events)

	engine.transitionContextTree(graph, step, result)

	if len(graph.ContextTree) != 1 {
		t.Errorf("expected no branch (no embed, no heuristic), got %d", len(graph.ContextTree))
	}
	assertNoBranchEvent(t, events)
}

// ── Case 6: custom threshold from config is honored ───────────────────────

// Feature: Context Tree
// Scenario: Custom threshold — intent_shift_threshold=0.90 makes a moderate
//
//	similarity drop (≈0.707) count as drift
//
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: 上下文树
// -----------------------------------------------------------------------------
func TestTransitionContextTree_CustomThreshold_Branches(t *testing.T) {
	engine, _ := intentShiftTestEngine(t, &config.SystemSettings{
		IntentShiftThreshold: 0.90, // stricter than default 0.20
	})

	// cosine([1,1,0],[1,0,0]) = 1/√2 ≈ 0.707: no drift at the default
	// threshold, drift at the configured 0.90.
	graph, step, result := intentShiftGraph(
		[]float32{1, 1, 0},
		[]float32{1, 0, 0},
		nil,
	)

	events := engine.logger.SubscribeEvents()
	defer engine.logger.UnsubscribeEvents(events)

	engine.transitionContextTree(graph, step, result)

	if len(graph.ContextTree) != 2 {
		t.Fatalf("expected branch under custom threshold 0.90, got %d nodes", len(graph.ContextTree))
	}
	ev := waitForBranchEvent(t, events)
	if thr, _ := ev.Detail["threshold"].(float64); thr != 0.90 {
		t.Errorf("expected telemetry threshold=0.90, got %v", ev.Detail["threshold"])
	}
}

// ── Pure helper: dimension mismatch is "unavailable", not "maximal drift" ─

func TestIntentShifted_DimensionMismatch_NoDrift(t *testing.T) {
	// CosineSimilarity returns 0.0 for unequal lengths, which would look
	// like maximal drift. The guard must treat it as unavailable.
	drift, score := intentShifted([]float32{1, 0, 0}, []float32{1, 0, 0, 0}, 0.20)
	if drift || score != -1 {
		t.Errorf("dimension mismatch: expected (false, -1), got (%v, %v)", drift, score)
	}
	// Cold cache on either side.
	if drift, score := intentShifted(nil, []float32{1}, 0.20); drift || score != -1 {
		t.Errorf("nil node embedding: expected (false, -1), got (%v, %v)", drift, score)
	}
	if drift, score := intentShifted([]float32{1}, nil, 0.20); drift || score != -1 {
		t.Errorf("nil step embedding: expected (false, -1), got (%v, %v)", drift, score)
	}
	// Boundary: score exactly at threshold is NOT drift (strict <).
	if drift, _ := intentShifted([]float32{1, 0}, []float32{1, 0}, 1.0); drift {
		t.Error("score == threshold must not count as drift (strict <)")
	}
}
