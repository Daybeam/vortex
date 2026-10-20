package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// These tests pin the four defects that let one task pattern accumulate eight
// unrelated tasks and then get injected into every one of them as "this task
// resembles a fixed sequence of steps ... with a consistently high success
// rate". None of them mention a specific product or evaluation suite: the
// failure mode is an orchestrator that routes many tasks through the same
// roles, which is the normal case, not an AppWorld quirk.

// TestStepSequenceKey_DistinguishesDifferentToolCompositions is the core
// regression: two runs with an identical role triple but completely
// different work must not share a composition key. Before the fix they did
// -- the key was built from role_id + capability + skills only.
func TestStepSequenceKey_DistinguishesDifferentToolCompositions(t *testing.T) {
	const sharedRole = "agent"
	const sharedCap = "day-to-day app task execution"

	runA := []StepRecord{{
		StepID:     "s1",
		RoleID:     sharedRole,
		Capability: sharedCap,
		Trace:      []ToolInteraction{{ToolName: "mail__send_message"}},
	}}
	runB := []StepRecord{{
		StepID:     "s1",
		RoleID:     sharedRole,
		Capability: sharedCap,
		Trace:      []ToolInteraction{{ToolName: "calendar__create_event"}},
	}}

	keyA, keyB := stepSequenceKey(runA), stepSequenceKey(runB)

	if keyA == "" || keyB == "" {
		t.Fatalf("tool calls were recorded, so neither key may be empty: A=%q B=%q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("distinct tool compositions collapsed onto one key %q — "+
			"unrelated tasks will merge into a single pattern and inflate its SampleCount", keyA)
	}
}

// TestStepSequenceKey_RequiresToolEvidence guards the "never merge without
// evidence" default. A record with no trace tells us nothing about how the
// task was carried out, so it must not be treated as evidence that any two
// tasks are the same composition.
func TestStepSequenceKey_RequiresToolEvidence(t *testing.T) {
	recs := []StepRecord{{
		StepID:     "s1",
		RoleID:     "agent",
		Capability: "anything",
	}}
	if got := stepSequenceKey(recs); got != "" {
		t.Fatalf("no tool calls recorded, key must be empty so upsert never merges; got %q", got)
	}
	if got := stepSequenceKey(nil); got != "" {
		t.Fatalf("nil records must yield an empty key; got %q", got)
	}
}

// TestStepSequenceKey_DedupesRepeatedToolCallsInOrder keeps the key useful
// for its real purpose. Calling the same tool twice while working through a
// composition is a variation in execution, not a different composition --
// otherwise retries would defeat composition matching entirely.
func TestStepSequenceKey_DedupesRepeatedToolCallsInOrder(t *testing.T) {
	base := []StepRecord{{
		StepID:     "s1",
		RoleID:     "agent",
		Capability: "c",
		Trace:      []ToolInteraction{{ToolName: "a"}, {ToolName: "b"}, {ToolName: "c"}},
	}}
	withRetries := []StepRecord{{
		StepID:     "s1",
		RoleID:     "agent",
		Capability: "c",
		Trace: []ToolInteraction{
			{ToolName: "a"}, {ToolName: "b"}, {ToolName: "a"},
			{ToolName: "c"}, {ToolName: "b"},
		},
	}}

	if got, want := stepSequenceKey(withRetries), stepSequenceKey(base); got != want {
		t.Fatalf("re-calling a tool must not change the composition key: got %q, want %q", got, want)
	}

	// Order still matters: a different sequence is a different composition.
	reordered := []StepRecord{{
		StepID:     "s1",
		RoleID:     "agent",
		Capability: "c",
		Trace:      []ToolInteraction{{ToolName: "b"}, {ToolName: "a"}, {ToolName: "c"}},
	}}
	if stepSequenceKey(reordered) == stepSequenceKey(base) {
		t.Fatal("tool order must be part of the key: b,a,c must not match a,b,c")
	}
}

