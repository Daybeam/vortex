package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestStepInput_OwnerIDFieldExists is the regression test for OAUTH §8.1
// (2026-10-27): StepInput must carry an OwnerID field so the caller's
// resolved identity can propagate through the submission chain to
// TaskGraph.OwnerID and then to SpawnRequest.OwnerID for internal
// sub-spawns (decider, planner, rubric).
//
// Without this field, internal sub-dispatch (decider/planner/rubric) has
// no way to inherit the owning graph's OwnerID, so per-tenant credential
// lookup silently degrades to empty-tenant — either fetching no credentials
// or fetching another tenant's.
//
// This test is a compile-time + runtime guarantee: if the OwnerID field is
// removed from StepInput, this file will not compile.
func TestStepInput_OwnerIDFieldExists(t *testing.T) {
	si := schemas.StepInput{
		ID:      "s1",
		RoleID:  "agent",
		Task:    "do work",
		OwnerID: "tenant-A",
	}
	if si.OwnerID != "tenant-A" {
		t.Fatalf("StepInput.OwnerID not set correctly: got %q", si.OwnerID)
	}
}

// TestRequestPlanning_AcceptsOwnerID verifies that RequestPlanning's
// signature includes the ownerID parameter and that it does not panic
// when called with a non-empty ownerID. This is the planner sub-spawn
// path — if the parameter is removed, this test will not compile.
func TestRequestPlanning_AcceptsOwnerID(t *testing.T) {
	s, _ := newPlannerTestEngine(t)

	// Flag off → early return, but the ownerID parameter must still be
	// accepted by the signature. If someone removes the parameter, this
	// call won't compile.
	steps, reason := s.RequestPlanning("task-x", "agent", "do thing", 0, true, "tenant-A")
	if steps != nil {
		t.Fatalf("flag off must not produce a plan")
	}
	if reason == "" {
		t.Fatalf("flag off must report a reason")
	}
}

// TestDispatchGroup_AcceptsOwnerID verifies that DispatchGroup's signature
// includes the ownerID parameter. This is the group-dispatch path — if the
// parameter is removed, this test will not compile.
func TestDispatchGroup_AcceptsOwnerID(t *testing.T) {
	reg := &config.Registry{
		RoleGroups: map[string]*config.RoleGroup{
			"test-group": {
				ID:      "test-group",
				Members: []config.GroupMember{{RoleID: "agent"}},
				Policy:  config.GroupPolicySequential,
			},
		},
		Roles: map[string]*config.Role{
			"agent": {ID: "agent", Name: "Agent"},
		},
	}
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })
	engine := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	dispatcher := NewGroupDispatcher(engine, reg)

	// DispatchGroup will fail because the engine has no spawner, but the
	// ownerID parameter must still be accepted by the signature. If someone
	// removes the parameter, this call won't compile.
	_, err := dispatcher.DispatchGroup("test-group", "test task", nil, nil, nil, nil, "", false, "tenant-A")
	// We expect an error (no spawner), but the call itself must compile
	// and not panic.
	_ = err
}

// TestSpawnDecider_AcceptsOwnerID verifies that spawnDecider's signature
// includes the ownerID parameter. This is the decider sub-spawn path —
// if the parameter is removed, this test will not compile.
func TestSpawnDecider_AcceptsOwnerID(t *testing.T) {
	s, _ := newPlannerTestEngine(t)

	// spawnDecider will return ("", false) because there's no spawner,
	// but the ownerID parameter must still be accepted by the signature.
	choice, ok := s.spawnDecider("decider", "task-x", "s1", schemas.DecisionStepFailed,
		map[string]any{"reason": "test"}, []string{"retry", "abort"}, false, "tenant-A")
	if ok && choice != "" {
		t.Fatalf("expected no choice from nil spawner, got %q", choice)
	}
}

// TestTryPlannerReplan_AcceptsOwnerID verifies that tryPlannerReplan's
// signature includes the ownerID parameter. This is the replan sub-spawn
// path — if the parameter is removed, this test will not compile.
func TestTryPlannerReplan_AcceptsOwnerID(t *testing.T) {
	s, _ := newPlannerTestEngine(t)

	// tryPlannerReplan will return "" because there's no spawner, but the
	// ownerID parameter must still be accepted by the signature.
	result := s.tryPlannerReplan("task-x", "s1", "test reason", false, "tenant-A")
	if result != "" {
		t.Fatalf("expected empty result from nil spawner, got %q", result)
	}
}
