package store

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestPruneStaleJITRefs(t *testing.T) {
	reg := &config.Registry{
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}

	s := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}
	s.TaskPatterns["pat_alive"] = TaskPattern{
		ID: "pat_alive",
		StepSequence: []map[string]any{
			{"tools": []any{"jit_alive123", "read_file"}},
		},
	}
	s.TaskPatterns["pat_dead"] = TaskPattern{
		ID: "pat_dead",
		StepSequence: []map[string]any{
			{"tools": []any{"jit_expired"}},
		},
	}
	s.TaskPatterns["pat_no_jit"] = TaskPattern{
		ID: "pat_no_jit",
		StepSequence: []map[string]any{
			{"tools": []any{"read_file", "write_file"}},
		},
	}

	reg.DynamicMCPs["jit_alive123"] = &config.MCPDef{}

	pruned := s.PruneStaleJITRefs(reg)
	if pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d", pruned)
	}
	if _, ok := s.TaskPatterns["pat_dead"]; ok {
		t.Fatal("pat_dead should have been pruned")
	}
	if _, ok := s.TaskPatterns["pat_alive"]; !ok {
		t.Fatal("pat_alive should NOT have been pruned")
	}
	if _, ok := s.TaskPatterns["pat_no_jit"]; !ok {
		t.Fatal("pat_no_jit should NOT have been pruned")
	}
}
