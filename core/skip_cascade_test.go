package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestFilterSkipOptions_NoFiltering(t *testing.T) {
	options := []string{"skip", "abort", "retry"}
	result := filterSkipOptions(0, 3, nil, options)
	if len(result) != 3 || result[0] != "skip" {
		t.Fatalf("expected no filtering, got %v", result)
	}
}

func TestFilterSkipOptions_CascadeGuard(t *testing.T) {
	options := []string{"skip", "abort"}
	result := filterSkipOptions(3, 3, nil, options)
	if contains(result, "skip") {
		t.Fatalf("skip should be filtered when consecutiveSkips >= maxSkips, got %v", result)
	}
	if !contains(result, "abort") {
		t.Fatalf("abort should remain, got %v", result)
	}
}

func TestFilterSkipOptions_CascadeGuardBelowThreshold(t *testing.T) {
	options := []string{"skip", "abort"}
	result := filterSkipOptions(2, 3, nil, options)
	if !contains(result, "skip") {
		t.Fatalf("skip should NOT be filtered when consecutiveSkips < maxSkips, got %v", result)
	}
}

func TestFilterSkipOptions_ScenarioAware_CapabilityRequired(t *testing.T) {
	ctx := map[string]any{"root_cause": "capability_required"}
	options := []string{"skip", "abort", "retry", "refine_and_retry"}
	result := filterSkipOptions(0, 3, ctx, options)
	if contains(result, "skip") {
		t.Fatalf("skip should be filtered for capability_required, got %v", result)
	}
	if !contains(result, "retry") {
		t.Fatalf("retry should remain, got %v", result)
	}
}

func TestFilterSkipOptions_ScenarioAware_ContextDeficit(t *testing.T) {
	ctx := map[string]any{"root_cause": "context_deficit"}
	options := []string{"skip", "abort"}
	result := filterSkipOptions(0, 3, ctx, options)
	if contains(result, "skip") {
		t.Fatalf("skip should be filtered for context_deficit, got %v", result)
	}
	if !contains(result, "abort") {
		t.Fatalf("abort should remain, got %v", result)
	}
}

func TestFilterSkipOptions_ScenarioAware_OtherRootCause(t *testing.T) {
	ctx := map[string]any{"root_cause": "generative_uncertainty"}
	options := []string{"skip", "abort"}
	result := filterSkipOptions(0, 3, ctx, options)
	if !contains(result, "skip") {
		t.Fatalf("skip should NOT be filtered for generative_uncertainty, got %v", result)
	}
}

func TestFilterSkipOptions_AbortFallback(t *testing.T) {
	ctx := map[string]any{"root_cause": "capability_required"}
	options := []string{"skip", "retry"}
	result := filterSkipOptions(0, 3, ctx, options)
	if contains(result, "skip") {
		t.Fatalf("skip should be filtered, got %v", result)
	}
	if !contains(result, "abort") {
		t.Fatalf("abort should be added as fallback, got %v", result)
	}
}

func TestFilterSkipOptions_EmptyAfterFilter(t *testing.T) {
	options := []string{"skip"}
	result := filterSkipOptions(3, 3, nil, options)
	if len(result) != 0 {
		t.Fatalf("expected empty when only skip was offered and filtered, got %v", result)
	}
}

func TestDefaultNonInteractiveChoice_FallbackChain(t *testing.T) {
	cases := []struct {
		name    string
		dtype   schemas.DecisionType
		options []string
		want    string
		ok      bool
	}{
		{"skip available → skip", schemas.DecisionStepFailed, []string{"skip", "abort"}, "skip", true},
		{"skip filtered → retry", schemas.DecisionStepFailed, []string{"retry", "abort"}, "retry", true},
		{"skip+retry filtered → refine", schemas.DecisionStepFailed, []string{"refine_and_retry", "abort"}, "refine_and_retry", true},
		{"only abort → abort", schemas.DecisionStepFailed, []string{"abort"}, "abort", true},
		{"no valid options → no auto", schemas.DecisionStepFailed, []string{"fulfill"}, "", false},
		{"capability: skip filtered → retry", schemas.DecisionCapabilityRequired, []string{"retry", "abort"}, "retry", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := defaultNonInteractiveChoice(tc.dtype, tc.options)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("choice = %q, want %q", got, tc.want)
			}
		})
	}
}