// TestUpsertTaskPattern_ManyTasksSameRolesDoNotCollapse replays the incident
// shape end to end: eight tasks, one shared role triple, eight different
// tools. The store must hold eight patterns of one sample each, not one
// pattern claiming eight observations.
func TestUpsertTaskPattern_ManyTasksSameRolesDoNotCollapse(t *testing.T) {
	es, err := NewExperienceStore(t.TempDir(), nil, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}

	const n = 8
	for i := 0; i < n; i++ {
		es.upsertTaskPattern(
			fmt.Sprintf("task-%02d", i),
			"",
			[]StepRecord{{
				StepID:     "s1",
				RoleID:     "agent",
				Capability: "day-to-day app task execution",
				Task:       fmt.Sprintf("distinct task %d", i),
				Trace:      []ToolInteraction{{ToolName: fmt.Sprintf("app%d__do_thing", i)}},
			}},
			0.9,
		)
	}

	if got := len(es.TaskPatterns); got != n {
		t.Fatalf("want %d distinct patterns (one per tool composition), got %d — "+
			"unrelated tasks are still being merged", n, got)
	}
	for _, p := range es.TaskPatterns {
		if p.SampleCount != 1 {
			t.Fatalf("pattern %s: want SampleCount 1, got %d (SourceText=%q) — "+
				"this is the exact contamination that injected one task's history into another",
				p.ID, p.SampleCount, p.SourceText)
		}
	}

	// The same composition repeated must still accumulate, or the feature is dead.
	for i := 0; i < 2; i++ {
		es.upsertTaskPattern(
			fmt.Sprintf("repeat-%d", i),
			"",
			[]StepRecord{{
				StepID:     "s1",
				RoleID:     "agent",
				Capability: "day-to-day app task execution",
				Trace:      []ToolInteraction{{ToolName: "app0__do_thing"}},
			}},
			0.9,
		)
	}
	var repeated *TaskPattern
	for _, p := range es.TaskPatterns {
		if strings.Contains(p.SequenceKey, "app0__do_thing") {
			cp := p
			repeated = &cp
		}
	}
	if repeated == nil {
		t.Fatal("pattern for app0__do_thing disappeared")
	}
	if repeated.SampleCount != 3 {
		t.Fatalf("genuine repetition must accumulate: want SampleCount 3, got %d", repeated.SampleCount)
	}
}

// TestFormatJITCompositionHints_DoesNotClaimSuccessItCannotSupport pins the
// prompt text. The old wording asserted "a consistently high success rate"
// for every pattern regardless of avg confidence, and pointed the model at
// orchestrator_invoke, which core/tool_router.go keeps in
// privilegedToolBlocklist and therefore never exposes.
func TestFormatJITCompositionHints_DoesNotClaimSuccessItCannotSupport(t *testing.T) {
	low := FormatJITCompositionHints([]TaskPattern{
		{ID: "pat_low", SampleCount: 38, AvgConfidence: 0.22},
	})
	if strings.Contains(low, "high success") {
		t.Fatalf("low-confidence hint must not claim a high success rate:\n%s", low)
	}
	if !strings.Contains(low, "low confidence") {
		t.Fatalf("low-confidence hint must be labelled as weak evidence:\n%s", low)
	}
	if strings.Contains(low, "orchestrator_invoke") {
		t.Fatalf("hint must not name a tool the router guarantees is never exposed:\n%s", low)
	}

	high := FormatJITCompositionHints([]TaskPattern{
		{ID: "pat_high", SampleCount: 9, AvgConfidence: 0.90},
	})
	if strings.Contains(high, "low confidence") {
		t.Fatalf("high-confidence hint must not be labelled weak:\n%s", high)
	}
	if !strings.Contains(high, "0.90") {
		t.Fatalf("hint must report the observed confidence, not describe it:\n%s", high)
	}
}
