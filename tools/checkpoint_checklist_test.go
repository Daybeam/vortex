package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckpointChecklist_Tools is a structural invariant test that prevents
// ownership-check checkpoints from being silently dropped during refactors.
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P5.
func TestCheckpointChecklist_Tools(t *testing.T) {
	toolsDir := "."

	type checkpoint struct {
		name     string
		file     string
		pattern  string
		minCount int
	}

	checklist := []checkpoint{
		{
			name:     "DenyIfNotOwner called in tools_task.go (≥4 call sites)",
			file:     "tools_task.go",
			pattern:  "DenyIfNotOwner(",
			minCount: 4,
		},
	}

	for _, cp := range checklist {
		t.Run(cp.name, func(t *testing.T) {
			path := filepath.Join(toolsDir, cp.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}
			count := strings.Count(string(data), cp.pattern)
			if count < cp.minCount {
				t.Errorf("checkpoint MISSING: expected ≥%d occurrence(s) of %q in %s, got %d.\n"+
					"Ownership checks prevent cross-owner task manipulation. "+
					"If you intentionally removed a call, update this checklist with a comment.",
					cp.minCount, cp.pattern, cp.file, count)
			}
		})
	}
}
