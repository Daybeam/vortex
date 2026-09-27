package core

import (
	"testing"
)

// TestPH8_StateChangingTools_PackageLevel is a regression test for audit P-H8:
// stateChangingTools must be a package-level var to avoid per-call map
// allocation. Before the fix, the map was allocated inside the function on
// every call, causing unnecessary GC pressure.
//
// Reproduction: verify the function returns correct results and the map
// is the same instance across calls (package-level, not per-call).
func TestPH8_StateChangingTools_PackageLevel(t *testing.T) {
	// Verify correct results for known state-changing tools.
	stateChanging := []string{"click", "type", "navigate", "press_key", "scroll", "set_cookie", "execute_script"}
	for _, name := range stateChanging {
		if !isStateChangingTool(name) {
			t.Errorf("expected %q to be state-changing", name)
		}
	}

	// Verify non-state-changing tools return false.
	nonStateChanging := []string{"screenshot", "get_text", "evaluate", "wait", "", "unknown"}
	for _, name := range nonStateChanging {
		if isStateChangingTool(name) {
			t.Errorf("expected %q to NOT be state-changing", name)
		}
	}

	// Verify the package-level map is non-nil and has the expected size.
	if stateChangingTools == nil {
		t.Fatal("stateChangingTools must be a non-nil package-level var")
	}
	if len(stateChangingTools) != 7 {
		t.Errorf("expected 7 state-changing tools, got %d", len(stateChangingTools))
	}
}
