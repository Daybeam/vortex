package tools

import (
	"testing"

	"github.com/daybeam/vortex/core"
)

// tools_task_host_capabilities_test.go — regression test for host-capability-based
// execution hint selection (Layer 1 of the host-compatibility extension).
//
// Verifies:
// 1. executionHintForCapabilities selects the correct mode for each capability.
// 2. Priority order: acp > subagent_dispatch > local_fs > broker.
// 3. enrichDelegationViews uses capabilities when provided, falls back to tier when nil.
// 4. Fallback field is set for subagent_dispatch mode.

func TestExecutionHintForCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		caps       []string
		wantMode   string
		wantNil    bool
		wantFallbk string
	}{
		{"acp", []string{"acp"}, "acp", false, ""},
		{"subagent_dispatch", []string{"subagent_dispatch"}, "subagent", false, "local_fs"},
		{"local_fs", []string{"local_fs"}, "local_fs", false, ""},
		{"broker", []string{"broker"}, "broker", false, ""},
		{"empty", nil, "", true, ""},
		{"unrecognized_only", []string{"foo", "bar"}, "", true, ""},
		{"priority_acp_over_subagent", []string{"subagent_dispatch", "acp"}, "acp", false, ""},
		{"priority_subagent_over_local_fs", []string{"local_fs", "subagent_dispatch"}, "subagent", false, "local_fs"},
		{"priority_local_fs_over_broker", []string{"broker", "local_fs"}, "local_fs", false, ""},
		{"unrecognized_plus_local_fs", []string{"foo", "local_fs"}, "local_fs", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint := executionHintForCapabilities(tt.caps)
			if tt.wantNil {
				if hint != nil {
					t.Fatalf("expected nil hint, got mode=%q", hint.Mode)
				}
				return
			}
			if hint == nil {
				t.Fatal("expected non-nil hint, got nil")
			}
			if hint.Mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", hint.Mode, tt.wantMode)
			}
			if hint.Fallback != tt.wantFallbk {
				t.Errorf("fallback = %q, want %q", hint.Fallback, tt.wantFallbk)
			}
		})
	}
}

func TestEnrichDelegationViews_WithCapabilities(t *testing.T) {
	views := []core.DelegationView{
		{StepID: "s1", DecisionID: "d1"},
		{StepID: "s2", DecisionID: "d2"},
	}
	enrichDelegationViews(views, "admin", []string{"subagent_dispatch"})
	for i, v := range views {
		if v.ExecutionHint == nil {
			t.Fatalf("view[%d] ExecutionHint is nil", i)
		}
		if v.ExecutionHint.Mode != "subagent" {
			t.Errorf("view[%d] mode = %q, want %q", i, v.ExecutionHint.Mode, "subagent")
		}
		if v.ExecutionHint.Fallback != "local_fs" {
			t.Errorf("view[%d] fallback = %q, want %q", i, v.ExecutionHint.Fallback, "local_fs")
		}
	}
}

func TestEnrichDelegationViews_FallbackToTier(t *testing.T) {
	views := []core.DelegationView{
		{StepID: "s1", DecisionID: "d1"},
	}
	// nil capabilities → fall back to tier-based logic
	enrichDelegationViews(views, "admin", nil)
	if views[0].ExecutionHint == nil {
		t.Fatal("ExecutionHint is nil")
	}
	if views[0].ExecutionHint.Mode != "local_fs" {
		t.Errorf("mode = %q, want %q (tier fallback)", views[0].ExecutionHint.Mode, "local_fs")
	}
}

func TestEnrichDelegationViews_EmptyCapsFallsBackToTier(t *testing.T) {
	views := []core.DelegationView{
		{StepID: "s1", DecisionID: "d1"},
	}
	// empty (non-nil) capabilities with no recognized values → fall back to tier
	enrichDelegationViews(views, "admin", []string{"unknown_cap"})
	if views[0].ExecutionHint == nil {
		t.Fatal("ExecutionHint is nil")
	}
	if views[0].ExecutionHint.Mode != "local_fs" {
		t.Errorf("mode = %q, want %q (tier fallback for unrecognized caps)", views[0].ExecutionHint.Mode, "local_fs")
	}
}
