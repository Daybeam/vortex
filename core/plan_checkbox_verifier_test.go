package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPlanCheckboxVerifier_AllChecked(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.md")
	plan := `# Plan

- [x] Read config file
- [x] Edit auth module
- [x] Run tests
`
	if err := os.WriteFile(planPath, []byte(plan), 0644); err != nil {
		t.Fatal(err)
	}

	v := &PlanCheckboxVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{"path": "plan.md"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected pass, got fail: %s", msg)
	}
}

func TestPlanCheckboxVerifier_SomeUnchecked(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.md")
	plan := `# Plan

- [x] Read config file
- [ ] Edit auth module
- [x] Run tests
`
	if err := os.WriteFile(planPath, []byte(plan), 0644); err != nil {
		t.Fatal(err)
	}

	v := &PlanCheckboxVerifier{}
	passed, msg, err := v.Verify(context.Background(), dir, map[string]any{"path": "plan.md"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Errorf("expected fail, got pass: %s", msg)
	}
	if msg == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestPlanCheckboxVerifier_NoCheckboxes(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.md")
	plan := `# Plan

1. Read config file
2. Edit auth module
3. Run tests
`
	if err := os.WriteFile(planPath, []byte(plan), 0644); err != nil {
		t.Fatal(err)
	}

	v := &PlanCheckboxVerifier{}
	passed, _, err := v.Verify(context.Background(), dir, map[string]any{"path": "plan.md"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected pass for plan with no checkboxes (numbered list format)")
	}
}

func TestPlanCheckboxVerifier_FileNotFound(t *testing.T) {
	v := &PlanCheckboxVerifier{}
	passed, _, err := v.Verify(context.Background(), "", map[string]any{"path": "/nonexistent/plan.md"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected fail for missing file")
	}
}

func TestPlanCheckboxVerifier_MissingPathParam(t *testing.T) {
	v := &PlanCheckboxVerifier{}
	passed, _, err := v.Verify(context.Background(), "", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if passed {
		t.Error("expected fail for missing path param")
	}
}

func TestPlanCheckboxVerifier_CheckedWithCapitalX(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.md")
	plan := `- [X] Step one
- [x] Step two
`
	if err := os.WriteFile(planPath, []byte(plan), 0644); err != nil {
		t.Fatal(err)
	}

	v := &PlanCheckboxVerifier{}
	passed, _, err := v.Verify(context.Background(), dir, map[string]any{"path": "plan.md"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected pass for [X] and [x] both accepted as checked")
	}
}
