package schemas

import (
	"testing"
	"time"
)

// TestSubstitutionLedger_RecordAndRetrieve verifies that RecordSubstitution
// appends to the ledger and GetSubstitutions returns a safe snapshot.
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P3.
func TestSubstitutionLedger_RecordAndRetrieve(t *testing.T) {
	g := &TaskGraph{TaskID: "test_task"}

	// Initially empty
	subs := g.GetSubstitutions()
	if len(subs) != 0 {
		t.Fatalf("expected 0 substitutions, got %d", len(subs))
	}

	// Record a role fallback
	g.RecordSubstitution("role_fallback", "product_visionary", "orchestrator_default", "role not found", "step_1")
	// Record an MCP degradation
	g.RecordSubstitution("mcp_degraded", "tushare_mcp", "", "dependency not found", "step_2")

	subs = g.GetSubstitutions()
	if len(subs) != 2 {
		t.Fatalf("expected 2 substitutions, got %d", len(subs))
	}

	if subs[0].Kind != "role_fallback" {
		t.Errorf("subs[0].Kind = %q, want %q", subs[0].Kind, "role_fallback")
	}
	if subs[0].Requested != "product_visionary" {
		t.Errorf("subs[0].Requested = %q, want %q", subs[0].Requested, "product_visionary")
	}
	if subs[0].Actual != "orchestrator_default" {
		t.Errorf("subs[0].Actual = %q, want %q", subs[0].Actual, "orchestrator_default")
	}
	if subs[0].StepID != "step_1" {
		t.Errorf("subs[0].StepID = %q, want %q", subs[0].StepID, "step_1")
	}
	if subs[0].At.IsZero() {
		t.Error("subs[0].At should not be zero")
	}

	if subs[1].Kind != "mcp_degraded" {
		t.Errorf("subs[1].Kind = %q, want %q", subs[1].Kind, "mcp_degraded")
	}
}

// TestSubstitutionLedger_GetSubstitutionsIsSnapshot verifies that the returned
// slice is a copy — mutating it does not affect the ledger.
func TestSubstitutionLedger_GetSubstitutionsIsSnapshot(t *testing.T) {
	g := &TaskGraph{TaskID: "test_task"}
	g.RecordSubstitution("role_fallback", "a", "b", "c", "d")

	subs := g.GetSubstitutions()
	subs[0].Kind = "mutated"

	// Re-fetch — original should be unchanged
	subs2 := g.GetSubstitutions()
	if subs2[0].Kind != "role_fallback" {
		t.Errorf("ledger was mutated via snapshot: Kind = %q, want %q", subs2[0].Kind, "role_fallback")
	}
}

// TestSubstitutionLedger_StatusDictView verifies that ToStatusDictView surfaces
// substitutions in both summary and full views.
func TestSubstitutionLedger_StatusDictView(t *testing.T) {
	g := &TaskGraph{
		TaskID:    "test_task",
		Status:    "running",
		Steps:     map[string]*Step{},
		CreatedAt: time.Now(),
	}
	g.RecordSubstitution("role_fallback", "a", "b", "c", "d")

	// Summary view: should have substitution_count
	summary := g.ToStatusDictView("summary")
	count, ok := summary["substitution_count"]
	if !ok {
		t.Fatal("summary view missing 'substitution_count' field")
	}
	if count.(int) != 1 {
		t.Errorf("substitution_count = %v, want 1", count)
	}

	// Full view: should have substitutions list
	full := g.ToStatusDictView("full")
	subs, ok := full["substitutions"]
	if !ok {
		t.Fatal("full view missing 'substitutions' field")
	}
	subsList, ok := subs.([]Substitution)
	if !ok {
		t.Fatalf("substitutions has wrong type: %T", subs)
	}
	if len(subsList) != 1 {
		t.Errorf("len(substitutions) = %d, want 1", len(subsList))
	}
	if subsList[0].Kind != "role_fallback" {
		t.Errorf("subs[0].Kind = %q, want %q", subsList[0].Kind, "role_fallback")
	}
}
