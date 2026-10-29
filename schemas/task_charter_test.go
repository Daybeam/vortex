package schemas

import (
	"strings"
	"testing"
	"time"
)

// TestTaskCharter_PromptFormat verifies that CharterPrompt formats the charter
// correctly for system prompt injection.
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P1.
func TestTaskCharter_PromptFormat(t *testing.T) {
	// Nil charter → empty string (backward compatible)
	var nilCharter *TaskCharter
	if got := nilCharter.CharterPrompt(800); got != "" {
		t.Errorf("nil charter should produce empty string, got %q", got)
	}

	// Empty charter → just the header
	empty := &TaskCharter{}
	got := empty.CharterPrompt(800)
	if !strings.Contains(got, "Task Charter") {
		t.Errorf("empty charter should contain header, got %q", got)
	}

	// Full charter
	c := &TaskCharter{
		Intent:      "Verify the auth module handles expired tokens correctly.",
		NonGoals:    []string{"Do not rewrite the auth middleware.", "Do not add new endpoints."},
		Constraints: []string{"All existing tests must pass.", "No new dependencies."},
		Version:     1,
	}
	got = c.CharterPrompt(800)
	if !strings.Contains(got, "Verify the auth module") {
		t.Errorf("charter prompt missing intent: %q", got)
	}
	if !strings.Contains(got, "Non-Goals") {
		t.Errorf("charter prompt missing non-goals header: %q", got)
	}
	if !strings.Contains(got, "Do not rewrite the auth middleware") {
		t.Errorf("charter prompt missing non-goal item: %q", got)
	}
	if !strings.Contains(got, "Constraints") {
		t.Errorf("charter prompt missing constraints header: %q", got)
	}
	if !strings.Contains(got, "All existing tests must pass") {
		t.Errorf("charter prompt missing constraint item: %q", got)
	}
}

// TestTaskCharter_Truncation verifies that CharterPrompt truncates at maxChars.
func TestTaskCharter_Truncation(t *testing.T) {
	c := &TaskCharter{
		Intent: strings.Repeat("x", 100),
	}
	got := c.CharterPrompt(50)
	if len(got) > 200 { // 50 + truncation marker
		t.Errorf("charter prompt should be truncated, got length %d", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncated charter should contain truncation marker: %q", got)
	}
}

// TestTaskCharter_StatusDictView verifies that the charter is surfaced in
// task status responses.
func TestTaskCharter_StatusDictView(t *testing.T) {
	g := &TaskGraph{
		TaskID:    "test_task",
		Status:    "running",
		Steps:     map[string]*Step{},
		CreatedAt: parseTime("2026-10-29T00:00:00Z"),
		Charter: &TaskCharter{
			Intent:  "Test intent",
			Version: 1,
		},
	}

	// Summary: should have has_charter = true
	summary := g.ToStatusDictView("summary")
	hasCharter, ok := summary["has_charter"]
	if !ok {
		t.Fatal("summary view missing 'has_charter' field")
	}
	if !hasCharter.(bool) {
		t.Error("has_charter should be true when charter is set")
	}

	// Full: should have the charter object
	full := g.ToStatusDictView("full")
	charter, ok := full["charter"]
	if !ok {
		t.Fatal("full view missing 'charter' field")
	}
	charterObj, ok := charter.(*TaskCharter)
	if !ok {
		t.Fatalf("charter has wrong type: %T", charter)
	}
	if charterObj.Intent != "Test intent" {
		t.Errorf("charter.Intent = %q, want %q", charterObj.Intent, "Test intent")
	}
	if charterObj.Version != 1 {
		t.Errorf("charter.Version = %d, want 1", charterObj.Version)
	}

	// No charter: should have has_charter = false
	g2 := &TaskGraph{
		TaskID:    "test_task_2",
		Status:    "running",
		Steps:     map[string]*Step{},
		CreatedAt: parseTime("2026-10-29T00:00:00Z"),
	}
	summary2 := g2.ToStatusDictView("summary")
	hasCharter2 := summary2["has_charter"].(bool)
	if hasCharter2 {
		t.Error("has_charter should be false when no charter is set")
	}
}

func parseTime(s string) (t time.Time) {
	t, _ = time.Parse(time.RFC3339, s)
	return
}
