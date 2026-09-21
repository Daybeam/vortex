package filters

import "testing"

// Compile-time interface satisfaction checks.
var (
	_ ToolFilter = (*BloomToolFilter)(nil)
	_ ToolFilter = (*CuckooToolFilter)(nil)
)

func TestBloomToolFilter_AddAndTest(t *testing.T) {
	f := NewBloomToolFilter(100)
	f.Add("orchestrator_run_command")
	f.Add("orchestrator_invoke")

	if !f.Test("orchestrator_run_command") {
		t.Error("expected Test to return true for an added item")
	}
	if !f.Test("orchestrator_invoke") {
		t.Error("expected Test to return true for an added item")
	}
}

func TestBloomToolFilter_TestFalseForUnadded(t *testing.T) {
	f := NewBloomToolFilter(1000)
	f.Add("orchestrator_run_command")

	if f.Test("definitely_not_a_registered_tool_name_xyz123") {
		t.Error("unexpected true for an item that was never added (possible but very unlikely false positive at this capacity)")
	}
}

func TestBloomToolFilter_DeleteIsNoOp(t *testing.T) {
	f := NewBloomToolFilter(100)
	f.Add("orchestrator_submit_task")

	if f.Delete("orchestrator_submit_task") {
		t.Error("expected Delete to always return false for BloomToolFilter")
	}
	if !f.Test("orchestrator_submit_task") {
		t.Error("expected item to still test true after a no-op Delete")
	}
}

func TestCuckooToolFilter_AddTestDelete(t *testing.T) {
	f := NewCuckooToolFilter(100)
	f.Add("orchestrator_wait_task")

	if !f.Test("orchestrator_wait_task") {
		t.Error("expected Test to return true for an added item")
	}
	if f.Test("definitely_not_a_registered_tool_name_xyz123") {
		t.Error("expected Test to return false for an item that was never added")
	}

	if !f.Delete("orchestrator_wait_task") {
		t.Error("expected Delete to return true for an item that was present")
	}
	if f.Test("orchestrator_wait_task") {
		t.Error("expected Test to return false after Delete")
	}
}

func TestCuckooToolFilter_DeleteAbsentItemReturnsFalse(t *testing.T) {
	f := NewCuckooToolFilter(100)
	if f.Delete("never_added") {
		t.Error("expected Delete to return false for an item that was never added")
	}
}

func TestNewCuckooToolFilter_EnforcesMinimumCapacity(t *testing.T) {
	f := NewCuckooToolFilter(0)
	f.Add("small_capacity_probe")
	if !f.Test("small_capacity_probe") {
		t.Error("expected filter constructed with a below-minimum capacity request to still work correctly")
	}
}
