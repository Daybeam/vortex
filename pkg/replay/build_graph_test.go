package replay

import (
	"reflect"
	"testing"
)

// buildSubmittedGraph is the read-side half of the explicit DAG contract:
// task_submitted carries a compact plan snapshot, and replay must turn it into
// Graph without inventing nodes that were never declared.
func testSteps(ids ...string) []Step {
	out := make([]Step, 0, len(ids))
	for _, id := range ids {
		out = append(out, Step{ID: id})
	}
	return out
}

func testSubmitted(detail map[string]any) []rawEvent {
	return []rawEvent{{Event: "task_submitted", Detail: detail}}
}

func graphNode(g *Graph, id string) *GraphStep {
	for i := range g.Steps {
		if g.Steps[i].ID == id {
			return &g.Steps[i]
		}
	}
	return nil
}

func TestBuildSubmittedGraphNoSnapshotStaysInferred(t *testing.T) {
	events := testSubmitted(map[string]any{"step_count": 5})
	if g := buildSubmittedGraph(testSteps("s1", "s2"), events); g != nil {
		t.Fatalf("no plan snapshot must stay on the inference path, got %+v", g)
	}
	// Non-submission events never contribute a graph.
	events = []rawEvent{{Event: "step_completed", Detail: map[string]any{"step_ids": []any{"s1"}}}}
	if g := buildSubmittedGraph(testSteps("s1"), events); g != nil {
		t.Fatalf("only task_submitted declares the plan, got %+v", g)
	}
}

func TestBuildSubmittedGraphFullyParallelIsExplicit(t *testing.T) {
	// The old deps-only snapshot skipped dependency-free roots, so a plan like
	// 390fbe51 logged nothing at all. step_ids fixes that.
	events := testSubmitted(map[string]any{
		"step_ids": []any{"s5", "s1", "s4", "s2", "s3"},
	})
	g := buildSubmittedGraph(testSteps("s1", "s2", "s3", "s4", "s5"), events)
	if g == nil {
		t.Fatal("an edge-less plan is still an explicit fan-out graph")
	}
	want := []string{"s1", "s2", "s3", "s4", "s5"}
	if len(g.Steps) != len(want) {
		t.Fatalf("graph has %d nodes, want %d", len(g.Steps), len(want))
	}
	for i, id := range want {
		if g.Steps[i].ID != id {
			t.Errorf("node %d = %q, want %q (sorted for determinism)", i, g.Steps[i].ID, id)
		}
		if len(g.Steps[i].DependsOn) != 0 {
			t.Errorf("parallel root %q must have no inbound edges, got %v", id, g.Steps[i].DependsOn)
		}
	}
}

func TestBuildSubmittedGraphLegacyDepsFallback(t *testing.T) {
	// Historical runs have no step_ids. The executed step IDs are still an
	// honest node set for the edges we did record.
	events := testSubmitted(map[string]any{
		"deps": map[string]any{
			"s3": []any{"s2", "s1", "s1", "s3", "ghost"},
		},
	})
	g := buildSubmittedGraph(testSteps("s1", "s2", "s3"), events)
	if g == nil {
		t.Fatal("legacy deps must still produce a graph")
	}
	if len(g.Steps) != 3 {
		t.Fatalf("graph has %d nodes, want 3", len(g.Steps))
	}
	node := graphNode(g, "s3")
	if node == nil {
		t.Fatal("s3 missing from the graph")
	}
	if want := []string{"s1", "s2"}; !reflect.DeepEqual(node.DependsOn, want) {
		t.Errorf("s3.DependsOn = %v, want %v (dedup, no self-loop, no unknown node)", node.DependsOn, want)
	}
}

func TestBuildSubmittedGraphKeepsJoinNodeWithoutTelemetry(t *testing.T) {
	// Real run 390fbe51: {"s6": ["s1".."s5"]} is a five-way join, but s6 never
	// produced telemetry. Dropping it would mislabel the plan as five unrelated
	// parallel roots instead of one fan-out joined by a barrier.
	events := testSubmitted(map[string]any{
		"deps": map[string]any{
			"s6": []any{"s1", "s2", "s3", "s4", "s5"},
		},
	})
	g := buildSubmittedGraph(testSteps("s1", "s2", "s3", "s4", "s5"), events)
	if g == nil {
		t.Fatal("a declared join must produce a graph")
	}
	if len(g.Steps) != 6 {
		t.Fatalf("graph has %d nodes, want 6 (s1..s5 plus the unexecuted join s6)", len(g.Steps))
	}
	join := graphNode(g, "s6")
	if join == nil {
		t.Fatal("join node s6 was dropped")
	}
	if want := []string{"s1", "s2", "s3", "s4", "s5"}; !reflect.DeepEqual(join.DependsOn, want) {
		t.Errorf("s6.DependsOn = %v, want %v", join.DependsOn, want)
	}
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
		if n := graphNode(g, id); n == nil || len(n.DependsOn) != 0 {
			t.Errorf("%s must stay a dependency-free parallel root, got %+v", id, n)
		}
	}
}

func TestBuildSubmittedGraphPrefersPlannedNodeSet(t *testing.T) {
	events := testSubmitted(map[string]any{
		"step_ids": []any{"s1", "s2", "s3"},
		"deps":     map[string]any{"s3": []any{"s1"}},
	})
	// s4 only exists in telemetry (e.g. a retry), not in the submitted plan.
	g := buildSubmittedGraph(testSteps("s1", "s2", "s3", "s4"), events)
	if g == nil {
		t.Fatal("expected a graph")
	}
	if len(g.Steps) != 3 {
		t.Fatalf("graph has %d nodes, want the 3 planned ones", len(g.Steps))
	}
	if graphNode(g, "s4") != nil {
		t.Error("unplanned telemetry node s4 must not be invented as a graph node")
	}
	if node := graphNode(g, "s3"); node == nil || !reflect.DeepEqual(node.DependsOn, []string{"s1"}) {
		t.Errorf("s3 edges = %+v, want [s1]", node)
	}
}

func TestBuildSubmittedGraphNoStepsIsNil(t *testing.T) {
	events := testSubmitted(map[string]any{"step_ids": []any{"s1"}})
	if g := buildSubmittedGraph(nil, events); g != nil {
		t.Fatalf("a run with no executed steps has nothing to draw, got %+v", g)
	}
}
