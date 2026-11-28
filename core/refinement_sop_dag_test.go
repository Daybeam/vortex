package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestSOPLoadPath_RenderReviewRefine validates the render_review_refine
// SOP: unmarshal + DAG validation + structural assertions.
// Pattern follows sop_load_path_test.go.
func TestSOPLoadPath_RenderReviewRefine(t *testing.T) {
	path := filepath.Join("..", "workspace", "sops", "render_review_refine.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read SOP file: %v", err)
	}

	var sop schemas.SOP
	if err := json.Unmarshal(data, &sop); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := validateSOPDAG(sop.Steps); err != nil {
		t.Fatalf("validateSOPDAG: %v", err)
	}

	// Structural assertions.
	if sop.ID != "render_review_refine" {
		t.Errorf("ID = %q, want render_review_refine", sop.ID)
	}
	if sop.Version != "1.0" {
		t.Errorf("Version = %q, want 1.0", sop.Version)
	}
	if sop.MutableByAgent != false {
		t.Error("MutableByAgent should be false")
	}

	// 3 steps: render_artifact, rubric_review, apply_gate_and_refine.
	expectedSteps := []string{"render_artifact", "rubric_review", "apply_gate_and_refine"}
	if len(sop.Steps) != len(expectedSteps) {
		t.Fatalf("steps = %d, want %d", len(sop.Steps), len(expectedSteps))
	}
	for _, id := range expectedSteps {
		if _, ok := sop.Steps[id]; !ok {
			t.Errorf("missing step %q", id)
		}
	}

	// DAG: render_artifact (no deps) -> rubric_review (deps: render_artifact) -> apply_gate_and_refine (deps: rubric_review).
	if len(sop.Steps["render_artifact"].DependsOn) != 0 {
		t.Errorf("render_artifact deps = %v, want empty", sop.Steps["render_artifact"].DependsOn)
	}
	if len(sop.Steps["rubric_review"].DependsOn) != 1 || sop.Steps["rubric_review"].DependsOn[0] != "render_artifact" {
		t.Errorf("rubric_review deps = %v, want [render_artifact]", sop.Steps["rubric_review"].DependsOn)
	}
	if len(sop.Steps["apply_gate_and_refine"].DependsOn) != 1 || sop.Steps["apply_gate_and_refine"].DependsOn[0] != "rubric_review" {
		t.Errorf("apply_gate_and_refine deps = %v, want [rubric_review]", sop.Steps["apply_gate_and_refine"].DependsOn)
	}

	// Roles.
	if sop.Steps["render_artifact"].Role != "report_writer" {
		t.Errorf("render_artifact role = %q, want report_writer", sop.Steps["render_artifact"].Role)
	}
	if sop.Steps["rubric_review"].Role != "vision_analyst" {
		t.Errorf("rubric_review role = %q, want vision_analyst", sop.Steps["rubric_review"].Role)
	}
	if sop.Steps["apply_gate_and_refine"].Role != "report_writer" {
		t.Errorf("apply_gate_and_refine role = %q, want report_writer", sop.Steps["apply_gate_and_refine"].Role)
	}
}
