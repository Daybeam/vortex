package chat

import (
	"testing"
)

// Extracted from core/step_plan_enhancements_test.go during the pkg/chat
// extraction. Needs ChatHarness white-box access (SteerLock, PendingSteer).

func TestChatHarness_PendingSteer_DefaultEmpty(t *testing.T) {
	h := &ChatHarness{}
	h.SteerLock.Lock()
	if h.PendingSteer != "" {
		t.Error("PendingSteer should default to empty")
	}
	h.SteerLock.Unlock()
}
