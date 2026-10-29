package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckpointChecklist is a structural invariant test that prevents
// checkpoint mechanisms from being silently dropped during refactors.
//
// History: FallbackFor and SelectSkill were once silently removed during a
// rewrite and their absence was not caught until much later. This test reads
// the source files at test time and asserts each checkpoint still exists at
// its expected call site.
//
// When adding a new checkpoint, add it to the checklist below AND verify the
// test passes. When removing a checkpoint, remove it from the checklist with
// a comment explaining why.
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P5.
func TestCheckpointChecklist(t *testing.T) {
	// Resolve the core/ directory from this test file's location.
	coreDir := filepath.Dir(".")
	// When running via `go test`, the CWD is the package directory.
	if _, err := os.Stat(filepath.Join(coreDir, "scheduler_decision_failure.go")); err != nil {
		// Fallback: use the file path relative to the test binary.
		coreDir = "."
	}

	type checkpoint struct {
		name     string
		file     string
		pattern  string // substring that must appear in the file
		minCount int    // minimum number of occurrences
	}

	checklist := []checkpoint{
		{
			name:     "FallbackFor called in scheduler_decision_failure.go",
			file:     "scheduler_decision_failure.go",
			pattern:  "graph.FallbackFor(",
			minCount: 1,
		},
		{
			name:     "FallbackFor called in scheduler_decision_gate.go",
			file:     "scheduler_decision_gate.go",
			pattern:  "graph.FallbackFor(",
			minCount: 1,
		},
		{
			name:     "SelectSkill called in scheduler_decision_failure.go",
			file:     "scheduler_decision_failure.go",
			pattern:  "s.expStore.SelectSkill(",
			minCount: 1,
		},
		{
			name:     "EnableAutoRepair gated in builtin_interceptors.go",
			file:     "builtin_interceptors.go",
			pattern:  "registry.System.EnableAutoRepair",
			minCount: 1,
		},
		{
			name:     "budget_exhausted returned in budget_gate.go",
			file:     "budget_gate.go",
			pattern:  "budget_exhausted",
			minCount: 1,
		},
	}

	for _, cp := range checklist {
		t.Run(cp.name, func(t *testing.T) {
			path := filepath.Join(coreDir, cp.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}
			count := strings.Count(string(data), cp.pattern)
			if count < cp.minCount {
				t.Errorf("checkpoint MISSING: expected ≥%d occurrence(s) of %q in %s, got %d.\n"+
					"This checkpoint was previously silently dropped during a refactor. "+
					"If you intentionally removed it, update the checklist in checkpoint_checklist_test.go "+
					"with a comment explaining why.",
					cp.minCount, cp.pattern, cp.file, count)
			}
		})
	}
}
