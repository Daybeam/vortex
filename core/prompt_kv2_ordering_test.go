package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// KV-2 (2026-10-27): volatile blocks (anti-pattern, similar-task,
// active-context) must appear AFTER the constraints block (the cache marker).
// These blocks query the experience store inside Build, and
// UpsertDecisionPrecedent writes during the turn loop
// (spawner_dospawn.go:652) → they can change mid-step. Placing them after
// the stable prefix keeps the prefix byte-identical across turns → prefix
// cache hits.
//
// This test verifies the structural invariant: no volatile-category block
// appears before the constraints-category block in the output of Build.

func TestBuild_VolatileBlocksAfterConstraints(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	role := &config.Role{
		ID:             "executor",
		BaseCapability: "do things",
		Instruction:    "You are an executor.",
	}

	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-kv2",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Find the constraints block index.
	constraintsIdx := -1
	for i, b := range blocks {
		if b.Category == CategoryConstraints {
			constraintsIdx = i
			break
		}
	}
	if constraintsIdx == -1 {
		t.Fatal("constraints block not found — every prompt must have one")
	}

	// Verify no volatile-category block appears before the constraints block.
	volatileCategories := map[string]bool{
		CategoryAntiPattern:   true,
		CategorySimilarTask:   true,
		CategoryActiveContext: true,
	}
	for i, b := range blocks {
		if volatileCategories[b.Category] && i < constraintsIdx {
			t.Fatalf("volatile block %q at index %d appears before constraints block at index %d — "+
				"this breaks prefix cache stability (KV-2)", b.Category, i, constraintsIdx)
		}
	}
}

// Verify the constraints block carries the cache marker (CacheControl: "ephemeral").
// This is "Marker 1: End of System Prompt" — the cache breakpoint.
func TestBuild_ConstraintsBlockHasCacheMarker(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	role := &config.Role{
		ID:             "executor",
		BaseCapability: "do things",
		Instruction:    "You are an executor.",
	}

	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-kv2-marker",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, b := range blocks {
		if b.Category == CategoryConstraints {
			if b.CacheControl != "ephemeral" {
				t.Fatalf("constraints block must have CacheControl=%q, got %q "+
					"(Marker 1: End of System Prompt)", "ephemeral", b.CacheControl)
			}
			return
		}
	}
	t.Fatal("constraints block not found")
}

// Verify that archive blocks also come after constraints (KV-2 2026-10-25).
func TestBuild_ArchiveBlocksAfterConstraints(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	role := &config.Role{
		ID:             "executor",
		BaseCapability: "do things",
		Instruction:    "You are an executor.",
	}

	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-kv2-archive",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	constraintsIdx := -1
	for i, b := range blocks {
		if b.Category == CategoryConstraints {
			constraintsIdx = i
			break
		}
	}
	if constraintsIdx == -1 {
		t.Fatal("constraints block not found")
	}

	for i, b := range blocks {
		if b.Category == CategoryArchiveSummary && i < constraintsIdx {
			t.Fatalf("archive block at index %d appears before constraints block at index %d — "+
				"archive must be after the stable prefix (KV-2 2026-10-25)", i, constraintsIdx)
		}
	}
}
