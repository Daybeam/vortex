package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// TestPromptAssembler_ShapeMismatchWarning_Wired verifies that
// CheckDataShapeMismatch is called inside Build() when
// SetRecentInteractions has been called with interactions that have
// inconsistent DataShape metadata. Regression test for the wiring
// (2026-10-25): CheckDataShapeMismatch had zero callers; it is now
// wired via SetRecentInteractions → Build → TagBlockWithCategory.
func TestPromptAssembler_ShapeMismatchWarning_Wired(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}

	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetRecentInteractions([]store.ToolInteraction{
		{
			ToolName: "get_futures",
			Meta:     &store.ToolMetadata{DataShape: "futures_dominant_daily"},
		},
		{
			ToolName: "get_spot",
			Meta:     &store.ToolMetadata{DataShape: "spot_tick_l1"},
		},
	})

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-shape-mismatch",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "[WARN]") && strings.Contains(b.Text, "inconsistent") {
			found = true
			if !strings.Contains(b.Text, "futures_dominant_daily") {
				t.Errorf("expected DataShape 'futures_dominant_daily' in warning, got: %s", b.Text)
			}
			if !strings.Contains(b.Text, "spot_tick_l1") {
				t.Errorf("expected DataShape 'spot_tick_l1' in warning, got: %s", b.Text)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected DataShape mismatch warning in prompt blocks, not found — CheckDataShapeMismatch is not wired into Build()")
	}
}

// TestPromptAssembler_ShapeMismatchWarning_NoMismatch verifies that
// when all interactions share the same DataShape, no warning is emitted.
func TestPromptAssembler_ShapeMismatchWarning_NoMismatch(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}

	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetRecentInteractions([]store.ToolInteraction{
		{
			ToolName: "get_futures",
			Meta:     &store.ToolMetadata{DataShape: "futures_dominant_daily"},
		},
		{
			ToolName: "get_futures_v2",
			Meta:     &store.ToolMetadata{DataShape: "futures_dominant_daily"},
		},
	})

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-no-mismatch",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, b := range blocks {
		if strings.Contains(b.Text, "[WARN]") && strings.Contains(b.Text, "inconsistent") {
			t.Fatal("unexpected DataShape mismatch warning when all shapes are identical")
		}
	}
}

// TestPromptAssembler_ShapeMismatchWarning_NilInteractions verifies
// that Build() does not panic and emits no warning when
// SetRecentInteractions is never called (nil/empty interactions).
func TestPromptAssembler_ShapeMismatchWarning_NilInteractions(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}

	pa := NewPromptAssembler(nil, nil, reg, nil)

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-nil-interactions",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, b := range blocks {
		if strings.Contains(b.Text, "[WARN]") && strings.Contains(b.Text, "inconsistent") {
			t.Fatal("unexpected DataShape mismatch warning when no interactions are set")
		}
	}
}
