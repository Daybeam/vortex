package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestSieveCompressContext_OnlyRTKRemovesTimestamps verifies the RTK-style
// behavior of applyRTK on a string containing timestamp patterns. It exercises
// the core ContextManager squeeze path (not available inside core/sieve).
func TestSieveCompressContext_OnlyRTKRemovesTimestamps(t *testing.T) {
	input := "Processing...[2026-08-27 12:00:00] Progress: 50%[2026-08-27 12:00:01] Updating..."
	cm := NewContextManager(newAntiCliffRegistry(10000, 5000, 10000))
	p := &MockProvider{name: "anti"}
	req := &schemas.CompleteRequest{
		UserBlocks: []schemas.ContentBlock{{Text: input}},
	}
	squeezed, _, err := cm.Squeeze(context.Background(), p, req, LevelLight)
	if err != nil {
		t.Fatalf("Squeeze failed: %v", err)
	}
	// Timestamps should be stripped
	if strings.Contains(squeezed.User, "[2026-08-27 12:00:00]") {
		t.Error("expected timestamp stripped by RTK but still present")
	}
}
