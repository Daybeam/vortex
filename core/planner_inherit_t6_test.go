package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// T6 regression: planner-produced terminal steps must inherit the caller's
// per-run settings (ContextRefs, VerifierModel, MaxAutoRefine, EnableDebate,
// MaxDebateRounds, AdditionalSkills, Deliver). The original inline loop only
// inherited AdditionalSkills and Deliver, silently dropping the rest.
//
// Bug being pinned: a planner-produced terminal step that lost its ContextRefs
// could not receive upstream results; one that lost VerifierModel fell back to
// the default verifier, changing audit semantics.

func TestInheritCallerSettings_TerminalStepsInheritAll(t *testing.T) {
	planned := []schemas.StepInput{
		{ID: "s1", RoleID: "agent", Task: "intermediate"},
		{ID: "s2", RoleID: "agent", Task: "terminal", DependsOn: []string{"s1"}},
	}
	original := schemas.StepInput{
		AdditionalSkills: []string{"skill-x"},
		Deliver:          "artifact://out",
		ContextRefs:      map[string]string{"upstream": "s1.output"},
		VerifierModel:    "gemini-3-flash",
		MaxAutoRefine:    3,
		EnableDebate:     true,
		MaxDebateRounds:  2,
	}

	inheritCallerSettings(planned, original)

	// s2 is terminal → inherits everything.
	s2 := planned[1]
	if s2.Deliver != "artifact://out" {
		t.Fatalf("Deliver not inherited: %q", s2.Deliver)
	}
	if len(s2.AdditionalSkills) != 1 || s2.AdditionalSkills[0] != "skill-x" {
		t.Fatalf("AdditionalSkills not inherited: %v", s2.AdditionalSkills)
	}
	if s2.ContextRefs["upstream"] != "s1.output" {
		t.Fatalf("ContextRefs not inherited: %v", s2.ContextRefs)
	}
	if s2.VerifierModel != "gemini-3-flash" {
		t.Fatalf("VerifierModel not inherited: %q", s2.VerifierModel)
	}
	if s2.MaxAutoRefine != 3 {
		t.Fatalf("MaxAutoRefine not inherited: %d", s2.MaxAutoRefine)
	}
	if !s2.EnableDebate {
		t.Fatal("EnableDebate not inherited")
	}
	if s2.MaxDebateRounds != 2 {
		t.Fatalf("MaxDebateRounds not inherited: %d", s2.MaxDebateRounds)
	}
}

// Intermediate steps must NOT inherit per-run settings — e.g. the caller's
// Deliver contract belongs on the final step, not on every fan-out branch.
func TestInheritCallerSettings_IntermediateStepsNotTouched(t *testing.T) {
	planned := []schemas.StepInput{
		{ID: "s1", RoleID: "agent", Task: "intermediate"},
		{ID: "s2", RoleID: "agent", Task: "terminal", DependsOn: []string{"s1"}},
	}
	original := schemas.StepInput{
		Deliver:       "artifact://out",
		VerifierModel: "gemini-3-flash",
		ContextRefs:   map[string]string{"k": "v"},
	}

	inheritCallerSettings(planned, original)

	s1 := planned[0]
	if s1.Deliver != "" {
		t.Fatalf("intermediate step should not inherit Deliver: %q", s1.Deliver)
	}
	if s1.VerifierModel != "" {
		t.Fatalf("intermediate step should not inherit VerifierModel: %q", s1.VerifierModel)
	}
	if len(s1.ContextRefs) != 0 {
		t.Fatalf("intermediate step should not inherit ContextRefs: %v", s1.ContextRefs)
	}
}

// Multiple terminal steps (fan-out) all inherit.
func TestInheritCallerSettings_MultipleTerminals(t *testing.T) {
	planned := []schemas.StepInput{
		{ID: "s1", RoleID: "agent", Task: "fan-out A"},
		{ID: "s2", RoleID: "agent", Task: "fan-out B"},
	}
	original := schemas.StepInput{
		Deliver:       "artifact://out",
		VerifierModel: "qwen-9b",
	}

	inheritCallerSettings(planned, original)

	for i, st := range planned {
		if st.Deliver != "artifact://out" {
			t.Fatalf("terminal step %d did not inherit Deliver: %q", i, st.Deliver)
		}
		if st.VerifierModel != "qwen-9b" {
			t.Fatalf("terminal step %d did not inherit VerifierModel: %q", i, st.VerifierModel)
		}
	}
}
