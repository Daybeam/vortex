//go:build test
// +build test

package schemas

import (
	"testing"
)

func TestTaskGraph_ReadySteps(t *testing.T) {
	steps := map[string]*Step{
		"step1": {ID: "step1", Status: StepOK},
		"step2": {ID: "step2", Status: StepPending, DependsOn: []string{"step1"}},
		"step3": {ID: "step3", Status: StepPending, DependsOn: []string{"step2"}},
		"step4": {ID: "step4", Status: StepPending, DependsOn: []string{"step1"}},
	}
	graph := &TaskGraph{Steps: steps, Status: GraphRunning}

	ready := graph.ReadySteps()
	if len(ready) != 2 {
		t.Errorf("expected 2 ready steps (step2, step4), got %d", len(ready))
	}

	found2, found4 := false, false
	for _, s := range ready {
		if s.ID == "step2" {
			found2 = true
		}
		if s.ID == "step4" {
			found4 = true
		}
	}
	if !found2 || !found4 {
		t.Errorf("ready steps did not contain expected IDs")
	}
}

func TestTaskGraph_IsTerminal(t *testing.T) {
	graph := &TaskGraph{
		Steps: map[string]*Step{
			"s1": {Status: StepOK},
			"s2": {Status: StepFailed},
		},
	}
	if !graph.IsTerminal() {
		t.Error("graph with all steps in terminal state should be terminal")
	}

	graph.Steps["s3"] = &Step{Status: StepPending}
	if graph.IsTerminal() {
		t.Error("graph with pending steps should not be terminal")
	}
}
