package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestDetectUpstreamInsufficient_UpstreamFailed detects a pending
// downstream step whose upstream dependency has failed (terminal but
// not StepOK/StepPartial), triggering the upstream_insufficient decision.
func TestDetectUpstreamInsufficient_UpstreamFailed(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"upstream":   {ID: "upstream", Status: schemas.StepFailed, LastError: "error"},
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info == nil {
		t.Fatal("Expected upstream_insufficient detection, got nil")
	}
	if info.Step.ID != "downstream" {
		t.Errorf("Step ID = %s, want downstream", info.Step.ID)
	}
	if len(info.UpstreamIDs) != 1 || info.UpstreamIDs[0] != "upstream" {
		t.Errorf("UpstreamIDs = %v, want [upstream]", info.UpstreamIDs)
	}
}

// TestDetectUpstreamInsufficient_UpstreamRunning returns nil when upstream
// is still running (not terminal) — the graph should just wait.
func TestDetectUpstreamInsufficient_UpstreamRunning(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"upstream":   {ID: "upstream", Status: schemas.StepRunning},
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Error("Upstream still running, detection should return nil (wait)")
	}
}

// TestDetectUpstreamInsufficient_UpstreamOK returns nil when upstream is
// successfully done — downstream should be ready via ReadySteps().
func TestDetectUpstreamInsufficient_UpstreamOK(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"upstream":   {ID: "upstream", Status: schemas.StepOK},
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Error("Upstream OK, detection should return nil (downstream is ready)")
	}
}

// TestDetectUpstreamInsufficient_UpstreamPartial returns nil — partial
// success is sufficient, downstream should proceed.
func TestDetectUpstreamInsufficient_UpstreamPartial(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"upstream":   {ID: "upstream", Status: schemas.StepPartial},
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Error("Upstream Partial, detection should return nil")
	}
}

// TestDetectUpstreamInsufficient_UpstreamBlocked returns detection info
// since StepBlocked is terminal-but-not-success.
func TestDetectUpstreamInsufficient_UpstreamBlocked(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"upstream":   {ID: "upstream", Status: schemas.StepBlocked},
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info == nil {
		t.Fatal("Expected upstream_insufficient detection for blocked upstream")
	}
	if info.UpstreamStatuses["upstream"] != schemas.StepBlocked {
		t.Errorf("UpstreamStatus = %s, want %s", info.UpstreamStatuses["upstream"], schemas.StepBlocked)
	}
}

// TestDetectUpstreamInsufficient_MultiDep_FailedOneWithOKOne detects
// the situation when one of multiple upstreams failed.
func TestDetectUpstreamInsufficient_MultiDep_FailedOneWithOKOne(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"step_a":   {ID: "step_a", Status: schemas.StepOK},
			"step_b":   {ID: "step_b", Status: schemas.StepFailed, LastError: "failed"},
			"consumer": {ID: "consumer", Status: schemas.StepPending, DependsOn: []string{"step_a", "step_b"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info == nil {
		t.Fatal("Expected detection when one upstream failed")
	}
	if info.Step.ID != "consumer" {
		t.Errorf("Step ID = %s, want consumer", info.Step.ID)
	}
	if len(info.UpstreamIDs) != 2 {
		t.Errorf("UpstreamIDs = %v, want 2 deps", info.UpstreamIDs)
	}
	if info.UpstreamStatuses["step_b"] != schemas.StepFailed {
		t.Errorf("step_b status = %s, want %s", info.UpstreamStatuses["step_b"], schemas.StepFailed)
	}
}

// TestDetectUpstreamInsufficient_DependentSkipped detects skipped as
// insufficient when it's not explicitly handled by the scheduler.
func TestDetectUpstreamInsufficient_DependentSkipped(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"upstream":   {ID: "upstream", Status: schemas.StepSkipped},
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"upstream"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info == nil {
		t.Fatal("Expected detection when upstream was skipped")
	}
}

// TestDetectUpstreamInsufficient_NoDeps returns nil — a pending step
// with no deps should be ready (not insufficient).
func TestDetectUpstreamInsufficient_NoDeps(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"orphan": {ID: "orphan", Status: schemas.StepPending, DependsOn: []string{}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Error("Pending step with no deps should not trigger detection")
	}
}

// TestDetectUpstreamInsufficient_MissingDependency returns nil when
// the depends-on reference doesn't exist in graph (unknown dep).
func TestDetectUpstreamInsufficient_MissingDependency(t *testing.T) {
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		Steps: map[string]*schemas.Step{
			"downstream": {ID: "downstream", Status: schemas.StepPending, DependsOn: []string{"ghost"}},
		},
	}

	info := engine.detectUpstreamInsufficient(graph)
	if info != nil {
		t.Error("Missing dependency should return nil (unknown dep — not a terminal failure)")
	}
}
