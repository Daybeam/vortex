package chat

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
)

// Extracted from core/context_archive_segmentation_test.go during the
// pkg/chat extraction. Needs ChatHarness white-box access.

func TestChatHarness_SegmentManager_AddItemCalled(t *testing.T) {
	tmpDir := t.TempDir()
	sm := core.NewSegmentManager(filepath.Join(tmpDir, "segments.jsonl"))

	// Create a chat harness with SegmentManager set.
	harness := &ChatHarness{
		Segments: sm,
	}

	if harness.Segments == nil {
		t.Fatal("SegmentManager should be set")
	}

	// Verify AddItem works through the harness.
	seg := harness.Segments.AddItem(core.MemoryItem{
		NodeID:    "test-item",
		Intent:    "test intent",
		Summary:   "test summary",
		Timestamp: time.Now(),
	}, "test-scope")

	if seg == nil {
		t.Error("AddItem should return a non-nil segment")
	}
}
