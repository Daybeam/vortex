package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// P1 regression coverage: voting is parallel, normalized, and always ends in a
// weighted aggregator that depends on every member.
func TestExpandVoting_Structure(t *testing.T) {
	gd := &GroupDispatcher{}
	group := &config.RoleGroup{
		ID: "vote", Policy: config.GroupPolicyVoting,
		Members: []config.GroupMember{
			{RoleID: "a", TaskTemplate: "A {{input}}", Weight: 0},
			{RoleID: "b", TaskTemplate: "B {{input}}", Weight: 2.5},
		},
	}
	inputs, err := gd.expandVoting(group, "question", map[string]string{"seed": "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 3 {
		t.Fatalf("got %d steps, want two members and one aggregator", len(inputs))
	}
	for i, wantRole := range []string{"a", "b"} {
		if inputs[i].RoleID != wantRole || len(inputs[i].DependsOn) != 0 {
			t.Fatalf("member %d is not independent %s: %#v", i, wantRole, inputs[i])
		}
		if inputs[i].ContextRefs["seed"] != "ctx" {
			t.Fatalf("member %d lost context refs", i)
		}
	}
	agg := inputs[2]
	if !strings.HasPrefix(agg.ID, "vote_vote_") || agg.RoleID != "a" {
		t.Fatalf("unexpected aggregator: %#v", agg)
	}
	if len(agg.DependsOn) != 2 || agg.DependsOn[0] != inputs[0].ID || agg.DependsOn[1] != inputs[1].ID {
		t.Fatalf("aggregator dependencies = %v", agg.DependsOn)
	}
	if !strings.Contains(agg.Task, "weight: 1.00") || !strings.Contains(agg.Task, "weight: 2.50") {
		t.Fatalf("aggregator lacks normalized weights: %q", agg.Task)
	}
}

// P2 regression coverage: chain-of-thought is sequential and each next step
// receives the previous step's output reference.
func TestExpandChainOfThought_Structure(t *testing.T) {
	gd := &GroupDispatcher{}
	group := &config.RoleGroup{
		ID: "chain", Policy: config.GroupPolicyChainOfThought,
		AggregatorRoleID: "editor",
		Members: []config.GroupMember{
			{RoleID: "a", TaskTemplate: "A {{input}}"},
			{RoleID: "b", TaskTemplate: "B {{input}}"},
		},
	}
	inputs, err := gd.expandChainOfThought(group, "question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 3 {
		t.Fatalf("got %d steps, want two members and aggregator", len(inputs))
	}
	if len(inputs[0].DependsOn) != 0 || inputs[0].ContextRefs["prev_step_output"] != "" {
		t.Fatalf("first member unexpectedly depends on prior output: %#v", inputs[0])
	}
	if len(inputs[1].DependsOn) != 1 || inputs[1].DependsOn[0] != inputs[0].ID {
		t.Fatalf("second member dependencies = %v", inputs[1].DependsOn)
	}
	if inputs[1].ContextRefs["prev_step_output"] != inputs[0].ID {
		t.Fatalf("second member context = %v", inputs[1].ContextRefs)
	}
	agg := inputs[2]
	if agg.RoleID != "editor" || len(agg.DependsOn) != 1 || agg.DependsOn[0] != inputs[1].ID {
		t.Fatalf("unexpected aggregator: %#v", agg)
	}
	if agg.ContextRefs["chain_output"] != inputs[1].ID {
		t.Fatalf("aggregator context = %v", agg.ContextRefs)
	}
}
